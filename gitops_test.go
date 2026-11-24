package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseURL(t *testing.T) {
	tests := []struct {
		name, input, host, transport string
		wantErr                      bool
	}{
		{name: "HTTPS", input: "https://Git.Example.com/team/repo.git", host: "git.example.com", transport: "https"},
		{name: "SSH URL", input: "ssh://git@git.example.com:2222/team/repo.git", host: "git.example.com:2222", transport: "ssh"},
		{name: "scp SSH", input: "git@git.example.com:team/repo.git", host: "git.example.com", transport: "ssh"},
		{name: "embedded password", input: "https://user:secret@git.example.com/repo.git", wantErr: true},
		{name: "query rejected", input: "https://git.example.com/repo.git?token=secret", wantErr: true},
		{name: "unsupported scheme", input: "git://git.example.com/repo.git", wantErr: true},
		{name: "empty path", input: "https://git.example.com", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseURL(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("ParseURL() error = %v, wantErr %v", err, test.wantErr)
			}
			if err == nil && (got.Host != test.host || got.Transport != test.transport) {
				t.Fatalf("ParseURL() = %#v", got)
			}
		})
	}
}

func TestValidateHostAllowsConfiguredHostnameOnCustomPort(t *testing.T) {
	for _, test := range []struct {
		name        string
		host        string
		allowed     []string
		wantAllowed bool
	}{
		{name: "bare host covers custom port", host: "git.example.test:8443", allowed: []string{"git.example.test"}, wantAllowed: true},
		{name: "port-specific entry is exact", host: "git.example.test:8444", allowed: []string{"git.example.test:8443"}},
		{name: "exact host and port", host: "git.example.test:8443", allowed: []string{"git.example.test:8443"}, wantAllowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateHost(test.host, test.allowed)
			if (err == nil) != test.wantAllowed {
				t.Fatalf("ValidateHost(%q, %#v) error = %v", test.host, test.allowed, err)
			}
		})
	}
}

