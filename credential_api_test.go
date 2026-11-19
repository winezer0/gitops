package gitops

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCredentialAPIEndpointsForCommonProviders(t *testing.T) {
	for _, test := range []struct {
		name       string
		host       string
		hostName   string
		credential Credential
		endpoint   string
		header     string
		value      string
	}{
		{name: "GitHub.com", host: "github.com", hostName: "github.com", credential: Credential{Provider: ProviderGitHub, Token: "token"}, endpoint: "https://api.github.com/user", header: "Authorization", value: "Bearer token"},
		{name: "GitHub Enterprise", host: "git.enterprise.test:8443", hostName: "git.enterprise.test", credential: Credential{Provider: ProviderGitHub, Token: "token"}, endpoint: "https://git.enterprise.test:8443/api/v3/user", header: "Authorization", value: "Bearer token"},
		{name: "GitLab", host: "gitlab.example.test", hostName: "gitlab.example.test", credential: Credential{Provider: ProviderGitLab, Token: "token"}, endpoint: "https://gitlab.example.test/api/v4/user", header: "PRIVATE-TOKEN", value: "token"},
		{name: "Gitea", host: "gitea.example.test", hostName: "gitea.example.test", credential: Credential{Provider: ProviderGitea, Token: "token"}, endpoint: "https://gitea.example.test/api/v1/user", header: "Authorization", value: "token token"},
		{name: "Custom", host: "git.example.test", hostName: "git.example.test", credential: Credential{Provider: ProviderCustom, Token: "token", APIURL: "https://api.example.test/user"}, endpoint: "https://api.example.test/user", header: "Authorization", value: "Bearer token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint, header, value, err := credentialAPIEndpoint(test.host, test.hostName, test.credential)
			if err != nil {
				t.Fatal(err)
			}
			if endpoint != test.endpoint || header != test.header || value != test.value {
				t.Fatalf("credential API route = (%q, %q, %q), want (%q, %q, %q)", endpoint, header, value, test.endpoint, test.header, test.value)
			}
		})
	}
}

func TestTestHostCredentialAuthenticatesWithoutLeakingSecrets(t *testing.T) {
	const token = "private-gitops-credential-secret"
	var receivedHeader atomic.Value
	server := localCredentialAPIServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedHeader.Store(request.Header.Get("Authorization"))
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	var events []OperationEvent
	client, err := NewClient(Config{
		AllowedHosts: []string{strings.ToUpper(host)},
		CredentialFor: func(got string) (Credential, error) {
			if got != host {
				return Credential{}, ErrHostNotAllowed
			}
			return Credential{Provider: ProviderCustom, Token: token, APIURL: server.URL + "/user"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.TestHostCredential(context.Background(), CredentialTestRequest{Host: host, Timeout: 5 * time.Second, OnEvent: func(event OperationEvent) { events = append(events, event) }})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Authenticated || result.Diagnostic.Code != "git_credential_authenticated" {
		t.Fatalf("host credential result = %#v", result)
	}
	if got := receivedHeader.Load(); got != "Bearer "+token {
		t.Fatalf("provider API authorization = %v", got)
	}
	if strings.Contains(fmt.Sprint(result, events), token) {
		t.Fatal("credential result or operation events exposed the token")
	}
	if len(events) < 2 || events[len(events)-1].Kind != EventDiagnostic {
		t.Fatalf("credential operation events = %#v", events)
	}
}

func TestTestHostCredentialClassifiesScopeAndRedirectFailures(t *testing.T) {
	certificate, certificatePEM := newLocalGitCertificate(t)
	configureGitOpsHTTPSTransport(t, certificatePEM)
	var redirected atomic.Int32
	target := newLocalGitTLSServer(t, certificate, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	server := newLocalGitTLSServer(t, certificate, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/scope":
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"error":"insufficient_scope"}`))
		case "/redirect":
			http.Redirect(writer, request, target.URL+"/user", http.StatusFound)
		default:
			writer.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	client, err := NewClient(Config{
		AllowedHosts: []string{host},
		CredentialFor: func(string) (Credential, error) {
			return Credential{Provider: ProviderCustom, Token: "api-secret", APIURL: server.URL + "/scope"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := client.TestHostCredential(context.Background(), CredentialTestRequest{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Authenticated || scope.Diagnostic.Code != "git_credential_api_scope_insufficient" {
		t.Fatalf("insufficient-scope result = %#v", scope)
	}
	client.config.CredentialFor = func(string) (Credential, error) {
		return Credential{Provider: ProviderCustom, Token: "api-secret", APIURL: server.URL + "/redirect"}, nil
	}
	redirect, err := client.TestHostCredential(context.Background(), CredentialTestRequest{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if redirect.Authenticated || redirect.Diagnostic.Code != "git_credential_api_redirect" || redirected.Load() != 0 {
		t.Fatalf("redirect result = %#v, target requests = %d", redirect, redirected.Load())
	}
}

func TestTestHostCredentialRejectsMissingOrUnapprovedCredentialAPIHost(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	client, err := NewClient(Config{AllowedHosts: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	missing, err := client.TestHostCredential(context.Background(), CredentialTestRequest{Host: host})
	if err != nil || missing.Diagnostic.Code != "git_host_credential_not_configured" {
		t.Fatalf("missing credential result = %#v, %v", missing, err)
	}
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer target.Close()
	client.config.CredentialFor = func(string) (Credential, error) {
		return Credential{Provider: ProviderCustom, Token: "secret", APIURL: target.URL + "/user"}, nil
	}
	unapproved, err := client.TestHostCredential(context.Background(), CredentialTestRequest{Host: host})
	if err != nil || unapproved.Diagnostic.Code != "git_credential_api_host_not_allowed" {
		t.Fatalf("unapproved API host result = %#v, %v", unapproved, err)
	}
}

func TestTestHostCredentialTimeoutIsClassified(t *testing.T) {
	certificate, certificatePEM := newLocalGitCertificate(t)
	configureGitOpsHTTPSTransport(t, certificatePEM)
	server := newLocalGitTLSServer(t, certificate, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	client, err := NewClient(Config{
		AllowedHosts: []string{host},
		CredentialFor: func(string) (Credential, error) {
			return Credential{Provider: ProviderCustom, Token: "secret", APIURL: server.URL + "/user"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.TestHostCredential(context.Background(), CredentialTestRequest{Host: host, Timeout: 20 * time.Millisecond})
	if err == nil || result.Diagnostic.Code != "git_credential_api_timeout" {
		t.Fatalf("timeout result = %#v, %v", result, err)
	}
}

func localCredentialAPIServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	certificate, certificatePEM := newLocalGitCertificate(t)
	configureGitOpsHTTPSTransport(t, certificatePEM)
	return newLocalGitTLSServer(t, certificate, handler)
}

func configureGitOpsHTTPSTransport(t *testing.T, certificatePEM []byte) {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("failed to trust local GitOps test certificate")
	}
	previous := http.DefaultTransport
	transport := previous.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = previous
		transport.CloseIdleConnections()
	})
}
