package gitops

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const credentialResponseLimit = 16 << 10

type credentialTestFinisher func(CredentialStatus, ProfileAccessStatus, string, string, string, string, bool, error) (CredentialTestResult, error)

func credentialTestSucceeded(credentialStatus CredentialStatus, profileAccess ProfileAccessStatus) bool {
	return credentialStatus == CredentialStatusAccepted &&
		(profileAccess == ProfileAccessAllowed || profileAccess == ProfileAccessNotChecked)
}

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
	finish := func(credentialStatus CredentialStatus, profileAccess ProfileAccessStatus, code, phase, message, recommendation string, retryable bool, cause error) (CredentialTestResult, error) {
		value := CredentialTestResult{
			CredentialStatus: credentialStatus,
			ProfileAccess:    profileAccess,
			CheckedAt:        time.Now().UTC(),
			Diagnostic: Diagnostic{
				Code: code, Phase: phase, Message: message, Recommendation: recommendation,
				Host: host, Transport: transport, Duration: time.Since(started), Timeout: timeout, Retryable: retryable,
			},
		}
		emitProgress(request.OnEvent, OperationEvent{Kind: EventDiagnostic, Operation: "credential_test", Diagnostic: &value.Diagnostic})
		if credentialTestSucceeded(credentialStatus, profileAccess) {
			return value, nil
		}
		return value, &CredentialTestError{Diagnostic: value.Diagnostic, cause: cause}
	}
	if ctx.Err() != nil {
		code, message := "git_credential_canceled", "Credential test was canceled."
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code, message = "git_credential_timeout", "The credential test did not finish before the timeout."
		}
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, code, "host_validation", message, "Retry the credential test if it was canceled unintentionally.", true, ctx.Err())
	}
	if err != nil {
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "invalid_git_host", "host_validation", "Git host is invalid.", "Use a bare hostname or host:port that is present in the saved host credentials.", false, err)
	}
	if err := client.allowHost(host); err != nil {
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "source_host_not_allowed", "host_policy", "Git host is not allowed.", "Add the host to the Git allowlist and save a host-specific credential.", false, err)
	}
	if client.config.ValidateHost != nil {
		if err := client.config.ValidateHost(ctx, host); err != nil {
			if ctx.Err() != nil {
				return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_canceled", "host_validation", "Git host validation was canceled.", "Retry the credential test if it was canceled unintentionally.", false, ctx.Err())
			}
			return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_host_validation_failed", "host_validation", "Git host failed the server address safety check.", "Check the host allowlist, DNS result, and SSRF protection settings.", false, err)
		}
	}
	if request.Credential == nil && client.config.CredentialFor == nil {
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_host_credential_not_configured", "credential_lookup", "No host-specific Git credential is configured.", "Save a credential for this exact Git host before testing it.", false, ErrCredentialNotConfigured)
	}
	var credential Credential
	if request.Credential != nil {
		credential = *request.Credential
	} else {
		credential, err = client.config.CredentialFor(host)
		if err != nil {
			if errors.Is(err, ErrCredentialNotConfigured) {
				return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_host_credential_not_configured", "credential_lookup", "No host-specific Git credential is configured.", "Save a credential for this exact Git host before testing it.", false, err)
			}
			return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_lookup_failed", "credential_lookup", "The saved Git credential could not be loaded.", "Check the configured credential source and retry.", false, err)
		}
	}
	if len(credential.SSHPrivateKey) > 0 || credential.SSHPrivateKeyPath != "" {
		transport = "ssh"
		if credential.Token != "" || credential.Password != "" {
			return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_host_credential_invalid", "credential_lookup", "The saved credential contains conflicting HTTPS and SSH authentication data.", "Choose one credential type for this host and save it again.", false, nil)
		}
		return client.testSSHCredential(ctx, request, host, hostName, port, credential, timeout, finish)
	}
	if credential.Token == "" && credential.Password == "" {
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_host_credential_empty", "credential_lookup", "The saved Git credential has no usable secret.", "Update the host credential and save it before running the test.", false, nil)
	}
	return client.testHTTPSCredential(ctx, request, host, hostName, credential, timeout, finish)
}

