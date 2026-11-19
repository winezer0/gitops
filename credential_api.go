package gitops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/winezer0/processkit/processrunner"
)

const credentialResponseLimit = 16 << 10

func (client *Client) testHostCredential(ctx context.Context, request CredentialTestRequest) (result CredentialTestResult, returnErr error) {
	started := time.Now()
	timeout := client.requestTimeout(request.Timeout)
	if request.Timeout < 0 {
		return CredentialTestResult{}, operationError(fmt.Errorf("credential test timeout must not be negative"))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	host, hostName, port, err := parseCredentialHost(request.Host)
	transport := "https"
	finish := func(authenticated bool, code, phase, message, recommendation string, retryable bool) CredentialTestResult {
		value := CredentialTestResult{
			Authenticated: authenticated,
			CheckedAt:     time.Now().UTC(),
			Diagnostic: Diagnostic{
				Code: code, Phase: phase, Message: message, Recommendation: recommendation,
				Host: host, Transport: transport, Duration: time.Since(started), Timeout: timeout, Retryable: retryable,
			},
		}
		emitProgress(request.OnEvent, OperationEvent{Kind: EventDiagnostic, Operation: "credential_test", Diagnostic: &value.Diagnostic})
		return value
	}
	if ctx.Err() != nil {
		code, message := "git_credential_canceled", "Credential test was canceled."
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code, message = "git_credential_timeout", "The credential test did not finish before the timeout."
		}
		return finish(false, code, "host_validation", message, "Retry the credential test if it was canceled unintentionally.", true), ctx.Err()
	}
	if err != nil {
		return finish(false, "invalid_git_host", "host_validation", "Git host is invalid.", "Use a bare hostname or host:port that is present in the saved host credentials.", false), nil
	}
	if err := client.allowHost(host); err != nil {
		return finish(false, "source_host_not_allowed", "host_policy", "Git host is not allowed.", "Add the host to the Git allowlist and save a host-specific credential.", false), nil
	}
	if client.config.ValidateHost != nil {
		if err := client.config.ValidateHost(ctx, host); err != nil {
			if ctx.Err() != nil {
				return finish(false, "git_credential_canceled", "host_validation", "Git host validation was canceled.", "Retry the credential test if it was canceled unintentionally.", false), ctx.Err()
			}
			return finish(false, "git_host_validation_failed", "host_validation", "Git host failed the server address safety check.", "Check the host allowlist, DNS result, and SSRF protection settings.", false), nil
		}
	}
	if client.config.CredentialFor == nil {
		return finish(false, "git_host_credential_not_configured", "credential_lookup", "No host-specific Git credential is configured.", "Save a credential for this exact Git host before testing it.", false), nil
	}
	credential, err := client.config.CredentialFor(host)
	if err != nil {
		if errors.Is(err, ErrCredentialNotConfigured) {
			return finish(false, "git_host_credential_not_configured", "credential_lookup", "No host-specific Git credential is configured.", "Save a credential for this exact Git host before testing it.", false), nil
		}
		return finish(false, "git_credential_lookup_failed", "credential_lookup", "The saved Git credential could not be loaded.", "Check the configured credential source and retry.", false), nil
	}
	if len(credential.SSHPrivateKey) > 0 {
		transport = "ssh"
		if credential.Token != "" || credential.Password != "" {
			return finish(false, "git_host_credential_invalid", "credential_lookup", "The saved credential contains conflicting HTTPS and SSH authentication data.", "Choose one credential type for this host and save it again.", false), nil
		}
		return client.testSSHCredential(ctx, request, host, hostName, port, credential, timeout, finish)
	}
	if credential.Token == "" && credential.Password == "" {
		return finish(false, "git_host_credential_empty", "credential_lookup", "The saved Git credential has no usable secret.", "Update the host credential and save it before running the test.", false), nil
	}
	return client.testHTTPSCredential(ctx, request, host, hostName, credential, timeout, finish)
}

