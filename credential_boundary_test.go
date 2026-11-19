package gitops

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPSRedirectDoesNotForwardCredentialsToAnotherHost(t *testing.T) {
	const username = "redirect-test-user"
	const token = "redirect-test-secret"
	var targetRequests atomic.Int32
	var originReceivedCredential atomic.Bool
	var targetReceivedCredential atomic.Bool
	certificate, certificatePEM := newLocalGitCertificate(t)
	target := newLocalGitTLSServer(t, certificate, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		targetRequests.Add(1)
		requestUsername, requestToken, ok := request.BasicAuth()
		targetReceivedCredential.Store(ok && requestUsername == username && requestToken == token)
		writer.Header().Set("WWW-Authenticate", `Basic realm="redirect-target"`)
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer target.Close()

	redirectTarget, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal("failed to parse local redirect target")
	}
	redirectTarget.Host = strings.Replace(redirectTarget.Host, "127.0.0.1", "localhost", 1)
	redirectTarget.Path = "/team/private.git/info/refs"
	redirectTarget.RawQuery = "service=git-upload-pack"

	origin := newLocalGitTLSServer(t, certificate, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestUsername, requestToken, ok := request.BasicAuth()
		originReceivedCredential.Store(ok && requestUsername == username && requestToken == token)
		http.Redirect(writer, request, redirectTarget.String(), http.StatusFound)
	}))
	defer origin.Close()
	configureLocalGitHTTPS(t, certificatePEM)

	url := strings.TrimSuffix(origin.URL, "/") + "/team/private.git"
	repository, err := ParseURL(url)
	if err != nil {
		t.Fatal("failed to parse local source repository URL")
	}
	credentialCalls := atomic.Int32{}
	client, err := NewClient(Config{
		AllowedHosts: []string{repository.Host},
		Timeout:      10 * time.Second,
		CredentialFor: func(host string) (Credential, error) {
			credentialCalls.Add(1)
			if host != repository.Host {
				return Credential{}, ErrHostNotAllowed
			}
			return Credential{Username: username, Token: token}, nil
		},
	})
	if err != nil {
		t.Fatal("failed to create local redirect test client")
	}
	_, probeErr := client.Probe(context.Background(), ProbeRequest{URL: url})
	if ErrorCode(probeErr) != string(ErrorAuthentication) {
		t.Fatalf("redirect probe error code = %s, want %s (origin credential=%v, target requests=%d, target credential=%v)", ErrorCode(probeErr), ErrorAuthentication, originReceivedCredential.Load(), targetRequests.Load(), targetReceivedCredential.Load())
	}
	if originReceivedCredential.Load() == false {
		t.Fatal("the source repository did not receive its configured credential")
	}
	if targetRequests.Load() == 0 {
		t.Fatal("Git did not follow the cross-host repository redirect")
	}
	if targetReceivedCredential.Load() {
		t.Fatal("the source repository credential was forwarded to the redirect host")
	}
	if credentialCalls.Load() != 1 {
		t.Fatalf("credential callback count = %d, want 1 for the validated source host", credentialCalls.Load())
	}
	if strings.Contains(probeErr.Error(), token) {
		t.Fatal("redirect failure exposed the source credential")
	}
}

func TestCredentialRoutingUsesNormalizedAllowedHost(t *testing.T) {
	var callbackCalls atomic.Int32
	client, err := NewClient(Config{
		AllowedHosts: []string{"GIT.EXAMPLE.TEST"},
		CredentialFor: func(host string) (Credential, error) {
			callbackCalls.Add(1)
			if host != "git.example.test" {
				return Credential{}, ErrHostNotAllowed
			}
			return Credential{Username: "custom-user", Token: "custom-token"}, nil
		},
	})
	if err != nil {
		t.Fatal("failed to create credential routing test client")
	}
	_, credential, err := client.prepare(context.Background(), "https://Git.Example.Test/team/repo.git")
	if err != nil {
		t.Fatalf("allowed normalized host was rejected: %v", err)
	}
	if credential.Username != "custom-user" || credential.Token != "custom-token" {
		t.Fatal("custom credentials were not returned for the normalized allowed host")
	}
	if callbackCalls.Load() != 1 {
		t.Fatalf("credential callback count = %d, want 1", callbackCalls.Load())
	}

	_, _, err = client.prepare(context.Background(), "https://attacker.example.test/team/repo.git")
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("unlisted host error = %v, want host denied", err)
	}
	if callbackCalls.Load() != 1 {
		t.Fatal("credential callback was invoked for an unlisted host")
	}
}

func newLocalGitTLSServer(t *testing.T, certificate tls.Certificate, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	server.StartTLS()
	return server
}

func newLocalGitCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("failed to create a local TLS key")
	}
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal("failed to create a local TLS certificate serial")
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{CommonName: "gitops local test"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal("failed to create a local TLS certificate")
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal("failed to encode a local TLS key")
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		t.Fatal("failed to load the local TLS key pair")
	}
	return certificate, certificatePEM
}

func configureLocalGitHTTPS(t *testing.T, certificatePEM []byte) {
	t.Helper()
	certificatePath := filepath.Join(t.TempDir(), "test-ca.pem")
	if err := os.WriteFile(certificatePath, certificatePEM, 0o600); err != nil {
		t.Fatal("failed to write temporary local Git CA bundle")
	}
	globalConfigPath := filepath.Join(t.TempDir(), "empty-git-config")
	if err := os.WriteFile(globalConfigPath, nil, 0o600); err != nil {
		t.Fatal("failed to write empty Git global config")
	}
	t.Setenv("GIT_SSL_CAINFO", certificatePath)
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfigPath)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("all_proxy", "")
}
