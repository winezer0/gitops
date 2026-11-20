package gitops

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthenticationFailureIsNotRetried(t *testing.T) {
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
		AllowedHosts: []string{repository.Host},
		Timeout:      10 * time.Second,
		CABundle:     certificatePEM,
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
