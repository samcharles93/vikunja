// Vikunja is a to-do list application to facilitate your life.
// Copyright 2018-present Vikunja and contributors. All rights reserved.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package linear

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"code.vikunja.io/api/pkg/models"
	"code.vikunja.io/api/pkg/modules/migration"
	"code.vikunja.io/api/pkg/user"

	"github.com/gocarina/gocsv"
	"github.com/yuin/goldmark"
)

// Migrator implements the migration.FileMigrator interface to import issues
// exported from Linear (Settings > Import & Export > Export > Issue data >
// "Download" link sent by email).
type Migrator struct {
}

// priorityMap translates Linear's textual priority into Vikunja's numeric
// priority scale (0 unset, 1 low, 2 medium, 3 high, 4 urgent). Linear has no
// equivalent of Vikunja's "do now" (5), so the highest it maps to is urgent.
var priorityMap = map[string]int64{
	"no priority": 0,
	"low":         1,
	"medium":      2,
	"high":        3,
	"urgent":      4,
}

// doneStatuses are the Linear workflow states that map to a completed
// Vikunja task. Every other status (Backlog, Todo, In Progress, In Review,
// Triage, ...) is imported as not done.
var doneStatuses = map[string]bool{
	"done":      true,
	"canceled":  true,
	"cancelled": true,
}

// teamKeyPattern extracts the short team key ("CAT") from a Linear issue
// identifier ("CAT-5"), which becomes the identifier of the team's project.
var teamKeyPattern = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*)-\d+$`)

type linearIssue struct {
	ID          string     `csv:"ID"`
	Team        string     `csv:"Team"`
	Title       string     `csv:"Title"`
	Description string     `csv:"Description"`
	Status      string     `csv:"Status"`
	Priority    string     `csv:"Priority"`
	Project     string     `csv:"Project"`
	Labels      string     `csv:"Labels"`
	Completed   linearTime `csv:"Completed"`
	Canceled    linearTime `csv:"Canceled"`
	DueDate     linearTime `csv:"Due Date"`
	ParentIssue string     `csv:"Parent issue"`
	RelatedTo   string     `csv:"Related to"`
	BlockedBy   string     `csv:"Blocked by"`
	DuplicateOf string     `csv:"Duplicate of"`
}

// linearTime parses the JS `Date#toString()` format Linear uses for every
// timestamp column, e.g. "Tue Jun 30 2026 00:43:35 GMT+0000 (GMT+00:00)".
type linearTime struct {
	time.Time
}

// linearTimeFormats are tried in order. Linear's timestamp columns (Created,
// Completed, Canceled, ...) use the full JS Date#toString() format; Due Date
// has been observed as a plain ISO date in some exports.
var linearTimeFormats = []string{
	"Mon Jan 2 2006 15:04:05 GMT-0700",
	"2006-01-02",
	time.RFC3339,
}

func (lt *linearTime) UnmarshalCSV(csv string) (err error) {
	lt.Time = time.Time{}
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil
	}
	// Drop the trailing zone name in parentheses - Go can't parse it and the
	// numeric offset earlier in the string already carries the information.
	if idx := strings.Index(csv, " ("); idx != -1 {
		csv = csv[:idx]
	}
	for _, format := range linearTimeFormats {
		lt.Time, err = time.Parse(format, csv)
		if err == nil {
			return nil
		}
	}
	return err
}

// Name is used to get the name of the linear migration - we're using the docs here to annotate the status route.
// @Summary Get migration status
// @Description Returns if the current user already did the migation or not. This is useful to show a confirmation message in the frontend if the user is trying to do the same migration again.
// @tags migration
// @Produce json
// @Security JWTKeyAuth
// @Success 200 {object} migration.Status "The migration status"
// @Failure 500 {object} models.Message "Internal server error"
// @Router /migration/linear/status [get]
func (m *Migrator) Name() string {
	return "linear"
}

// Migrate takes a Linear issue export, parses it and imports everything in it into Vikunja.
// @Summary Import all teams, projects, tasks and labels from a Linear issue export
// @Description Imports teams and projects (as nested Vikunja projects), tasks, labels, priorities and relations from a Linear CSV issue export (Settings > Import & Export > Export > Issue data) into Vikunja.
// @tags migration
// @Accept x-www-form-urlencoded
// @Produce json
// @Security JWTKeyAuth
// @Param import formData string true "The Linear issue export csv file."
// @Success 200 {object} models.Message "A message telling you everything was migrated successfully."
// @Failure 500 {object} models.Message "Internal server error"
// @Router /migration/linear/migrate [put]
func (m *Migrator) Migrate(user *user.User, file io.ReaderAt, size int64) error {
	if size == 0 {
		return &migration.ErrFileIsEmpty{}
	}

	fr := io.NewSectionReader(file, 0, size)

	buf := make([]byte, 1024)
	n, err := fr.Read(buf)
	if errors.Is(err, io.EOF) || n == 0 {
		return &migration.ErrFileIsEmpty{}
	}
	if err != nil {
		return err
	}

	if !isValidCSV(string(buf[:n])) {
		return &migration.ErrNotACSVFile{}
	}

	_, err = fr.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}

	decoder, err := newDecoder(fr)
	if err != nil {
		return err
	}

	allIssues := []*linearIssue{}
	err = gocsv.UnmarshalDecoder(decoder, &allIssues)
	if err != nil {
		return err
	}

	if len(allIssues) == 0 {
		return &migration.ErrFileIsEmpty{}
	}

	for _, issue := range allIssues {
		issue.Title = strings.TrimSpace(issue.Title)
	}

	vikunjaProjects := convertLinearToVikunja(allIssues)

	return migration.InsertFromStructure(vikunjaProjects, user)
}

