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
			gitBinary := os.Getenv("GITOPS_TEST_GIT_BINARY")
			sshKeyPath := os.Getenv("GITOPS_TEST_" + platform + "_SSH_KEY_FILE")
			var sshKey []byte
			if sshKeyPath != "" {
				sshKey, err = os.ReadFile(sshKeyPath)
				if err != nil {
					t.Fatal("failed to read the configured SSH key file")
				}
			}
			client, err := NewClient(Config{
				GitBinary:    gitBinary,
				Timeout:      45 * time.Second,
				AllowedHosts: []string{repository.Host},
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
			if platform == "PRIVATE" && os.Getenv("GITOPS_TEST_PRIVATE_NEGATIVE") == "1" {
				verifyPrivateFailurePaths(t, client, requestFor(url, ref), username, token, password)
			}
		})
	}
	if configured == 0 {
		t.Log("no external Git provider integration URLs configured; all provider cases skipped")
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
