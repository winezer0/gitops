package gitops

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestAuthenticationFailureWithConfiguredTokenIsNotRetried(t *testing.T) {
	const username = "fallback-test-user"
	const token = "fallback-test-secret"
	var requests atomic.Int32
	var receivedCredential atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		requestUsername, requestToken, ok := request.BasicAuth()
		receivedCredential.Store(ok && requestUsername == username && requestToken == token)
		writer.Header().Set("WWW-Authenticate", `Basic realm="gitops-test"`)
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	listener := &connectionCounterListener{Listener: server.Listener}
	server.Listener = listener
	server.StartTLS()
	defer server.Close()

	certificate := server.Certificate()
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("all_proxy", "")

	url := strings.TrimSuffix(server.URL, "/") + "/team/private.git"
	repository, err := ParseURL(url)
	if err != nil {
		t.Fatal("failed to parse local test repository URL")
	}
	client, err := NewClient(Config{
		AllowedHosts:                  []string{repository.Host},
		FallbackSSHOnHTTPSAuthFailure: true,
		Timeout:                       10 * time.Second,
		CABundle:                      certificatePEM,
		CredentialFor: func(host string) (Credential, error) {
			if host != repository.Host {
				return Credential{}, ErrHostNotAllowed
			}
			return Credential{Username: username, Token: token}, nil
		},
	})
	if err != nil {
		t.Fatal("failed to create test Git client")
	}
	_, err = client.Probe(context.Background(), ProbeRequest{URL: url})
	if ErrorCode(err) != string(ErrorAuthentication) {
		t.Fatalf("authentication error code = %s, want %s", ErrorCode(err), ErrorAuthentication)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("authentication error exposed the test token")
	}
	if !receivedCredential.Load() {
		t.Fatal("the local Git server did not receive the configured credential")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("Git server request count = %d, want 1", got)
	}
	if got := listener.connections.Load(); got != 1 {
		t.Fatalf("TLS connection count = %d, want 1; an authentication failure must not retry through go-git", got)
	}
}

