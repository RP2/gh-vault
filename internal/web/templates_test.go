package web

import (
	"bytes"
	"html/template"
	"testing"
	"time"

	"github.com/RP2/gh-vault/internal/model"
)

func TestTemplatesRender(t *testing.T) {
	base, err := template.ParseFS(templateFS, "templates/layout.html")
	if err != nil {
		t.Fatalf("parse layout: %v", err)
	}

	cases := map[string]any{
		"dashboard.html": struct {
			RepoCount   int
			BackedUp    int
			NotBackedUp int
			RecentLogs  []model.LogEntry
			CSRFToken   string
			CurrentPath string
		}{RepoCount: 5, BackedUp: 3, NotBackedUp: 2, RecentLogs: []model.LogEntry{
			{ID: 1, RepoID: 1, Action: "backup", Status: "success", Message: "ok", CreatedAt: time.Now()},
		}, CSRFToken: "abc", CurrentPath: "/"},
		"repos.html": struct {
			CSRFToken    string
			Repos        []model.Repo
			DeletedRepos []model.Repo
			DeletedCount int
			CurrentPath  string
		}{CSRFToken: "abc", Repos: []model.Repo{
			{ID: 1, Owner: "RP2", Name: "gh-vault", Format: model.FormatClone, AutoArchive: true},
		}, DeletedRepos: []model.Repo{
			{ID: 2, Owner: "RP2", Name: "old-repo", Format: model.FormatClone, GitHubDeleted: true},
		}, DeletedCount: 1, CurrentPath: "/repos"},
		"logs.html": struct {
			Logs        []model.LogEntry
			Page        int
			Size        int
			PrevPage    int
			NextPage    int
			HasNext     bool
			CSRFToken   string
			CurrentPath string
		}{Logs: []model.LogEntry{
			{ID: 1, RepoID: 1, Action: "backup", Status: "success", Message: "ok", CreatedAt: time.Now()},
		}, Page: 1, Size: 50, PrevPage: 0, NextPage: 2, HasNext: true, CSRFToken: "abc", CurrentPath: "/logs"},
		"settings.html": struct {
			Settings    model.Settings
			Reason      string
			CSRFToken   string
			CurrentPath string
		}{Settings: model.Settings{CronSchedule: "0 0 * * *", DryRun: true, AutoArchiveDays: 30, LogRetentionDays: 90}, Reason: "token_missing", CSRFToken: "abc", CurrentPath: "/settings"},
		"setup.html": map[string]string{"Error": "bad", "CSRFToken": "abc", "CurrentPath": "/setup"},
		"login.html": map[string]string{"Error": "invalid", "CSRFToken": "abc", "CurrentPath": "/login"},
	}

	for name, data := range cases {
		tmpl, err := base.Clone()
		if err != nil {
			t.Errorf("clone for %s: %v", name, err)
			continue
		}
		if _, err := tmpl.ParseFS(templateFS, "templates/"+name); err != nil {
			t.Errorf("parse %s: %v", name, err)
			continue
		}
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
			t.Errorf("render %s: %v", name, err)
		}
	}
}
