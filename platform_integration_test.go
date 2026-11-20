package gitops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfiguredPlatformCompatibility(t *testing.T) {
	platforms := []string{"GITHUB", "GITLAB", "GITEA", "GITEE", "PRIVATE"}
	configured := 0
	for _, platform := range platforms {
		platform := platform
		t.Run(platform, func(t *testing.T) {
			url, ok := os.LookupEnv("GITOPS_TEST_" + platform + "_URL")
			if !ok || url == "" {
				t.Skip("set GITOPS_TEST_" + platform + "_URL to enable this integration case")
			}
			configured++
			repository, err := ParseURL(url)
			if err != nil {
				t.Fatal(operationError(err))
			}
			username := os.Getenv("GITOPS_TEST_" + platform + "_USERNAME")
			token := os.Getenv("GITOPS_TEST_" + platform + "_TOKEN")
			password := os.Getenv("GITOPS_TEST_" + platform + "_PASSWORD")
			sshKeyPath := os.Getenv("GITOPS_TEST_" + platform + "_SSH_KEY_FILE")
			var sshKey []byte
			if sshKeyPath != "" {
				sshKey, err = os.ReadFile(sshKeyPath)
				if err != nil {
					t.Fatal("failed to read the configured SSH key file")
				}
			}
			timeout := 45 * time.Second
			if platform == "PRIVATE" && os.Getenv("GITOPS_TEST_PRIVATE_REF_MATRIX") == "1" {
				timeout = 2 * time.Minute
			}
			client, err := NewClient(Config{
				Timeout:              timeout,
				AllowedHosts:         []string{repository.Host},
				DiscoverSSHKeys:      repository.Transport == "ssh" && os.Getenv("GITOPS_TEST_SSH_DISCOVERY") == "1",
				UseUserSSHKnownHosts: repository.Transport == "ssh" && os.Getenv("GITOPS_TEST_SSH_USER_KNOWN_HOSTS") == "1",
				CredentialFor: func(host string) (Credential, error) {
					if host != repository.Host {
						return Credential{}, ErrHostNotAllowed
					}
					return Credential{Username: username, Token: token, Password: password, SSHPrivateKey: sshKey}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if platform == "PRIVATE" && os.Getenv("GITOPS_TEST_PRIVATE_USER_API") == "1" {
				credential := Credential{Provider: ProviderGitLab, Username: username, Token: token, Password: password}
				result, err := client.TestHostCredential(context.Background(), CredentialTestRequest{
					Host: repository.Host, Credential: &credential, Timeout: timeout,
				})
				switch {
				case result.CredentialStatus == CredentialStatusAccepted && result.ProfileAccess == ProfileAccessAllowed:
					if err != nil {
						t.Fatalf("GitLab user API allowed profile access but returned an error: %v", err)
					}
					if result.Diagnostic.Code != "git_credential_authenticated" {
						t.Fatalf("GitLab user API success diagnostic = %q", result.Diagnostic.Code)
					}
				case result.CredentialStatus == CredentialStatusAccepted && result.ProfileAccess == ProfileAccessInsufficientScope:
					var credentialTestError *CredentialTestError
					if !errors.As(err, &credentialTestError) {
						t.Fatalf("GitLab user API scope denial error = %T %v, want *CredentialTestError", err, err)
					}
					if result.Diagnostic.Code != "git_credential_api_scope_insufficient" || credentialTestError.Diagnostic.Code != result.Diagnostic.Code || ErrorCode(err) != result.Diagnostic.Code {
						t.Fatalf("GitLab user API scope denial result = %#v, error diagnostic = %#v", result, credentialTestError.Diagnostic)
					}
				default:
					t.Fatalf("GitLab user API returned unexpected result: %#v, error: %v", result, err)
				}
				t.Logf("GitLab user API result: credential=%s profile_access=%s code=%s phase=%s", result.CredentialStatus, result.ProfileAccess, result.Diagnostic.Code, result.Diagnostic.Phase)
				return
			}
			ref := os.Getenv("GITOPS_TEST_" + platform + "_REF")
			probe, err := client.Probe(context.Background(), ProbeRequest{URL: url, Ref: ref})
			if err != nil {
				t.Fatalf("repository probe failed (%s): %v", ErrorCode(err), err)
			}
			if probe.ResolvedCommit == "" {
				t.Fatal("probe returned no resolved commit")
			}
			clone, err := client.Clone(context.Background(), CloneRequest{
				URL: url, Ref: ref, Destination: filepath.Join(t.TempDir(), "clone"),
			})
			if err != nil {
				t.Fatalf("repository clone failed (%s): %v", ErrorCode(err), err)
			}
			if clone.Commit != probe.ResolvedCommit {
				t.Fatalf("probe commit %s differs from cloned commit %s", probe.ResolvedCommit, clone.Commit)
			}
			if platform == "PRIVATE" && os.Getenv("GITOPS_TEST_PRIVATE_REF_MATRIX") == "1" {
				verifyPrivateReferenceMatrix(t, client, url, probe)
			}
			if platform == "PRIVATE" && os.Getenv("GITOPS_TEST_PRIVATE_NEGATIVE") == "1" {
				verifyPrivateFailurePaths(t, client, requestFor(url, ref), username, token, password)
			}
		})
	}
	if configured == 0 {
		t.Log("no external Git provider integration URLs configured; all provider cases skipped")
	}
}

func verifyPrivateReferenceMatrix(t *testing.T, client *Client, url string, defaultProbe ProbeResult) {
	t.Helper()
	if defaultProbe.DefaultBranch == "" || defaultProbe.ResolvedRef != defaultProbe.DefaultBranch {
		t.Fatalf("default ref %q did not resolve to advertised default branch %q", defaultProbe.ResolvedRef, defaultProbe.DefaultBranch)
	}

	var otherBranch *Reference
	for i := range defaultProbe.References {
		reference := &defaultProbe.References[i]
		if reference.Type == "branch" && reference.Name != defaultProbe.DefaultBranch {
			otherBranch = reference
			break
		}
	}
	if otherBranch == nil {
		t.Log("private repository has no second advertised branch; branch and commit matrix cases were skipped")
		return
	}

	t.Run("other_branch", func(t *testing.T) {
		verifyPrivateRefProbe(t, client, url, "other branch", otherBranch.Name, otherBranch.Commit)
	})
	t.Run("commit_sha", func(t *testing.T) {
		verifyPrivateRefClone(t, client, url, "commit SHA", otherBranch.Commit, otherBranch.Name, otherBranch.Commit)
	})
	t.Run("commit_sha_as_ref", func(t *testing.T) {
		verifyPrivateDirectCommitClone(t, client, url, otherBranch.Commit)
	})
}

func verifyPrivateRefProbe(t *testing.T, client *Client, url, label, ref, expectedCommit string) {
	t.Helper()
	probe, err := client.Probe(context.Background(), ProbeRequest{URL: url, Ref: ref})
	if err != nil {
		t.Fatalf("%s probe failed (%s): %v", label, ErrorCode(err), err)
	}
	if probe.ResolvedCommit != expectedCommit {
		t.Fatalf("%s resolved to %s, want %s", label, probe.ResolvedCommit, expectedCommit)
	}
}

func verifyPrivateRefClone(t *testing.T, client *Client, url, label, probeRef, cloneRef, expectedCommit string) {
	t.Helper()
	verifyPrivateRefProbe(t, client, url, label, probeRef, expectedCommit)
	clone, err := client.Clone(context.Background(), CloneRequest{
		URL: url, Ref: cloneRef, Commit: expectedCommit, Destination: filepath.Join(t.TempDir(), "clone"),
	})
	if err != nil {
		t.Fatalf("%s clone failed (%s): %v", label, ErrorCode(err), err)
	}
	if clone.Commit != expectedCommit {
		t.Fatalf("%s cloned commit %s, want %s", label, clone.Commit, expectedCommit)
	}
}

func verifyPrivateDirectCommitClone(t *testing.T, client *Client, url, commit string) {
	t.Helper()
	verifyPrivateRefProbe(t, client, url, "commit SHA", commit, commit)
	clone, err := client.Clone(context.Background(), CloneRequest{
		URL: url, Ref: commit, Destination: filepath.Join(t.TempDir(), "clone"),
	})
	if err != nil {
		t.Fatalf("direct commit-SHA clone failed (%s): %v", ErrorCode(err), err)
	}
	if clone.Commit != commit {
		t.Fatalf("direct commit-SHA clone resolved to %s, want %s", clone.Commit, commit)
	}
}

func verifyPrivateFailurePaths(t *testing.T, client *Client, request ProbeRequest, username, token, password string) {
	t.Helper()
	if token != "" || password != "" {
		wrongCredential := Credential{Username: username, Token: "invalid-gitops-test-token"}
		if token == "" {
			wrongCredential.Token = ""
			wrongCredential.Password = "invalid-gitops-test-password"
		}
		_, err := client.TestCredential(context.Background(), request, wrongCredential)
		if !expectedAccessDenied(err) {
			t.Fatalf("wrong-credential result code = %s, want authentication/not-found denial", ErrorCode(err))
		}
		for _, secret := range []string{wrongCredential.Token, wrongCredential.Password} {
			if secret != "" && strings.Contains(err.Error(), secret) {
				t.Fatal("wrong-credential failure leaked the supplied secret")
			}
		}
	}
	_, err := client.TestCredential(context.Background(), request, Credential{})
	if !expectedAccessDenied(err) {
		t.Fatalf("anonymous result code = %s, want authentication/not-found denial", ErrorCode(err))
	}
	verifyCanceledProbe(t, client, request)
}

func expectedAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	return ErrorCode(err) == string(ErrorAuthentication) || ErrorCode(err) == string(ErrorNotFound)
}

func verifyCanceledProbe(t *testing.T, client *Client, request ProbeRequest) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Probe(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe error = %s, want canceled", ErrorCode(err))
	}
	timeout, cancelTimeout := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelTimeout()
	if _, err := client.Probe(timeout, request); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expired probe error = %s, want timeout/canceled", ErrorCode(err))
	}
}

func requestFor(url, ref string) ProbeRequest {
	return ProbeRequest{URL: url, Ref: ref}
}
