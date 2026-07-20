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
	"os"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/models"

	"github.com/gocarina/gocsv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustParseLinearTime(t *testing.T, s string) linearTime {
	t.Helper()
	var lt linearTime
	require.NoError(t, lt.UnmarshalCSV(s))
	return lt
}

func TestLinearTimeUnmarshalCSV(t *testing.T) {
	t.Run("with zone name in parens", func(t *testing.T) {
		lt := mustParseLinearTime(t, "Tue Jun 30 2026 00:43:35 GMT+0000 (GMT+00:00)")
		assert.Equal(t, 2026, lt.Time.Year())
		assert.Equal(t, time.June, lt.Time.Month())
		assert.Equal(t, 30, lt.Time.Day())
		assert.Equal(t, 0, lt.Time.Hour())
		assert.Equal(t, 43, lt.Time.Minute())
	})

	t.Run("empty value", func(t *testing.T) {
		lt := mustParseLinearTime(t, "")
		assert.True(t, lt.Time.IsZero())
	})

	t.Run("non-UTC offset", func(t *testing.T) {
		lt := mustParseLinearTime(t, "Wed Jul 01 2026 09:00:00 GMT-0700 (Pacific Daylight Time)")
		assert.Equal(t, 9, lt.Time.Hour())
		_, offset := lt.Time.Zone()
		assert.Equal(t, -7*60*60, offset)
	})

	t.Run("invalid value returns an error", func(t *testing.T) {
		var lt linearTime
		assert.Error(t, lt.UnmarshalCSV("not a date"))
	})
}

func TestTeamKey(t *testing.T) {
	assert.Equal(t, "CAT", teamKey("CAT-5"))
	assert.Equal(t, "ENG", teamKey("ENG-123"))
	assert.Equal(t, "", teamKey("not-an-id"))
	assert.Equal(t, "", teamKey(""))
}

func TestIsValidCSV(t *testing.T) {
	valid := `"ID","Team","Title","Priority","Labels"
"CAT-1","CatlowTech","Test","Medium",""
`
	assert.True(t, isValidCSV(valid))
	assert.False(t, isValidCSV(`{"not": "csv"}`))
	assert.False(t, isValidCSV("Team,Title\nno priority or labels column"))
}

func TestConvertMarkdownToHTML(t *testing.T) {
	html, err := convertMarkdownToHTML("## Problem\n\nSomething is broken.")
	require.NoError(t, err)
	assert.Contains(t, html, "<h2>Problem</h2>")
	assert.Contains(t, html, "<p>Something is broken.</p>")
}