func convertLinearToVikunja(issues []*linearIssue) (result []*models.ProjectWithTasksAndBuckets) {
	const pseudoParentID int64 = 1

	root := &models.ProjectWithTasksAndBuckets{
		Project: models.Project{
			ID:    pseudoParentID,
			Title: "Migrated from Linear",
		},
	}
	result = []*models.ProjectWithTasksAndBuckets{root}

	nextProjectID := pseudoParentID + 1
	teamProjects := make(map[string]*models.ProjectWithTasksAndBuckets)
	subProjects := make(map[string]*models.ProjectWithTasksAndBuckets) // keyed by team + "\x00" + project name

	projectFor := func(issue *linearIssue) *models.ProjectWithTasksAndBuckets {
		team := issue.Team
		if team == "" {
			team = "No Team"
		}

		teamProject, ok := teamProjects[team]
		if !ok {
			teamProject = &models.ProjectWithTasksAndBuckets{
				Project: models.Project{
					ID:              nextProjectID,
					ParentProjectID: pseudoParentID,
					Title:           team,
					Identifier:      teamKey(issue.ID),
				},
			}
			nextProjectID++
			teamProjects[team] = teamProject
			result = append(result, teamProject)
		}

		if issue.Project == "" {
			return teamProject
		}

		key := team + "\x00" + issue.Project
		sub, ok := subProjects[key]
		if !ok {
			sub = &models.ProjectWithTasksAndBuckets{
				Project: models.Project{
					ID:              nextProjectID,
					ParentProjectID: teamProject.ID,
					Title:           issue.Project,
				},
			}
			nextProjectID++
			subProjects[key] = sub
			result = append(result, sub)
		}
		return sub
	}

	idByLinearID := make(map[string]int64, len(issues))
	for i, issue := range issues {
		idByLinearID[issue.ID] = int64(i + 1)
	}

	addRelation := func(relations map[models.RelationKind][]*models.Task, kind models.RelationKind, refs string) {
		for _, ref := range strings.Split(refs, ",") {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}
			if otherID, ok := idByLinearID[ref]; ok {
				relations[kind] = append(relations[kind], &models.Task{ID: otherID})
			}
		}
	}

	for i, issue := range issues {
		project := projectFor(issue)

		status := strings.ToLower(strings.TrimSpace(issue.Status))
		done := doneStatuses[status]

		var doneAt time.Time
		if done {
			if status == "canceled" || status == "cancelled" {
				doneAt = issue.Canceled.Time
			} else {
				doneAt = issue.Completed.Time
			}
		}

		description := issue.Description
		if description != "" {
			if html, err := convertMarkdownToHTML(description); err == nil {
				description = html
			}
		}

		var labels []*models.Label
		for _, name := range strings.Split(issue.Labels, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			labels = append(labels, &models.Label{Title: name})
		}

		task := &models.TaskWithComments{
			Task: models.Task{
				ID:          int64(i + 1),
				Title:       issue.Title,
				Description: description,
				DueDate:     issue.DueDate.Time,
				Done:        done,
				DoneAt:      doneAt,
				Priority:    priorityMap[strings.ToLower(strings.TrimSpace(issue.Priority))],
				Labels:      labels,
			},
		}

		relations := map[models.RelationKind][]*models.Task{}
		addRelation(relations, models.RelationKindParenttask, issue.ParentIssue)
		addRelation(relations, models.RelationKindRelated, issue.RelatedTo)
		addRelation(relations, models.RelationKindBlocked, issue.BlockedBy)
		addRelation(relations, models.RelationKindDuplicateOf, issue.DuplicateOf)
		if len(relations) > 0 {
			task.RelatedTasks = relations
		}

		project.Tasks = append(project.Tasks, task)
	}

	sort.Slice(result[1:], func(i, j int) bool {
		return result[1:][i].Title < result[1:][j].Title
	})

	return result
}

// teamKey extracts the short team key ("CAT") from a Linear issue
// identifier ("CAT-5"). It returns an empty string if issueID doesn't match
// Linear's "<KEY>-<number>" identifier format.
func teamKey(issueID string) string {
	m := teamKeyPattern.FindStringSubmatch(issueID)
	if m == nil {
		return ""
	}
	return m[1]
}

func convertMarkdownToHTML(input string) (output string, err error) {
	var buf bytes.Buffer
	err = goldmark.Convert([]byte(input), &buf)
	if err != nil {
		return
	}
	//#nosec - we are not responsible to escape this as we don't know the context where it is used
	return buf.String(), nil
}

// isValidCSV performs a basic check to determine if the content looks like a
// Linear issue export.
func isValidCSV(content string) bool {
	if !strings.Contains(content, "Team") ||
		!strings.Contains(content, "Title") ||
		!strings.Contains(content, "Priority") ||
		!strings.Contains(content, "Labels") {
		return false
	}

	hasCommas := strings.Contains(content, ",")
	hasNewlines := strings.Contains(content, "\n")

	return hasCommas && hasNewlines
}

// stripBOM removes the UTF-8 BOM from the beginning of a reader, if present.
func stripBOM(r io.Reader) io.Reader {
	buf := make([]byte, 3)
	n, err := r.Read(buf)
	if err != nil && err != io.EOF {
		if n > 0 {
			return io.MultiReader(bytes.NewReader(buf[:n]), r)
		}
		return r
	}

	if n == 3 && buf[0] == 0xEF && buf[1] == 0xBB && buf[2] == 0xBF {
		return r
	}

	return io.MultiReader(bytes.NewReader(buf[:n]), r)
}

func newDecoder(r io.Reader) (gocsv.SimpleDecoder, error) {
	reader := csv.NewReader(stripBOM(r))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true
	return gocsv.NewSimpleDecoderFromCSVReader(reader), nil
}
