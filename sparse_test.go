package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemGitSparseCloneMaterializesSelectedDirectories(t *testing.T) {
	remote := createSparseFixture(t)
	destination := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	var events []OperationEvent
	result, err := (systemEngine{binary: "git"}).Clone(context.Background(), Repository{URL: remote, Host: "local.test", Transport: "https"}, CloneRequest{
		URL: remote, Destination: destination, Depth: 1, SparsePaths: []string{"advisories/github-reviewed"},
		OnEvent: func(event OperationEvent) { events = append(events, event) },
	}, Credential{}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit == "" {
		t.Fatal("sparse clone returned an empty commit")
	}
	if _, err := os.Stat(filepath.Join(destination, "advisories", "github-reviewed", "GHSA-test.json")); err != nil {
		t.Fatalf("selected sparse file is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "docs", "unselected.txt")); !os.IsNotExist(err) {
		t.Fatalf("unselected file exists or could not be checked: %v", err)
	}
	if !hasProcessEvent(events, "clone") {
		t.Fatalf("clone process outcome was not reported: %#v", events)
	}
}

func TestSparsePathValidationRejectsUnsafeAndPortableCollisions(t *testing.T) {
	for _, test := range []struct {
		name  string
		paths []string
	}{
		{name: "parent traversal", paths: []string{"../secret"}},
		{name: "absolute", paths: []string{"/etc"}},
		{name: "backslash", paths: []string{"advisories\\private"}},
		{name: "git metadata", paths: []string{".git/objects"}},
		{name: "duplicate case-insensitive", paths: []string{"Docs", "docs"}},
		{name: "whitespace padded", paths: []string{" docs"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateSparsePaths(test.paths); err == nil {
				t.Fatalf("validateSparsePaths(%q) succeeded", test.paths)
			}
		})
	}
	if err := validateSparsePaths([]string{"advisories/github-reviewed", "docs"}); err != nil {
		t.Fatalf("valid sparse paths were rejected: %v", err)
	}
}

func TestGoGitSparseCloneReturnsUnsupported(t *testing.T) {
	remote := createBareFixture(t)
	destination := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (goGitEngine{}).Clone(context.Background(), Repository{URL: remote, Host: "local.test", Transport: "https"}, CloneRequest{
		URL: remote, Destination: destination, SparsePaths: []string{"docs"},
	}, Credential{}, 0, "")
	if err != ErrUnsupported {
		t.Fatalf("go-git sparse clone error = %v, want unsupported", err)
	}
}

func createSparseFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "remote.git")
	runGit(t, "init", "-b", "main", work)
	runGit(t, "-C", work, "config", "user.name", "Git Ops Test")
	runGit(t, "-C", work, "config", "user.email", "gitops@example.test")
	files := map[string]string{
		"advisories/github-reviewed/GHSA-test.json": `{"id":"GHSA-test"}`,
		"docs/unselected.txt":                       "unselected\n",
	}
	for name, content := range files {
		path := filepath.Join(work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, "-C", work, "add", ".")
	runGit(t, "-C", work, "commit", "-m", "sparse fixture")
	runGit(t, "clone", "--bare", work, bare)
	if strings.TrimSpace(string(runGitOutput(t, "--git-dir", bare, "rev-parse", "HEAD"))) == "" {
		t.Fatal("bare fixture was not initialized")
	}
	return bare
}

func hasProcessEvent(events []OperationEvent, operation string) bool {
	for _, event := range events {
		if event.Kind == EventProcess && event.Operation == operation && event.Process != nil && event.Process.Name != "" {
			return true
		}
	}
	return false
}
