package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestGoGitSparseCloneMaterializesSelectedDirectories(t *testing.T) {
	remote := createSparseFixture(t)
	destination := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (goGitEngine{}).Clone(context.Background(), Repository{URL: remote, Host: "local.test", Transport: "https"}, CloneRequest{
		URL: remote, Destination: destination, Depth: 1, SparsePaths: []string{"advisories/github-reviewed"},
	}, Credential{}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit == "" {
		t.Fatal("go-git sparse clone returned an empty commit")
	}
	if _, err := os.Stat(filepath.Join(destination, "advisories", "github-reviewed", "GHSA-test.json")); err != nil {
		t.Fatalf("selected sparse file is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "docs", "unselected.txt")); !os.IsNotExist(err) {
		t.Fatalf("unselected file exists or could not be checked: %v", err)
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
