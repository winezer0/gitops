package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func TestResolveSubmoduleURL(t *testing.T) {
	tests := []struct {
		name      string
		parentURL string
		moduleURL string
		want      string
		wantErr   bool
	}{
		{name: "HTTPS sibling", parentURL: "https://git.example.test/group/root.git", moduleURL: "../child.git", want: "https://git.example.test/group/child.git"},
		{name: "HTTPS nested", parentURL: "https://git.example.test/group/root.git", moduleURL: "vendor/grand.git", want: "https://git.example.test/group/root.git/vendor/grand.git"},
		{name: "SSH URL sibling", parentURL: "ssh://git@git.example.test/group/root.git", moduleURL: "../child.git", want: "ssh://git@git.example.test/group/child.git"},
		{name: "SCP URL sibling", parentURL: "git@git.example.test:group/root.git", moduleURL: "../child.git", want: "git@git.example.test:group/child.git"},
		{name: "absolute HTTPS URL", parentURL: "https://git.example.test/group/root.git", moduleURL: "https://other.example.test/child.git", want: "https://other.example.test/child.git"},
		{name: "network path rejected", parentURL: "https://git.example.test/group/root.git", moduleURL: "//other.example.test/child.git", wantErr: true},
		{name: "absolute SCP URL", parentURL: "https://git.example.test/group/root.git", moduleURL: "git@other.example.test:group/child.git", want: "git@other.example.test:group/child.git"},
		{name: "relative URL cannot escape SCP root", parentURL: "git@git.example.test:group/root.git", moduleURL: "../../../child.git", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveSubmoduleURL(test.parentURL, test.moduleURL)
			if (err != nil) != test.wantErr {
				t.Fatalf("resolveSubmoduleURL() error = %v, wantErr %v", err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("resolveSubmoduleURL() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestValidateSubmodulePath(t *testing.T) {
	tests := []struct {
		path    string
		wantErr bool
	}{
		{path: "deps/child"},
		{path: "third-party/dep with spaces"},
		{path: "", wantErr: true},
		{path: "../outside", wantErr: true},
		{path: "a/../outside", wantErr: true},
		{path: "/outside", wantErr: true},
		{path: `C:\outside`, wantErr: true},
		{path: ".git/config", wantErr: true},
		{path: "NUL.txt", wantErr: true},
		{path: "deps/child.", wantErr: true},
		{path: "deps//child", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			err := validateSubmodulePath(test.path)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateSubmodulePath(%q) error = %v, wantErr %v", test.path, err, test.wantErr)
			}
		})
	}
}

func TestSubmoduleDestinationRejectsNonEmptyPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "deps", "child")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "existing.txt"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := submoduleDestination(root, "deps/child"); err == nil {
		t.Fatal("expected non-empty submodule destination to be rejected")
	}
}

func TestSubmoduleDestinationRemovesEmptyPlaceholder(t *testing.T) {
	root := t.TempDir()
	placeholder := filepath.Join(root, "deps", "child")
	if err := os.MkdirAll(placeholder, 0o700); err != nil {
		t.Fatal(err)
	}
	destination, err := submoduleDestination(root, "deps/child")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("empty placeholder still exists at %q: %v", destination, err)
	}
}

func TestSubmoduleDestinationRejectsSymlinkTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	if _, err := submoduleDestination(root, "linked/child"); err == nil {
		t.Fatal("expected symlink traversal to be rejected")
	}
}

