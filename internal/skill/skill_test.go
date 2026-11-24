package skill_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"skilld/internal/skill"
)

func TestSlugifyAndRelativePaths(t *testing.T) {
	if got := skill.SlugifySkillID("Code Reviewer (v2)!"); got != "code-reviewer-v2" {
		t.Fatalf("SlugifySkillID = %q, want code-reviewer-v2", got)
	}
	if got := skill.SlugifySkillID("   "); got != "skill" {
		t.Fatalf("SlugifySkillID blank = %q, want skill", got)
	}

	valid := []string{"SKILL.md", "scripts/run.sh"}
	for _, p := range valid {
		if !skill.IsValidRelativePath(p) {
			t.Fatalf("expected valid path %q", p)
		}
	}
	invalid := []string{"../escape.txt", "sub/../escape.txt", "/abs/path.md", `win\path.md`, "dir/", "", "a\x00b"}
	for _, p := range invalid {
		if skill.IsValidRelativePath(p) {
			t.Fatalf("expected invalid path %q", p)
		}
	}
}

func TestComputeFilesHashDeterminism(t *testing.T) {
	mapA := map[string]skill.FileEntry{
		"scripts/run.sh": {Encoding: "utf8", Content: "echo hi\n", Mode: "0755"},
		"SKILL.md":       {Encoding: "utf8", Content: "# Test\n", Mode: "0644"},
	}
	mapB := map[string]skill.FileEntry{
		"SKILL.md":       {Encoding: "utf8", Content: "# Test\n", Mode: "0644"},
		"scripts/run.sh": {Encoding: "utf8", Content: "echo hi\n", Mode: "0755"},
	}
	mapMode := map[string]skill.FileEntry{
		"SKILL.md":       {Encoding: "utf8", Content: "# Test\n", Mode: "0644"},
		"scripts/run.sh": {Encoding: "utf8", Content: "echo hi\n", Mode: "0644"},
	}

	hA, err := skill.ComputeFilesHash(mapA)
	if err != nil {
		t.Fatal(err)
	}
	hB, err := skill.ComputeFilesHash(mapB)
	if err != nil {
		t.Fatal(err)
	}
	hMode, err := skill.ComputeFilesHash(mapMode)
	if err != nil {
		t.Fatal(err)
	}
	if hA != hB {
		t.Fatalf("hash mismatch across key order: %s != %s", hA, hB)
	}
	if hA == hMode {
		t.Fatalf("expected mode change to alter hash")
	}
	if !strings.HasPrefix(hA, "sha256:") || len(hA) != 7+64 {
		t.Fatalf("unexpected hash format: %s", hA)
	}
}

func TestFrontmatterDriftAndDiff(t *testing.T) {
	yamlMd := "---\nname: \"PDF Specialist\"\ndescription: Extract tables and text from PDFs\nversion: 1.2.3\nallowed-tools:\n  - Read\n  - Bash\n---\n# Ignored Heading\nBody text.\n"
	meta := skill.ParseSkillFrontmatter(yamlMd)
	if meta.Name != "PDF Specialist" || meta.Description != "Extract tables and text from PDFs" || meta.Version != "1.2.3" {
		t.Fatalf("unexpected meta: %+v", meta)
	}

	fallback := skill.ParseSkillFrontmatter("# Heading Fallback Skill\n\nBody")
	if fallback.Name != "Heading Fallback Skill" {
		t.Fatalf("fallback name = %q", fallback.Name)
	}

	if got := skill.ComputeInstallationStatus("h1", "h1", "h1", true); got != skill.InstallStatusSynced {
		t.Fatalf("status = %s, want synced", got)
	}
	if got := skill.ComputeInstallationStatus("h1", "h1", "h2", true); got != skill.InstallStatusOutdated {
		t.Fatalf("status = %s, want outdated", got)
	}
	if got := skill.ComputeInstallationStatus("h1", "h3", "h2", true); got != skill.InstallStatusModified {
		t.Fatalf("status = %s, want modified", got)
	}
	if got := skill.ComputeInstallationStatus("", "h1", "", false); got != skill.InstallStatusUnmanaged {
		t.Fatalf("status = %s, want unmanaged", got)
	}
	if got := skill.ComputeInstallationStatus("h1", "", "h1", true); got != skill.InstallStatusMissing {
		t.Fatalf("status = %s, want missing", got)
	}

	diff, err := skill.DiffFileMaps(
		map[string]skill.FileEntry{
			"SKILL.md": {Encoding: "utf8", Content: "line 1\nline 2\nline 3\n", Mode: "0644"},
			"old.txt":  {Encoding: "utf8", Content: "deprecated\n", Mode: "0644"},
		},
		map[string]skill.FileEntry{
			"SKILL.md": {Encoding: "utf8", Content: "line 1\nline 2 edited\nline 3\nline 4\n", Mode: "0644"},
			"new.sh":   {Encoding: "utf8", Content: "#!/bin/sh\necho ok\n", Mode: "0755"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !diff.HasChanges || diff.ChangedCount != 3 {
		t.Fatalf("unexpected diff summary: %+v", diff)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestParseGitHubURLAndFetch(t *testing.T) {
	parsed, err := skill.ParseGitHubURL("https://github.com/anthropics/skills/tree/main/skills/pdf", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Owner != "anthropics" || parsed.Repo != "skills" || parsed.Ref != "main" || parsed.Subpath != "skills/pdf" {
		t.Fatalf("unexpected parsed url: %+v", parsed)
	}

	if _, err := skill.ParseGitHubURL("https://gitlab.com/a/b", "", ""); err == nil {
		t.Fatalf("expected non-github url rejection")
	}

	mdB64 := base64.StdEncoding.EncodeToString([]byte("---\nname: GitHub PDF Skill\n---\n# PDF\n"))
	shB64 := base64.StdEncoding.EncodeToString([]byte("#!/usr/bin/env bash\necho ok\n"))

	client := &skill.GitHubClient{
		BaseURL: "https://mock.github.local",
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				u := r.URL.String()
				var body string
				status := 200
				switch {
				case strings.Contains(u, "/commits?"):
					body = `[{"sha":"1111111111111111111111111111111111111111","commit":{"message":"Initial skill","committer":{"date":"2026-10-01T00:00:00Z"}}}]`
				case strings.Contains(u, "/git/trees/1111"):
					body = `{"tree":[{"path":"skills/pdf/SKILL.md","type":"blob","mode":"100644","sha":"b1","size":40},{"path":"skills/pdf/scripts/run.sh","type":"blob","mode":"100755","sha":"b2","size":28}]}`
				case strings.HasSuffix(u, "/git/blobs/b1"):
					body = `{"encoding":"base64","content":"` + mdB64 + `"}`
				case strings.HasSuffix(u, "/git/blobs/b2"):
					body = `{"encoding":"base64","content":"` + shB64 + `"}`
				default:
					status = 404
					body = `{"message":"Not Found"}`
				}
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}),
		},
	}

	fetched, err := client.FetchSkillFiles(context.Background(), parsed.Owner, parsed.Repo, parsed.Ref, parsed.Subpath, "")
	if err != nil {
		t.Fatalf("FetchSkillFiles failed: %v", err)
	}
	if fetched.Meta.Name != "GitHub PDF Skill" {
		t.Fatalf("meta.Name = %q", fetched.Meta.Name)
	}
	if fetched.Files["scripts/run.sh"].Mode != "0755" {
		t.Fatalf("script mode = %q, want 0755", fetched.Files["scripts/run.sh"].Mode)
	}
}