func (client *Client) testHTTPSCredential(ctx context.Context, request CredentialTestRequest, host, hostName string, credential Credential, timeout time.Duration, finish func(bool, string, string, string, string, bool) CredentialTestResult) (CredentialTestResult, error) {
	endpoint, header, value, err := credentialAPIEndpoint(host, hostName, credential)
	if err != nil {
		return finish(false, "git_credential_api_not_configured", "api_configuration", err.Error(), "Choose the Git service provider or enter its HTTPS user API endpoint, then save the settings.", false), nil
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Hostname() == "" {
		return finish(false, "git_credential_api_invalid", "api_configuration", "The configured Git credential API endpoint is invalid.", "Enter a valid HTTPS API endpoint without embedded credentials, query parameters, or fragments.", false), nil
	}
	apiHost := normalizedURLHost(parsedEndpoint)
	githubPublicAPI := credential.Provider == ProviderGitHub && hostName == "github.com" && normalizeHost(parsedEndpoint.Hostname()) == "api.github.com"
	if !githubPublicAPI && !sameHost(host, apiHost) {
		if err := client.allowHost(apiHost); err != nil {
			return finish(false, "git_credential_api_host_not_allowed", "api_host_policy", "The credential API host is not in the Git host allowlist.", "Allowlist the HTTPS API host before sending credentials to it.", false), nil
		}
	}
	if client.config.ValidateHost != nil {
		if err := client.config.ValidateHost(ctx, apiHost); err != nil {
			if ctx.Err() != nil {
				return finish(false, "git_credential_canceled", "api_host_validation", "Credential API validation was canceled.", "Retry the credential test if it was canceled unintentionally.", false), ctx.Err()
			}
			return finish(false, "git_credential_api_host_invalid", "api_host_validation", "Credential API host failed the server address safety check.", "Check the API host DNS result, allowlist, and SSRF protection settings.", false), nil
		}
	}
	proxyURL, err := client.proxy(ctx, "https")
	if err != nil {
		return finish(false, "git_proxy_failed", "proxy_connect", "The configured outbound proxy could not be loaded.", "Check the Git proxy settings and retry the credential test.", true), nil
	}
	transport, err := credentialHTTPTransport(proxyURL)
	if err != nil {
		return finish(false, "git_proxy_failed", "proxy_connect", "The configured outbound proxy could not be initialized.", "Check the Git proxy URL and retry the credential test.", false), nil
	}
	clientHTTP := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer clientHTTP.CloseIdleConnections()
	emitProgress(request.OnEvent, OperationEvent{Kind: EventProgress, Operation: "credential_test", Phase: "api_request"})
	apiRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return finish(false, "git_credential_api_invalid", "api_configuration", "The configured Git credential API endpoint is invalid.", "Enter a valid HTTPS API endpoint.", false), nil
	}
	if credential.Token != "" {
		apiRequest.Header.Set(header, value)
	} else {
		username := credential.Username
		if username == "" {
			username = "git"
		}
		apiRequest.SetBasicAuth(username, credential.Password)
	}
	response, err := clientHTTP.Do(apiRequest)
	if err != nil {
		if ctx.Err() != nil {
			code, message := "git_credential_canceled", "Credential API request was canceled."
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				code, message = "git_credential_api_timeout", "The credential API did not respond before the timeout."
			}
			return finish(false, code, "api_request", message, "Check the Git host, proxy, and probe timeout, then retry.", true), ctx.Err()
		}
		if networkError, ok := err.(net.Error); ok && networkError.Timeout() {
			return finish(false, "git_credential_api_timeout", "api_request", "The credential API did not respond before the timeout.", "Check the Git host, proxy, and probe timeout, then retry.", true), nil
		}
		return finish(false, "git_credential_api_unreachable", "api_request", "The credential API could not be reached.", "Check the API endpoint, TLS certificate, DNS, firewall, and Git proxy settings.", true), nil
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, credentialResponseLimit))
	switch {
	case response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices:
		return finish(true, "git_credential_authenticated", "authentication", "The Git host accepted the saved HTTPS credential.", "", false), nil
	case response.StatusCode == http.StatusUnauthorized:
		message := "The provider user API did not authorize this credential request."
		recommendation := "Check credential validity, expiration, provider selection, and host mapping. Use repository probing to verify repository access."
		if credential.Provider == ProviderGitLab {
			message = "The GitLab user API did not authorize this token request; it may be invalid, expired, or missing the required API scope."
			recommendation = "GitLab's /api/v4/user endpoint requires read_user or api. A read_repository-only token may still read repositories; use repository probing to verify repository access."
		}
		return finish(false, "git_credential_api_denied", "authentication", message, recommendation, false), nil
	case response.StatusCode == http.StatusForbidden:
		if strings.Contains(strings.ToLower(string(responseBody)), "insufficient_scope") || strings.Contains(strings.ToLower(response.Header.Get("WWW-Authenticate")), "insufficient_scope") {
			recommendation := "Authentication succeeded. This test does not verify repository access; use repository probing to check it."
			if credential.Provider == ProviderGitLab {
				recommendation = "Authentication succeeded. GitLab's /api/v4/user needs read_user or api to read the user profile. Use repository probing to verify repository access."
			}
			return finish(true, "git_credential_api_scope_insufficient", "authentication", "The provider API accepted the token but denied access to the user profile because a required scope is missing.", recommendation, false), nil
		}
		return finish(false, "git_credential_api_forbidden", "authorization", "The provider API denied this request; this does not establish whether repository access is allowed.", "Check provider API permissions, then use repository probing to verify repository access.", false), nil
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return finish(false, "git_credential_api_redirect", "api_request", "The credential API redirected the request; the token was not forwarded.", "Set the final HTTPS user API endpoint directly.", false), nil
	default:
		return finish(false, "git_credential_api_error", "api_request", fmt.Sprintf("The credential API returned HTTP %d.", response.StatusCode), "Check the provider API endpoint and service availability.", response.StatusCode >= 500), nil
	}
}

