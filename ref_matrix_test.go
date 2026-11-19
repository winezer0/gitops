package gitops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReferenceAndCloneMatrixForBothEngines(t *testing.T) {
	remote := createBareFixture(t)
	mainCommit := strings.TrimSpace(string(runGitOutput(t, "--git-dir", remote, "rev-parse", "refs/heads/main")))
	engines := []struct {
		name   string
		engine engine
	}{
		{name: "system Git", engine: systemEngine{binary: "git"}},
		{name: "go-git", engine: goGitEngine{}},
	}
	for _, test := range engines {
		t.Run(test.name, func(t *testing.T) {
			repository := Repository{URL: remote, Host: "local.test", Transport: "https"}
			defaultProbe, err := test.engine.Probe(context.Background(), repository, ProbeRequest{}, Credential{}, 0, "")
			if err != nil {
				t.Fatalf("default-branch probe failed: %v", err)
			}
			if defaultProbe.DefaultBranch != "main" || defaultProbe.ResolvedRef != "main" || defaultProbe.ResolvedCommit != mainCommit {
				t.Fatalf("default-branch result = %#v", defaultProbe)
			}
			assertReferenceType(t, defaultProbe.References, "main", "branch")
			assertReferenceType(t, defaultProbe.References, "feature", "branch")
			assertReferenceType(t, defaultProbe.References, "v1", "tag")

			for _, reference := range []struct{ requested, resolved string }{
				{requested: "", resolved: "main"},
				{requested: "main", resolved: "main"},
				{requested: "feature", resolved: "feature"},
				{requested: "v1", resolved: "v1"},
			} {
				probe, err := test.engine.Probe(context.Background(), repository, ProbeRequest{Ref: reference.requested}, Credential{}, 0, "")
				if err != nil {
					t.Fatalf("probe ref %q failed: %v", reference.requested, err)
				}
				if probe.ResolvedRef != reference.resolved || probe.ResolvedCommit != mainCommit {
					t.Errorf("probe ref %q = %#v", reference.requested, probe)
				}

				destination := filepath.Join(t.TempDir(), "clone")
				if err := os.Mkdir(destination, 0o700); err != nil {
					t.Fatal(err)
				}
				clone, err := test.engine.Clone(context.Background(), repository, CloneRequest{
					URL: remote, Ref: reference.requested, Destination: destination,
				}, Credential{}, 0, "")
				if err != nil {
					t.Fatalf("clone ref %q failed: %v", reference.requested, err)
				}
				if clone.Commit != mainCommit {
					t.Errorf("clone ref %q commit = %s, want %s", reference.requested, clone.Commit, mainCommit)
				}
			}

			commitProbe, err := test.engine.Probe(context.Background(), repository, ProbeRequest{Ref: mainCommit}, Credential{}, 0, "")
			if err != nil {
				t.Fatalf("commit probe failed: %v", err)
			}
			if commitProbe.ResolvedCommit != mainCommit || commitProbe.CommitValidationDeferred {
				t.Fatalf("commit probe result = %#v", commitProbe)
			}
			invalidProbe, err := test.engine.Probe(context.Background(), repository, ProbeRequest{Ref: "missing-ref"}, Credential{}, 0, "")
			if !errors.Is(err, ErrInvalidReference) || invalidProbe.ResolvedRef != "missing-ref" {
				t.Fatalf("missing-ref probe = %#v, %v", invalidProbe, err)
			}
		})
	}
}

func assertReferenceType(t *testing.T, references []Reference, name, kind string) {
	t.Helper()
	for _, reference := range references {
		if reference.Name == name && reference.Type == kind {
			return
		}
	}
	t.Errorf("reference %q of type %q not found in %#v", name, kind, references)
}