func TestShouldFallbackSSHRequiresAuthenticationFailureWithoutHTTPSCredentials(t *testing.T) {
	client, err := NewClient(Config{FallbackSSHOnHTTPSAuthFailure: true})
	if err != nil {
		t.Fatal(err)
	}
	repository := Repository{Transport: "https"}
	authenticationFailure := &OperationError{Kind: ErrorAuthentication}
	networkFailure := &OperationError{Kind: ErrorNetwork}
	tests := []struct {
		name       string
		repository Repository
		credential Credential
		err        error
		want       bool
	}{
		{name: "anonymous authentication rejection", repository: repository, err: authenticationFailure, want: true},
		{name: "network errors do not fallback", repository: repository, err: networkFailure},
		{name: "https token blocks fallback", repository: repository, credential: Credential{Token: "token"}, err: authenticationFailure},
		{name: "https password blocks fallback", repository: repository, credential: Credential{Password: "password"}, err: authenticationFailure},
		{name: "ssh source does not fallback", repository: Repository{Transport: "ssh"}, err: authenticationFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := client.shouldFallbackSSH(test.repository, test.credential, test.err); got != test.want {
				t.Fatalf("shouldFallbackSSH() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestFallbackFailureDiagnosticDoesNotExposeOriginalError(t *testing.T) {
	const secret = "fallback-secret-value"
	err := fallbackFailure(fmt.Errorf("HTTPS token %s was rejected", secret), ErrSSHHostKeyTrustNotConfigured, Repository{Host: "git.example.test"}, "main", time.Minute, time.Second)
	var operationError *OperationError
	if !errors.As(err, &operationError) {
		t.Fatalf("fallback error type = %T, want *OperationError", err)
	}
	if operationError.Diagnostic.Code != "git_transport_fallback_failed" || operationError.Diagnostic.Transport != sshFallbackTransport {
		t.Fatalf("fallback diagnostic = %#v", operationError.Diagnostic)
	}
	if strings.Contains(operationError.Diagnostic.Message, secret) || strings.Contains(operationError.Diagnostic.Recommendation, secret) {
		t.Fatal("fallback diagnostic exposed credential data")
	}
}

func TestHTTPSAuthenticationFailureFallsBackToSSHForProbeAndClone(t *testing.T) {
	bare := createBareFixture(t)
	sshListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sshListener.Close()

	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	_, userPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	userSigner, err := ssh.NewSignerFromKey(userPrivate)
	if err != nil {
		t.Fatal(err)
	}
	userBlock, err := ssh.MarshalPrivateKey(userPrivate, "fallback-test")
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), userSigner.PublicKey().Marshal()) {
			return nil, fmt.Errorf("unexpected SSH identity")
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(hostSigner)
	go serveGitUploadPackOverSSH(sshListener, serverConfig, bare)

	sshDir := t.TempDir()
	identityPath := filepath.Join(sshDir, "fallback-key")
	if err := os.WriteFile(identityPath, pem.EncodeToMemory(userBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	sshConfig := fmt.Sprintf("Host 127.0.0.1\n  Port %s\n  IdentityFile %s\n", strings.Split(sshListener.Addr().String(), ":")[1], identityPath)
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(sshConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	var httpsRequests atomic.Int32
	httpsServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		httpsRequests.Add(1)
		writer.Header().Set("WWW-Authenticate", `Basic realm="fallback-test"`)
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer httpsServer.Close()
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("all_proxy", "")

	certificate := httpsServer.Certificate()
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	client, err := NewClient(Config{
		AllowedHosts:                  []string{"127.0.0.1"},
		CABundle:                      certificatePEM,
		DiscoverSSHKeys:               true,
		FallbackSSHOnHTTPSAuthFailure: true,
		SSHConfigDir:                  sshDir,
		SSHHostKeyCallback:            ssh.FixedHostKey(hostSigner.PublicKey()),
		Timeout:                       10 * time.Second,
		CredentialForRepository: func(repository Repository) (Credential, error) {
			if repository.Transport == "ssh" {
				return Credential{Token: "https-token-must-not-cross-transport"}, nil
			}
			return Credential{}, ErrCredentialNotConfigured
		},
	})
	if err != nil {
		t.Fatal("failed to create Git client")
	}
	repositoryURL := strings.TrimSuffix(httpsServer.URL, "/") + "/team/private.git"
	probe, err := client.Probe(context.Background(), ProbeRequest{URL: repositoryURL, Ref: "main"})
	if err != nil {
		var operationError *OperationError
		if errors.As(err, &operationError) {
			t.Fatalf("SSH fallback probe failed: %v (%#v)", err, operationError.Diagnostic)
		}
		t.Fatalf("SSH fallback probe failed: %v", err)
	}
	if probe.Repository.Transport != "https" || probe.Diagnostic == nil || probe.Diagnostic.Transport != sshFallbackTransport {
		t.Fatalf("probe did not preserve the source URL and fallback diagnostic: %#v", probe)
	}
	if !strings.Contains(probe.Diagnostic.Recommendation, "local SSH config and key discovery") {
		t.Fatalf("fallback diagnostic did not identify the SSH credential source: %#v", probe.Diagnostic)
	}
	if probe.ResolvedCommit == "" {
		t.Fatal("fallback probe did not resolve the expected branch")
	}

	destination := filepath.Join(t.TempDir(), "checkout")
	clone, err := client.Clone(context.Background(), CloneRequest{URL: repositoryURL, Ref: "main", Destination: destination})
	if err != nil {
		t.Fatalf("SSH fallback clone failed: %v", err)
	}
	if clone.Repository.Transport != "https" {
		t.Fatalf("clone result transport = %q, want original HTTPS transport", clone.Repository.Transport)
	}
	if valid, _, err := client.ValidateCheckout(destination, repositoryURL, clone.Commit); err != nil || !valid {
		t.Fatalf("fallback checkout origin was not restored: valid=%t err=%v", valid, err)
	}
	if got := httpsRequests.Load(); got != 2 {
		t.Fatalf("HTTPS auth attempts = %d, want one per operation", got)
	}
}

func TestSSHFallbackURLDropsHTTPSPortAndPreservesPath(t *testing.T) {
	repository, err := ParseURL("https://git.example.test:8443/group/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	got, err := sshFallbackURL(repository)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ssh://git@git.example.test/group/repo.git"; got != want {
		t.Fatalf("SSH fallback URL = %q, want %q", got, want)
	}
}

type connectionCounterListener struct {
	net.Listener
	connections atomic.Int32
}

func (listener *connectionCounterListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err == nil {
		listener.connections.Add(1)
	}
	return connection, err
}
