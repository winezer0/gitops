package gitops

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientCloneCleansDestinationAfterRemoteFailure(t *testing.T) {
	certificate, certificatePEM := newLocalGitCertificate(t)
	server := newLocalGitTLSServer(t, certificate, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("WWW-Authenticate", `Basic realm="clone-cleanup"`)
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	disableHTTPProxyEnvironment(t)

	repositoryURL := server.URL + "/team/missing.git"
	repository, err := ParseURL(repositoryURL)
	if err != nil {
		t.Fatal("failed to parse test repository URL")
	}
	client, err := NewClient(Config{AllowedHosts: []string{repository.Host}, Timeout: 5 * time.Second, CABundle: certificatePEM})
	if err != nil {
		t.Fatal("failed to create test Git client")
	}
	destination := filepath.Join(t.TempDir(), "clone")
	_, err = client.Clone(context.Background(), CloneRequest{URL: repositoryURL, Ref: "main", Destination: destination})
	if ErrorCode(err) != string(ErrorAuthentication) {
		t.Fatalf("failed clone error code = %s, want %s", ErrorCode(err), ErrorAuthentication)
	}
	assertPathDoesNotExist(t, destination)
}

func TestClientClonePreservesExistingDestinationAfterFailure(t *testing.T) {
	certificate, certificatePEM := newLocalGitCertificate(t)
	server := newLocalGitTLSServer(t, certificate, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("WWW-Authenticate", `Basic realm="clone-rollback"`)
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	disableHTTPProxyEnvironment(t)

	repositoryURL := server.URL + "/team/private.git"
	repository, err := ParseURL(repositoryURL)
	if err != nil {
		t.Fatal("failed to parse test repository URL")
	}
	client, err := NewClient(Config{AllowedHosts: []string{repository.Host}, Timeout: 5 * time.Second, CABundle: certificatePEM})
	if err != nil {
		t.Fatal("failed to create test Git client")
	}
	destination := filepath.Join(t.TempDir(), "clone")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal("failed to prepare existing workspace")
	}
	marker := filepath.Join(destination, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal("failed to prepare existing workspace marker")
	}
	_, err = client.Clone(context.Background(), CloneRequest{
		URL: repositoryURL, Ref: "main", Destination: destination, ReplaceExisting: true,
	})
	if ErrorCode(err) != string(ErrorAuthentication) {
		t.Fatalf("failed replacement error code = %s, want %s", ErrorCode(err), ErrorAuthentication)
	}
	contents, readErr := os.ReadFile(marker)
	if readErr != nil || string(contents) != "keep" {
		t.Fatalf("existing workspace marker was not preserved: %v", readErr)
	}
}

func TestClientCloneCleansDestinationOnCancellation(t *testing.T) {
	server, requestStarted, certificatePEM := newBlockingLocalGitServer(t)
	defer server.Close()
	repositoryURL := server.URL + "/team/private.git"
	repository, err := ParseURL(repositoryURL)
	if err != nil {
		t.Fatal("failed to parse test repository URL")
	}
	client, err := NewClient(Config{AllowedHosts: []string{repository.Host}, Timeout: 10 * time.Second, CABundle: certificatePEM})
	if err != nil {
		t.Fatal("failed to create test Git client")
	}
	destination := filepath.Join(t.TempDir(), "clone")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cloneResult := make(chan error, 1)
	go func() {
		_, cloneErr := client.Clone(ctx, CloneRequest{URL: repositoryURL, Ref: "main", Destination: destination})
		cloneResult <- cloneErr
	}()
	waitForLocalGitRequest(t, requestStarted)
	cancel()
	select {
	case err := <-cloneResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled clone error = %s, want canceled", ErrorCode(err))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clone did not stop after context cancellation")
	}
	assertPathDoesNotExist(t, destination)
}

func TestClientCloneCleansDestinationAtSizeLimit(t *testing.T) {
	server, requestStarted, certificatePEM := newBlockingLocalGitServer(t)
	defer server.Close()
	repositoryURL := server.URL + "/team/private.git"
	repository, err := ParseURL(repositoryURL)
	if err != nil {
		t.Fatal("failed to parse test repository URL")
	}
	const maximumCloneBytes = 64 << 10
	client, err := NewClient(Config{AllowedHosts: []string{repository.Host}, Timeout: 10 * time.Second, MaxCloneBytes: maximumCloneBytes, CABundle: certificatePEM})
	if err != nil {
		t.Fatal("failed to create test Git client")
	}
	destination := filepath.Join(t.TempDir(), "clone")
	cloneResult := make(chan error, 1)
	go func() {
		_, cloneErr := client.Clone(context.Background(), CloneRequest{URL: repositoryURL, Ref: "main", Destination: destination})
		cloneResult <- cloneErr
	}()
	waitForLocalGitRequest(t, requestStarted)
	oversizedObject := make([]byte, maximumCloneBytes+1)
	stagingPattern := "." + filepath.Base(destination) + ".gitops-stage-"
	entries, err := os.ReadDir(filepath.Dir(destination))
	if err != nil {
		t.Fatal("failed to inspect staging directory")
	}
	var staging string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), stagingPattern) {
			staging = filepath.Join(filepath.Dir(destination), entry.Name())
			break
		}
	}
	if staging == "" {
		t.Fatal("clone staging directory was not created")
	}
	if err := os.WriteFile(filepath.Join(staging, "materialized-object"), oversizedObject, 0o600); err != nil {
		t.Fatal("failed to materialize test clone data")
	}
	select {
	case err := <-cloneResult:
		if !errors.Is(err, ErrCloneSizeLimit) {
			t.Fatalf("size-limited clone error = %s, want resource_limit", ErrorCode(err))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clone did not stop after crossing the size limit")
	}
	assertPathDoesNotExist(t, destination)
}

func TestClientCloneCleansDestinationForInvalidRef(t *testing.T) {
	client, err := NewClient(Config{})
	if err != nil {
		t.Fatal("failed to create test Git client")
	}
	destination := filepath.Join(t.TempDir(), "clone")
	_, err = client.Clone(context.Background(), CloneRequest{
		URL: "https://git.example.test/team/repo.git", Ref: "-invalid", Destination: destination,
	})
	if ErrorCode(err) != string(ErrorInvalidRef) {
		t.Fatalf("invalid ref error code = %s, want %s", ErrorCode(err), ErrorInvalidRef)
	}
	assertPathDoesNotExist(t, destination)
}

func newBlockingLocalGitServer(t *testing.T) (*httptest.Server, <-chan struct{}, []byte) {
	t.Helper()
	certificate, certificatePEM := newLocalGitCertificate(t)
	requestStarted := make(chan struct{}, 1)
	server := newLocalGitTLSServer(t, certificate, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case requestStarted <- struct{}{}:
		default:
		}
		<-request.Context().Done()
	}))
	disableHTTPProxyEnvironment(t)
	return server, requestStarted, certificatePEM
}

func waitForLocalGitRequest(t *testing.T, requestStarted <-chan struct{}) {
	t.Helper()
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("Git client did not reach the local test server")
	}
}

func assertPathDoesNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clone path still exists or could not be checked: %v", err)
	}
}