func credentialAPIEndpoint(host, hostName string, credential Credential) (string, string, string, error) {
	endpoint := strings.TrimSpace(credential.APIURL)
	if endpoint == "" {
		switch credential.Provider {
		case ProviderGitHub:
			if hostName == "github.com" {
				endpoint = "https://api.github.com/user"
			} else {
				endpoint = "https://" + host + "/api/v3/user"
			}
		case ProviderGitLab:
			endpoint = "https://" + host + "/api/v4/user"
		case ProviderGitea:
			endpoint = "https://" + host + "/api/v1/user"
		case ProviderCustom:
			return "", "", "", errors.New("custom API provider requires an endpoint")
		default:
			return "", "", "", errors.New("HTTPS token provider is not configured")
		}
	}
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", "", errors.New("HTTPS API endpoint must not contain credentials, query parameters, or fragments")
	}
	header, value := "Authorization", "Bearer "+credential.Token
	switch credential.Provider {
	case ProviderGitLab:
		header, value = "PRIVATE-TOKEN", credential.Token
	case ProviderGitea:
		value = "token " + credential.Token
	case ProviderGitHub, ProviderCustom:
	default:
		return "", "", "", errors.New("HTTPS token provider is not configured")
	}
	return endpoint, header, value, nil
}

func (client *Client) testSSHCredential(ctx context.Context, request CredentialTestRequest, host, hostName, port string, credential Credential, timeout time.Duration, finish func(bool, string, string, string, string, bool) CredentialTestResult) (CredentialTestResult, error) {
	directory, err := os.MkdirTemp("", "gitops-ssh-probe-")
	if err != nil {
		return finish(false, "git_credential_setup_failed", "credential_setup", "The SSH key could not be prepared for the credential test.", "Check server disk permissions and retry.", false), nil
	}
	defer os.RemoveAll(directory)
	keyPath := filepath.Join(directory, "id_key")
	if err := os.WriteFile(keyPath, credential.SSHPrivateKey, 0o600); err != nil {
		return finish(false, "git_credential_setup_failed", "credential_setup", "The SSH key could not be prepared for the credential test.", "Check server disk permissions and retry.", false), nil
	}
	username := strings.TrimSpace(credential.Username)
	if username == "" {
		username = "git"
	}
	connectSeconds := max(1, int(timeout.Seconds()))
	if connectSeconds > 60 {
		connectSeconds = 60
	}
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=" + strconv.Itoa(connectSeconds), "-i", keyPath}
	if port != "" {
		args = append(args, "-p", port)
	}
	if strings.Contains(hostName, ":") {
		hostName = "[" + hostName + "]"
	}
	args = append(args, "--", username+"@"+hostName)
	emitProgress(request.OnEvent, OperationEvent{Kind: EventProgress, Operation: "credential_test", Phase: "ssh_handshake"})
	binary := client.config.SSHBinary
	if binary == "" {
		binary = "ssh"
	}
	result, err := (processrunner.ProcessRunner{}).Run(ctx, processrunner.Invocation{
		Name: "ssh credential test", Binary: binary, Args: args,
		Environment: map[string]string{"GIT_TERMINAL_PROMPT": "0"}, CaptureBytes: credentialResponseLimit, RequireCompleteOutput: true,
	}, processrunner.RunOptions{})
	emitProcessEvent(request.OnEvent, "credential_test", "openssh", result)
	if errors.Is(err, exec.ErrNotFound) {
		return finish(false, "git_ssh_client_unavailable", "credential_setup", "OpenSSH is not available on the Git client host.", "Install OpenSSH or configure the credential as an HTTPS token.", false), nil
	}
	output := result.Output
	if err == nil || isSuccessfulSSHHandshake(output) {
		return finish(true, "git_credential_authenticated", "authentication", "The Git host accepted the saved SSH key.", "", false), nil
	}
	if ctx.Err() != nil {
		code, message := "git_credential_canceled", "SSH credential test was canceled."
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code, message = "git_credential_timeout", "The SSH host did not respond before the timeout."
		}
		return finish(false, code, "authentication", message, "Check network routes, firewall rules, VPN access, and the probe timeout.", true), ctx.Err()
	}
	diagnostic := diagnosticFromCommandOutput(output, err)
	if diagnostic.Code == "credential_rejected" {
		diagnostic.Code = "git_credential_rejected"
	}
	return finish(false, diagnostic.Code, diagnostic.Phase, diagnostic.Message, diagnostic.Recommendation, diagnostic.Retryable), nil
}