func (client *Client) testHTTPSCredential(ctx context.Context, request CredentialTestRequest, host, hostName string, credential Credential, timeout time.Duration, finish credentialTestFinisher) (CredentialTestResult, error) {
	endpoint, header, value, err := credentialAPIEndpoint(host, hostName, credential)
	if err != nil {
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_api_not_configured", "api_configuration", err.Error(), "Choose the Git service provider or enter its HTTPS user API endpoint, then save the settings.", false, err)
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Hostname() == "" {
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_api_invalid", "api_configuration", "The configured Git credential API endpoint is invalid.", "Enter a valid HTTPS API endpoint without embedded credentials, query parameters, or fragments.", false, err)
	}
	apiHost := normalizedURLHost(parsedEndpoint)
	githubPublicAPI := credential.Provider == ProviderGitHub && hostName == "github.com" && normalizeHost(parsedEndpoint.Hostname()) == "api.github.com"
	if !githubPublicAPI && !sameHost(host, apiHost) {
		if err := client.allowHost(apiHost); err != nil {
			return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_api_host_not_allowed", "api_host_policy", "The credential API host is not in the Git host allowlist.", "Allowlist the HTTPS API host before sending credentials to it.", false, err)
		}
	}
	if client.config.ValidateHost != nil {
		if err := client.config.ValidateHost(ctx, apiHost); err != nil {
			if ctx.Err() != nil {
				return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_canceled", "api_host_validation", "Credential API validation was canceled.", "Retry the credential test if it was canceled unintentionally.", false, ctx.Err())
			}
			return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_api_host_invalid", "api_host_validation", "Credential API host failed the server address safety check.", "Check the API host DNS result, allowlist, and SSRF protection settings.", false, err)
		}
	}
	proxyURL, err := client.proxy(ctx, "https")
	if err != nil {
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_proxy_failed", "proxy_connect", "The configured outbound proxy could not be loaded.", "Check the Git proxy settings and retry the credential test.", true, err)
	}
	transport, err := credentialHTTPTransport(proxyURL, client.config.CABundle)
	if err != nil {
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_proxy_failed", "proxy_connect", "The configured outbound proxy could not be initialized.", "Check the Git proxy URL and retry the credential test.", false, err)
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
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_api_invalid", "api_configuration", "The configured Git credential API endpoint is invalid.", "Enter a valid HTTPS API endpoint.", false, err)
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
			return finish(CredentialStatusUnknown, ProfileAccessNotChecked, code, "api_request", message, "Check the Git host, proxy, and probe timeout, then retry.", true, ctx.Err())
		}
		if networkError, ok := err.(net.Error); ok && networkError.Timeout() {
			return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_api_timeout", "api_request", "The credential API did not respond before the timeout.", "Check the Git host, proxy, and probe timeout, then retry.", true, err)
		}
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_api_unreachable", "api_request", "The credential API could not be reached.", "Check the API endpoint, TLS certificate, DNS, firewall, and Git proxy settings.", true, err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, credentialResponseLimit))
	switch {
	case response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices:
		return finish(CredentialStatusAccepted, ProfileAccessAllowed, "git_credential_authenticated", "authentication", "The Git host accepted the saved HTTPS credential and allowed access to the user profile.", "", false, nil)
	case response.StatusCode == http.StatusUnauthorized:
		message := "The provider user API did not authorize this credential request."
		recommendation := "Check credential validity, expiration, provider selection, and host mapping. Use repository probing to verify repository access."
		if credential.Provider == ProviderGitLab {
			message = "The GitLab user API did not authorize this token request; the token may be invalid, expired, or missing the API scope required to read the user profile."
			recommendation = "GitLab's /api/v4/user endpoint requires read_user or api. A read_repository-only token may still read repositories; use repository probing to verify repository access."
		}
		return finish(CredentialStatusUnknown, ProfileAccessDenied, "git_credential_api_denied", "authentication", message, recommendation, false, nil)
	case response.StatusCode == http.StatusForbidden:
		if strings.Contains(strings.ToLower(string(responseBody)), "insufficient_scope") || strings.Contains(strings.ToLower(response.Header.Get("WWW-Authenticate")), "insufficient_scope") {
			recommendation := "The credential was accepted, but this test does not verify repository access; use repository probing to check it."
			if credential.Provider == ProviderGitLab {
				recommendation = "The credential was accepted. GitLab's /api/v4/user needs read_user or api to read the user profile. Use repository probing to verify repository access."
			}
			return finish(CredentialStatusAccepted, ProfileAccessInsufficientScope, "git_credential_api_scope_insufficient", "authentication", "The provider API accepted the token but denied access to the user profile because a required scope is missing.", recommendation, false, nil)
		}
		return finish(CredentialStatusUnknown, ProfileAccessDenied, "git_credential_api_forbidden", "authorization", "The provider API denied this request; this does not establish whether repository access is allowed.", "Check provider API permissions, then use repository probing to verify repository access.", false, nil)
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_api_redirect", "api_request", "The credential API redirected the request; the token was not forwarded.", "Set the final HTTPS user API endpoint directly.", false, nil)
	default:
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_api_error", "api_request", fmt.Sprintf("The credential API returned HTTP %d.", response.StatusCode), "Check the provider API endpoint and service availability.", response.StatusCode >= 500, nil)
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

func (client *Client) testSSHCredential(ctx context.Context, request CredentialTestRequest, host, hostName, port string, credential Credential, timeout time.Duration, finish credentialTestFinisher) (CredentialTestResult, error) {
	username := strings.TrimSpace(credential.Username)
	if username == "" {
		username = "git"
	}
	sshURL := (&url.URL{Scheme: "ssh", User: url.User(username), Host: net.JoinHostPort(hostName, portOrDefault(port)), Path: "/"}).String()
	repository := Repository{URL: sshURL, Host: host, Transport: "ssh"}
	auth, err := authMethod(repository, credential, client.config, timeout)
	if err != nil {
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_setup_failed", "credential_setup", "The SSH credential or host-key trust could not be prepared.", "Check the private key, passphrase, and SSH host-key trust settings.", false, err)
	}
	publicKeys, ok := auth.(*goGitSSHAuth)
	if !ok {
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_credential_setup_failed", "credential_setup", "The SSH credential could not be prepared.", "Provide an SSH private key for this host.", false, nil)
	}
	sshConfig := publicKeys.clientConfig
	if port == "" {
		port = "22"
	}
	address := net.JoinHostPort(hostName, port)
	emitProgress(request.OnEvent, OperationEvent{Kind: EventProgress, Operation: "credential_test", Phase: "ssh_handshake"})
	dialer := net.Dialer{Timeout: timeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return finishSSHCredentialFailure(ctx, err, finish)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.SetDeadline(time.Now()) })
	defer stopCancel()
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, address, sshConfig)
	if err == nil {
		sshClient := ssh.NewClient(clientConnection, channels, requests)
		_ = sshClient.Close()
		return finish(CredentialStatusAccepted, ProfileAccessNotChecked, "git_credential_authenticated", "authentication", "The Git host accepted the saved SSH key.", "", false, nil)
	}
	return finishSSHCredentialFailure(ctx, classifySSHHostKeyError(err), finish)
}

func finishSSHCredentialFailure(ctx context.Context, err error, finish credentialTestFinisher) (CredentialTestResult, error) {
	if ctx.Err() != nil {
		code, message := "git_credential_canceled", "SSH credential test was canceled."
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code, message = "git_credential_timeout", "The SSH host did not respond before the timeout."
		}
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, code, "authentication", message, "Check network routes, firewall rules, VPN access, and the probe timeout.", true, ctx.Err())
	}
	switch {
	case errors.Is(err, ErrSSHHostKeyTrustNotConfigured):
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_ssh_host_key_trust_not_configured", "host_key_validation", "No SSH host-key trust source is configured.", "Configure trusted known_hosts files or an application host-key callback.", false, err)
	case errors.Is(err, ErrSSHHostKeyChanged):
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_ssh_host_key_changed", "host_key_validation", "The SSH host key differs from the trusted key.", "Verify the host key out of band before updating trusted keys.", false, err)
	case errors.Is(err, ErrSSHHostKeyNotTrusted):
		return finish(CredentialStatusUnknown, ProfileAccessNotChecked, "git_ssh_host_key_unknown", "host_key_validation", "The SSH host key is not trusted.", "Verify and add the host key to the configured trust source.", false, err)
	default:
		diagnostic := diagnosticForError(err, "authentication", Repository{}, "", 0, 0)
		credentialStatus := CredentialStatusUnknown
		if ErrorCode(err) == string(ErrorAuthentication) {
			credentialStatus = CredentialStatusRejected
		}
		return finish(credentialStatus, ProfileAccessNotChecked, diagnostic.Code, diagnostic.Phase, diagnostic.Message, diagnostic.Recommendation, diagnostic.Retryable, err)
	}
}

func portOrDefault(port string) string {
	if port != "" {
		return port
	}
	return "22"
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

func credentialHTTPTransport(proxyURL string, caBundle []byte) (*http.Transport, error) {
	result := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(proxyURL) != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, errors.New("invalid proxy URL")
		}
		result.Proxy = http.ProxyURL(parsed)
	}
	if len(caBundle) > 0 {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(caBundle) {
			return nil, errors.New("invalid HTTPS CA bundle")
		}
		result.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
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