func TestRecursiveSubmoduleClonePinsCommitsAndPropagatesDepth(t *testing.T) {
	root := t.TempDir()
	grandchildRemote := createBareFixture(t)
	grandchildCommit := strings.TrimSpace(string(runGitOutput(t, "--git-dir", grandchildRemote, "rev-parse", "refs/heads/main")))

	childWorktree := filepath.Join(root, "child-worktree")
	childCommit := createGitlinkWorktree(t, childWorktree, "nested/grandchild", "../grandchild.git", grandchildCommit)
	if err := os.WriteFile(filepath.Join(childWorktree, "newer.txt"), []byte("newer commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", childWorktree, "add", "newer.txt")
	runGit(t, "-C", childWorktree, "commit", "-m", "newer child commit")
	childRemote := filepath.Join(root, "child.git")
	runGit(t, "clone", "--bare", childWorktree, childRemote)

	rootWorktree := filepath.Join(root, "root-worktree")
	createGitlinkWorktree(t, rootWorktree, "deps/child", "../child.git", childCommit)
	parentRepository, err := ParseURL("https://git.example.test/group/root.git")
	if err != nil {
		t.Fatal(err)
	}
	var validatedHosts []string
	var credentialHosts []string
	client, err := NewClient(Config{
		AllowedHosts: []string{"git.example.test"},
		ValidateHost: func(_ context.Context, host string) error {
			validatedHosts = append(validatedHosts, host)
			return nil
		},
		CredentialFor: func(host string) (Credential, error) {
			credentialHosts = append(credentialHosts, host)
			return Credential{Username: "git", Token: "credential"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	parentRepository, parentCredential, err := client.prepare(context.Background(), parentRepository.URL)
	if err != nil {
		t.Fatal(err)
	}
	remoteByURL := map[string]string{
		"https://git.example.test/group/child.git":      childRemote,
		"https://git.example.test/group/grandchild.git": grandchildRemote,
	}
	var clonedRequests []CloneRequest
	cloneRepository := func(ctx context.Context, repository Repository, request CloneRequest, credential Credential) (CloneResult, error) {
		if credential.Token != "credential" {
			return CloneResult{}, errors.New("child credential was not resolved")
		}
		localRemote, ok := remoteByURL[repository.URL]
		if !ok {
			return CloneResult{}, fmt.Errorf("unexpected test remote %q", repository.URL)
		}
		clonedRequests = append(clonedRequests, request)
		cloned, err := git.PlainCloneContext(ctx, request.Destination, false, &git.CloneOptions{URL: localRemote, Depth: request.Depth})
		if err != nil {
			return CloneResult{}, err
		}
		worktree, err := cloned.Worktree()
		if err != nil {
			return CloneResult{}, err
		}
		if request.Commit != "" {
			if err := worktree.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(request.Commit), Force: true}); err != nil {
				return CloneResult{}, err
			}
		}
		head, err := cloned.Head()
		if err != nil {
			return CloneResult{}, err
		}
		return CloneResult{Repository: repository, Commit: head.Hash().String(), Path: request.Destination}, nil
	}
	state := submoduleCloneState{
		client:          client,
		ctx:             context.Background(),
		depth:           7,
		cloneRepository: cloneRepository,
	}
	if err := state.clone(parentRepository, parentCredential, rootWorktree, 0); err != nil {
		t.Fatal(err)
	}
	if len(clonedRequests) != 2 {
		t.Fatalf("recursive clone request count = %d, want 2", len(clonedRequests))
	}
	if clonedRequests[0].Commit != childCommit || clonedRequests[1].Commit != grandchildCommit {
		t.Fatalf("recursive clone commits = [%q %q], want [%q %q]", clonedRequests[0].Commit, clonedRequests[1].Commit, childCommit, grandchildCommit)
	}
	for _, request := range clonedRequests {
		if request.Depth != 7 || request.Ref != "" || request.RecurseSubmodules {
			t.Fatalf("recursive clone request = %#v, want inherited depth and pinned-commit checkout", request)
		}
	}
	if len(validatedHosts) != 3 || len(credentialHosts) != 3 {
		t.Fatalf("host checks = %v, credential lookups = %v; want root plus two child checks", validatedHosts, credentialHosts)
	}
	childPath := filepath.Join(rootWorktree, "deps", "child")
	if strings.TrimSpace(string(runGitOutput(t, "-C", childPath, "rev-parse", "HEAD"))) != childCommit {
		t.Fatal("child submodule did not remain at the parent gitlink commit")
	}
	if _, err := os.Stat(filepath.Join(childPath, "newer.txt")); !os.IsNotExist(err) {
		t.Fatalf("child submodule followed its branch tip instead of the gitlink commit: %v", err)
	}
	grandchildPath := filepath.Join(childPath, "nested", "grandchild")
	if strings.TrimSpace(string(runGitOutput(t, "-C", grandchildPath, "rev-parse", "HEAD"))) != grandchildCommit {
		t.Fatal("nested submodule did not remain at the parent gitlink commit")
	}
}

func TestRecursiveSubmoduleCloneValidatesChildHostBeforeClone(t *testing.T) {
	root := t.TempDir()
	childRemote := createBareFixture(t)
	childCommit := strings.TrimSpace(string(runGitOutput(t, "--git-dir", childRemote, "rev-parse", "refs/heads/main")))
	rootWorktree := filepath.Join(root, "root-worktree")
	createGitlinkWorktree(t, rootWorktree, "deps/child", "https://other.example.test/child.git", childCommit)
	parentRepository, err := ParseURL("https://git.example.test/group/root.git")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{AllowedHosts: []string{"git.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	cloneCalled := false
	state := submoduleCloneState{
		client: client,
		ctx:    context.Background(),
		cloneRepository: func(context.Context, Repository, CloneRequest, Credential) (CloneResult, error) {
			cloneCalled = true
			return CloneResult{}, nil
		},
	}
	if err := state.clone(parentRepository, Credential{}, rootWorktree, 0); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("unapproved child host error = %v, want host denied", err)
	}
	if cloneCalled {
		t.Fatal("unapproved submodule host reached the clone engine")
	}
}

func createGitlinkWorktree(t *testing.T, worktree, modulePath, moduleURL, moduleCommit string) string {
	t.Helper()
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, "init", "-b", "main", worktree)
	runGit(t, "-C", worktree, "config", "user.name", "Git Ops Test")
	runGit(t, "-C", worktree, "config", "user.email", "gitops@example.test")
	modules := fmt.Sprintf("[submodule \"fixture\"]\n\tpath = %s\n\turl = %s\n", modulePath, moduleURL)
	if err := os.WriteFile(filepath.Join(worktree, ".gitmodules"), []byte(modules), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", worktree, "add", ".gitmodules")
	runGit(t, "-C", worktree, "update-index", "--add", "--cacheinfo", "160000,"+moduleCommit+","+modulePath)
	runGit(t, "-C", worktree, "commit", "-m", "fixture with submodule")
	return strings.TrimSpace(string(runGitOutput(t, "-C", worktree, "rev-parse", "HEAD")))
}