func TestGoGitProbeCloneAndRefs(t *testing.T) {
	remote := createBareFixture(t)
	repository := Repository{URL: remote, Host: "local.test", Transport: "https"}
	probe, err := (goGitEngine{}).Probe(context.Background(), repository, ProbeRequest{Ref: "v1"}, Credential{}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if probe.ResolvedRef != "v1" || probe.ResolvedCommit == "" {
		t.Fatalf("unexpected probe result: %#v", probe)
	}
	defaultProbe, err := (goGitEngine{}).Probe(context.Background(), repository, ProbeRequest{}, Credential{}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if defaultProbe.DefaultBranch != "main" || defaultProbe.ResolvedCommit == "" {
		t.Fatalf("go-git default branch result = %#v", defaultProbe)
	}
	destination := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	cloned, err := (goGitEngine{}).Clone(context.Background(), repository, CloneRequest{URL: remote, Ref: "v1", Destination: destination}, Credential{}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if cloned.Commit != probe.ResolvedCommit || cloned.Engine != "go-git" {
		t.Fatalf("unexpected clone result: %#v", cloned)
	}
}

func TestCredentialHelpersAreRejected(t *testing.T) {
	_, err := NewClient(Config{UseCredentialHelpers: true})
	if !errors.Is(err, ErrCredentialHelpersUnsupported) {
		t.Fatalf("NewClient with credential helpers error = %v, want ErrCredentialHelpersUnsupported", err)
	}
}

func TestCredentialHelpersUnsupportedClassification(t *testing.T) {
	if got := ErrorCode(ErrCredentialHelpersUnsupported); got != string(ErrorUnsupported) {
		t.Fatalf("credential helper error code = %q, want %q", got, ErrorUnsupported)
	}
}

func TestCheckoutAndValidateCheckout(t *testing.T) {
	remote := createBareFixture(t)
	repository := Repository{URL: remote, Host: "local.test", Transport: "https"}
	t.Run("go-git", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "checkout")
		cloned, err := (goGitEngine{}).Clone(context.Background(), repository, CloneRequest{URL: remote, Destination: path}, Credential{}, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		runGit(t, "-C", path, "branch", "feature")
		runGit(t, "-C", path, "config", "remote.origin.url", "https://local.test/team/repo.git")
		client, err := NewClient(Config{})
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.Checkout(context.Background(), CheckoutRequest{Path: path, Ref: "feature"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Commit != cloned.Commit || result.Engine != "go-git" {
			t.Fatalf("checkout result = %#v", result)
		}
		valid, commit, err := client.ValidateCheckout(path, "https://local.test/team/repo.git", cloned.Commit)
		if err != nil || !valid || commit != cloned.Commit {
			t.Fatalf("ValidateCheckout() = (%v, %q, %v)", valid, commit, err)
		}
		if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("modified\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		valid, _, err = client.ValidateCheckout(path, "https://local.test/team/repo.git", cloned.Commit)
		if valid || !errors.Is(err, ErrDirtyWorktree) {
			t.Fatalf("dirty checkout validation = (%v, %v)", valid, err)
		}
	})
}

func TestCloneResolvesOlderCommitSHA(t *testing.T) {
	remote, oldCommit := createHistoryFixture(t)
	repository := Repository{URL: remote, Host: "local.test", Transport: "https"}
	destination := filepath.Join(t.TempDir(), "clone")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (goGitEngine{}).Clone(context.Background(), repository, CloneRequest{URL: remote, Commit: oldCommit, Destination: destination}, Credential{}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit != oldCommit {
		t.Fatalf("cloned commit = %s, want %s", result.Commit, oldCommit)
	}
}

func TestClientRestrictsHostsBeforeConnecting(t *testing.T) {
	client, err := NewClient(Config{AllowedHosts: []string{"git.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Probe(context.Background(), ProbeRequest{URL: "https://other.example.test/repo.git"}); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("host policy error = %v", err)
	}
}

func TestErrorClassificationAndSecretRedaction(t *testing.T) {
	cause := fmt.Errorf("authentication failed for token secret-value")
	classified := operationError(cause)
	var result *OperationError
	if !errors.As(classified, &result) || result.Kind != ErrorAuthentication || result.Retryable {
		t.Fatalf("classified error = %#v", result)
	}
	if strings.Contains(classified.Error(), "secret-value") {
		t.Fatal("classified error leaked secret material")
	}
	if !errors.Is(classified, cause) {
		t.Fatal("classified error did not preserve the cause")
	}
	if ErrorCode(nil) != "" {
		t.Fatal("nil error should have an empty error code")
	}
	for _, test := range []struct {
		err  error
		kind ErrorKind
	}{
		{err: context.DeadlineExceeded, kind: ErrorTimeout},
		{err: context.Canceled, kind: ErrorCanceled},
		{err: ErrCloneSizeLimit, kind: ErrorResourceLimit},
		{err: ErrSubmoduleLimit, kind: ErrorResourceLimit},
		{err: ErrUnsupported, kind: ErrorUnsupported},
	} {
		kind, _ := classifyError(test.err)
		if kind != test.kind {
			t.Errorf("classifyError(%v) = %q, want %q", test.err, kind, test.kind)
		}
	}
	for _, message := range []string{
		"remote: HTTP Basic: Access denied",
		"remote: Unauthorized",
		"remote: Invalid username or password",
		"remote: Forbidden",
		"fatal: could not read Username for 'https://git.example.test': terminal prompts disabled",
	} {
		if ErrorCode(fmt.Errorf("%s", message)) != string(ErrorAuthentication) {
			t.Errorf("authentication message %q was not classified as authentication failure", message)
		}
	}
}

func TestProgressWriterEmitsStructuredProgress(t *testing.T) {
	var events []ProgressEvent
	writer := progressWriter{engine: "go-git", callback: func(event ProgressEvent) { events = append(events, event) }}
	if _, err := writer.Write([]byte("Receiving objects: 47% (14/30), 2.1 MiB | 1.2 MiB/s\r")); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0] != (ProgressEvent{Engine: "go-git", Phase: "receiving objects", Percent: 47, ReceivedObjects: 14, TotalObjects: 30, BytesReceived: 2202009}) {
		t.Fatalf("progress events = %#v", events)
	}
}

func TestCommitProbeReportsDeferredValidation(t *testing.T) {
	commit := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	output := []byte("ref: refs/heads/main\tHEAD\n" + strings.Repeat("b", 40) + "\trefs/heads/main\n")
	result, err := parseReferences(Repository{}, commit, output, "go-git")
	if err != nil {
		t.Fatal(err)
	}
	if result.ResolvedCommit != commit || !result.CommitValidationDeferred {
		t.Fatalf("commit probe result = %#v", result)
	}
}

func TestClientAppliesHostValidationBeforeGit(t *testing.T) {
	called := false
	client, err := NewClient(Config{ValidateHost: func(_ context.Context, host string) error {
		called = true
		if host != "git.example.test" {
			t.Fatalf("validated host = %q", host)
		}
		return ErrHostNotAllowed
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Probe(context.Background(), ProbeRequest{URL: "https://git.example.test/repo.git"}); err == nil {
		t.Fatal("expected host validation error")
	}
	if !called {
		t.Fatal("host validation callback was not called")
	}
}

func TestCloneDestinationAndSizeLimits(t *testing.T) {
	root := t.TempDir()
	client, err := NewClient(Config{DestinationRoot: root, MaxCloneBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	destination, _, err := client.prepareDestination(filepath.Join(root, "repo"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.prepareDestination(filepath.Join(root, "..", "outside"), false); err == nil {
		t.Fatal("expected destination root rejection")
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "large.bin"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkspaceSize(context.Background(), destination, 8); err == nil {
		t.Fatal("expected workspace size rejection")
	} else if !errors.Is(err, ErrCloneSizeLimit) {
		t.Fatalf("size limit error = %v", err)
	}
	if err := checkWorkspaceSize(context.Background(), destination, 10); err != nil {
		t.Fatalf("expected exact size limit to pass, got %v", err)
	}
	if _, _, err := client.prepareDestination(destination, false); err == nil {
		t.Fatal("expected existing clone destination rejection")
	}
}

func TestWorkspaceMonitorCancelsAtLimit(t *testing.T) {
	root := t.TempDir()
	operationContext, cancelOperation := context.WithCancel(context.Background())
	defer cancelOperation()
	monitorContext, cancelMonitor := context.WithCancel(context.Background())
	defer cancelMonitor()
	result := make(chan error, 1)
	go func() {
		result <- monitorWorkspaceSize(monitorContext, root, 4, cancelOperation)
	}()
	if err := os.WriteFile(filepath.Join(root, "large.bin"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrCloneSizeLimit) {
			t.Fatalf("monitor error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workspace monitor did not stop at the configured limit")
	}
	if !errors.Is(operationContext.Err(), context.Canceled) {
		t.Fatal("clone operation context was not canceled")
	}
}

func TestClientRejectsMismatchedCredentials(t *testing.T) {
	client, err := NewClient(Config{CredentialFor: func(string) (Credential, error) {
		return Credential{SSHPrivateKey: []byte("private key")}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.prepare(context.Background(), "https://git.example.test/repo.git")
	if err == nil || errors.Is(err, ErrGitUnavailable) {
		t.Fatalf("mismatched credential error = %v", err)
	}
}

func createBareFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "remote.git")
	runGit(t, "init", "-b", "main", work)
	runGit(t, "-C", work, "config", "user.name", "Git Ops Test")
	runGit(t, "-C", work, "config", "user.email", "gitops@example.test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", work, "add", "README.md")
	runGit(t, "-C", work, "commit", "-m", "fixture")
	runGit(t, "-C", work, "branch", "feature")
	runGit(t, "-C", work, "tag", "-a", "v1", "-m", "release")
	runGit(t, "clone", "--bare", work, bare)
	return bare
}

func createHistoryFixture(t *testing.T) (string, string) {
	t.Helper()
	remote := createBareFixture(t)
	oldCommit := strings.TrimSpace(string(runGitOutput(t, "--git-dir", remote, "rev-parse", "refs/heads/main")))
	work := filepath.Join(t.TempDir(), "work")
	runGit(t, "clone", "--branch", "main", remote, work)
	runGit(t, "-C", work, "config", "user.name", "Git Ops Test")
	runGit(t, "-C", work, "config", "user.email", "gitops@example.test")
	if err := os.WriteFile(filepath.Join(work, "next.txt"), []byte("next\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", work, "add", "next.txt")
	runGit(t, "-C", work, "commit", "-m", "next")
	runGit(t, "-C", work, "push", "origin", "main")
	return remote, oldCommit
}

func runGit(t *testing.T, args ...string) {
	t.Helper()
	runGitOutput(t, args...)
}

func runGitOutput(t *testing.T, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
	return output
}