func parseCredentialHost(raw string) (host, hostname, port string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "/\\?#@ \t\r\n") || strings.Contains(raw, "://") {
		return "", "", "", errors.New("invalid Git host")
	}
	parsed, err := url.Parse("https://" + raw)
	if err != nil || parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", "", "", errors.New("invalid Git host")
	}
	port = parsed.Port()
	if port != "" {
		value, parseErr := strconv.Atoi(port)
		if parseErr != nil || value < 1 || value > 65535 {
			return "", "", "", errors.New("invalid Git host port")
		}
	}
	hostname = normalizeHost(parsed.Hostname())
	host = hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}
	return host, hostname, port, nil
}

func normalizedURLHost(parsed *url.URL) string {
	host := normalizeHost(parsed.Hostname())
	if parsed.Port() != "" {
		host = net.JoinHostPort(host, parsed.Port())
	}
	return host
}

func sameHost(left, right string) bool {
	return strings.EqualFold(normalizeHost(left), normalizeHost(right))
}

func credentialHTTPTransport(proxyURL string) (*http.Transport, error) {
	result := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(proxyURL) == "" {
		return result, nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("invalid proxy URL")
	}
	result.Proxy = http.ProxyURL(parsed)
	return result, nil
}

func isSuccessfulSSHHandshake(output []byte) bool {
	text := strings.ToLower(string(output))
	for _, marker := range []string{"successfully authenticated", "welcome to gitlab", "authenticated via ssh key"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
