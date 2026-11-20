package gitops

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPSRedirectTargetIsValidatedBeforeRequest(t *testing.T) {
	for _, test := range []struct {
		name             string
		allowTarget      bool
		denyTarget       bool
		wantTargetCalled bool
	}{
		{name: "allowlist", wantTargetCalled: false},
		{name: "host validator", allowTarget: true, denyTarget: true, wantTargetCalled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			disableHTTPProxyEnvironment(t)
			var targetRequests atomic.Int32
			target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				targetRequests.Add(1)
			}))
			defer target.Close()

			var targetValidated atomic.Bool
			origin := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				http.Redirect(writer, request, target.URL+request.URL.RequestURI(), http.StatusFound)
			}))
			defer origin.Close()

			repositoryURL := strings.TrimSuffix(origin.URL, "/") + "/team/repo.git"
			repository, err := ParseURL(repositoryURL)
			if err != nil {
				t.Fatal(err)
			}
			allowedHosts := []string{repository.Host}
			if test.allowTarget {
				targetURL := strings.TrimPrefix(target.URL, "https://")
				allowedHosts = append(allowedHosts, targetURL)
			}
			client, err := NewClient(Config{
				AllowedHosts: allowedHosts,
				CABundle:     testCABundle(t, origin, target),
				ValidateHost: func(_ context.Context, host string) error {
					if host != repository.Host {
						targetValidated.Store(true)
						if test.denyTarget {
							return ErrHostNotAllowed
						}
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			_, err = client.Probe(context.Background(), ProbeRequest{
				URL: repositoryURL, Credential: &Credential{Username: "git", Token: "redirect-secret"},
			})
			if !errors.Is(err, ErrHostNotAllowed) {
				t.Fatalf("redirect probe error = %v, want ErrHostNotAllowed", err)
			}
			if got := targetRequests.Load(); got != 0 {
				t.Fatalf("redirect target received %d requests before rejection", got)
			}
			if got := targetValidated.Load(); got != test.wantTargetCalled {
				t.Fatalf("redirect target validation called = %v, want %v", got, test.wantTargetCalled)
			}
		})
	}
}

func TestHTTPSCrossOriginRedirectDoesNotForwardCredential(t *testing.T) {
	disableHTTPProxyEnvironment(t)
	const token = "cross-origin-secret"
	var originReceivedCredential atomic.Bool
	var targetReceivedCredential atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, value, ok := request.BasicAuth()
		targetReceivedCredential.Store(ok && value == token)
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer target.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, value, ok := request.BasicAuth()
		originReceivedCredential.Store(ok && value == token)
		http.Redirect(writer, request, target.URL+request.URL.RequestURI(), http.StatusFound)
	}))
	defer origin.Close()

	repositoryURL := strings.TrimSuffix(origin.URL, "/") + "/team/repo.git"
	repository, err := ParseURL(repositoryURL)
	if err != nil {
		t.Fatal(err)
	}
	targetHost := strings.TrimPrefix(target.URL, "https://")
	client, err := NewClient(Config{
		AllowedHosts: []string{repository.Host, targetHost},
		CABundle:     testCABundle(t, origin, target),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Probe(context.Background(), ProbeRequest{
		URL: repositoryURL, Credential: &Credential{Username: "git", Token: token},
	})
	if !originReceivedCredential.Load() {
		t.Fatal("origin did not receive the configured credential")
	}
	if targetReceivedCredential.Load() {
		t.Fatal("cross-origin redirect forwarded the configured credential")
	}
}

func testCABundle(t *testing.T, servers ...*httptest.Server) []byte {
	t.Helper()
	var bundle []byte
	for _, server := range servers {
		certificate, err := x509.ParseCertificate(server.Certificate().Raw)
		if err != nil {
			t.Fatal(err)
		}
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})...)
	}
	return bundle
}