func TestConvertLinearToVikunja(t *testing.T) {
	issues := []*linearIssue{
		{ID: "CAT-1", Team: "CatlowTech", Title: "Set up marketing site", Status: "Todo", Priority: "Medium", Project: "Website", Labels: "bug, ui"},
		{ID: "CAT-2", Team: "CatlowTech", Title: "Ship pricing page", Status: "Done", Priority: "High", Project: "Website", Completed: mustParseLinearTime(t, "Wed Jul 01 2026 02:39:30 GMT+0000 (GMT+00:00)")},
		{ID: "CAT-3", Team: "CatlowTech", Title: "Old rebrand idea", Status: "Canceled", Priority: "Urgent", Canceled: mustParseLinearTime(t, "Thu Jul 09 2026 01:31:46 GMT+0000 (GMT+00:00)")},
		{ID: "CAT-4", Team: "CatlowTech", Title: "No project, no priority", Status: "Backlog", Priority: "No priority"},
		{ID: "CAT-5", Team: "CatlowTech", Title: "Add checkout button", Status: "Todo", Priority: "Medium", Project: "Website", ParentIssue: "CAT-1", RelatedTo: "CAT-1, CAT-2", BlockedBy: "CAT-3"},
		{ID: "CAT-6", Team: "CatlowTech", Title: "Duplicate pricing task", Status: "Canceled", Priority: "Low", Project: "Website", DuplicateOf: "CAT-2"},
		{ID: "ENG-1", Team: "Engineering", Title: "Migrate CI to new runners", Status: "Todo", Priority: "High"},
	}

	result := convertLinearToVikunja(issues)

	// root + CatlowTech + Website (sub of CatlowTech) + Engineering
	require.Len(t, result, 4)

	var root, catlowTech, website, engineering *models.ProjectWithTasksAndBuckets
	for _, p := range result {
		switch p.Title {
		case "Migrated from Linear":
			root = p
		case "CatlowTech":
			catlowTech = p
		case "Website":
			website = p
		case "Engineering":
			engineering = p
		}
	}
	require.NotNil(t, root)
	require.NotNil(t, catlowTech)
	require.NotNil(t, website)
	require.NotNil(t, engineering)

	assert.Equal(t, int64(0), root.ParentProjectID)
	assert.Equal(t, root.ID, catlowTech.ParentProjectID)
	assert.Equal(t, root.ID, engineering.ParentProjectID)
	assert.Equal(t, catlowTech.ID, website.ParentProjectID)
	assert.Equal(t, "CAT", catlowTech.Identifier)
	assert.Equal(t, "ENG", engineering.Identifier)

	// CAT-3 and CAT-4 have no Project column value, so they live directly under the team project.
	require.Len(t, catlowTech.Tasks, 2)
	teamTasksByTitle := make(map[string]*models.TaskWithComments, len(catlowTech.Tasks))
	for _, task := range catlowTech.Tasks {
		teamTasksByTitle[task.Title] = task
	}
	require.Contains(t, teamTasksByTitle, "No project, no priority")
	assert.Equal(t, int64(0), teamTasksByTitle["No project, no priority"].Priority)

	require.Len(t, website.Tasks, 4)
	byTitle := make(map[string]*models.TaskWithComments, len(website.Tasks))
	for _, task := range website.Tasks {
		byTitle[task.Title] = task
	}

	setupTask := byTitle["Set up marketing site"]
	require.NotNil(t, setupTask)
	assert.Equal(t, int64(2), setupTask.Priority) // Medium
	assert.False(t, setupTask.Done)
	require.Len(t, setupTask.Labels, 2)
	assert.Equal(t, "bug", setupTask.Labels[0].Title)
	assert.Equal(t, "ui", setupTask.Labels[1].Title)

	shipTask := byTitle["Ship pricing page"]
	require.NotNil(t, shipTask)
	assert.Equal(t, int64(3), shipTask.Priority) // High
	assert.True(t, shipTask.Done)
	assert.False(t, shipTask.DoneAt.IsZero())

	// "Old rebrand idea" (CAT-3) has no Project column value either, so it's a team-level task, not under Website.
	canceledTask := teamTasksByTitle["Old rebrand idea"]
	require.NotNil(t, canceledTask)
	assert.True(t, canceledTask.Done)
	assert.False(t, canceledTask.DoneAt.IsZero())

	checkoutTask := byTitle["Add checkout button"]
	require.NotNil(t, checkoutTask)
	require.Contains(t, checkoutTask.RelatedTasks, models.RelationKindParenttask)
	assert.Equal(t, setupTask.ID, checkoutTask.RelatedTasks[models.RelationKindParenttask][0].ID)
	require.Contains(t, checkoutTask.RelatedTasks, models.RelationKindRelated)
	assert.Len(t, checkoutTask.RelatedTasks[models.RelationKindRelated], 2)
	require.Contains(t, checkoutTask.RelatedTasks, models.RelationKindBlocked)
	assert.Equal(t, canceledTask.ID, checkoutTask.RelatedTasks[models.RelationKindBlocked][0].ID)

	dupTask := byTitle["Duplicate pricing task"]
	require.NotNil(t, dupTask)
	require.Contains(t, dupTask.RelatedTasks, models.RelationKindDuplicateOf)
	assert.Equal(t, shipTask.ID, dupTask.RelatedTasks[models.RelationKindDuplicateOf][0].ID)

	require.Len(t, engineering.Tasks, 1)
	assert.Equal(t, "Migrate CI to new runners", engineering.Tasks[0].Title)
}

func TestParseExportFile(t *testing.T) {
	f, err := os.Open("testdata_linear_export.csv")
	require.NoError(t, err)
	defer f.Close()

	decoder, err := newDecoder(f)
	require.NoError(t, err)

	var issues []*linearIssue
	err = gocsv.UnmarshalDecoder(decoder, &issues)
	require.NoError(t, err)
	require.Len(t, issues, 7)

	result := convertLinearToVikunja(issues)
	require.Len(t, result, 4) // root, CatlowTech, Website, Engineering

	var website *models.ProjectWithTasksAndBuckets
	for _, p := range result {
		if p.Title == "Website" {
			website = p
		}
	}
	require.NotNil(t, website)

	var flakyCI *models.TaskWithComments
	for _, task := range result[1].Tasks { // CatlowTech-level task without a Project
		if task.Title == "Investigate flaky CI" {
			flakyCI = task
		}
	}
	require.NotNil(t, flakyCI)
	assert.Contains(t, flakyCI.Description, "<h2>Problem</h2>")
	assert.Contains(t, flakyCI.Description, "<li>happens ~1 in 20 runs</li>")
}

func TestStripBOM(t *testing.T) {
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, []byte("ID,Team\nCAT-1,CatlowTech\n")...)
	r := stripBOM(bytes.NewReader(withBOM))
	out := make([]byte, 64)
	n, _ := r.Read(out)
	assert.Equal(t, "ID,Team\nCAT-1,CatlowTech\n", string(out[:n]))
}
