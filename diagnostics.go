package gitops

import (
	"context"
	"errors"
	"strings"
	"time"
)

func successDiagnostic(repository Repository, ref string, timeout, duration time.Duration) Diagnostic {
	return Diagnostic{
		Code: "git_probe_succeeded", Phase: "reference_lookup", Message: "Git remote references were read successfully.",
		Host: limitedDiagnosticText(repository.Host, 255), Ref: limitedDiagnosticText(ref, 256), Transport: repository.Transport, Duration: duration, Timeout: timeout,
	}
}

func diagnosticForError(err error, phase string, repository Repository, ref string, timeout, duration time.Duration) Diagnostic {
	var operation *OperationError
	if errors.As(err, &operation) && operation.Diagnostic != nil {
		diagnostic := *operation.Diagnostic
		diagnostic.Phase = phase
		diagnostic.Host = limitedDiagnosticText(repository.Host, 255)
		diagnostic.Ref = limitedDiagnosticText(ref, 256)
		diagnostic.Transport = repository.Transport
		diagnostic.Timeout = timeout
		diagnostic.Duration = duration
		return diagnostic
	}

	kind, retryable := classifyError(err)
	diagnostic := Diagnostic{
		Code: "git_remote_failed", Phase: phase, Message: "Git could not complete the requested operation.",
		Recommendation: "Check the repository URL, host policy, credentials, and network connectivity.",
		Host:           limitedDiagnosticText(repository.Host, 255), Ref: limitedDiagnosticText(ref, 256), Transport: repository.Transport, Duration: duration, Timeout: timeout, Retryable: retryable,
	}
	switch {
	case errors.Is(err, ErrInvalidURL):
		diagnostic.Code, diagnostic.Phase = "invalid_repository_url", "url_validation"
		diagnostic.Message = "Repository URL is invalid or uses an unsupported transport."
		diagnostic.Recommendation = "Use an HTTPS or SSH Git URL without embedded credentials, query parameters, or fragments."
	case errors.Is(err, ErrUnsupportedTransport):
		diagnostic.Code, diagnostic.Phase = "unsupported_git_transport", "url_validation"
		diagnostic.Message = "Repository URL uses an unsupported Git transport."
		diagnostic.Recommendation = "Use HTTPS or SSH transport."
	case errors.Is(err, ErrProxy):
		diagnostic.Code, diagnostic.Phase = "git_proxy_failed", "proxy_connect"
		diagnostic.Message = "The configured outbound proxy could not be loaded or initialized."
		diagnostic.Recommendation = "Check the Git proxy settings and retry the operation."
		diagnostic.Retryable = true
	case errors.Is(err, ErrHostNotAllowed):
		diagnostic.Code, diagnostic.Phase = "source_host_not_allowed", "host_policy"
		diagnostic.Message = "Repository host is not allowed."
		diagnostic.Recommendation = "Add this host to the Git host allowlist and configure credentials for it if required."
	case errors.Is(err, ErrCredentialNotConfigured):
		diagnostic.Code, diagnostic.Phase = "git_host_credential_not_configured", "credential_lookup"
		diagnostic.Message = "No host-specific Git credential is configured."
		diagnostic.Recommendation = "Save a credential for this exact Git host before testing it."
	case errors.Is(err, ErrNoReferences):
		diagnostic.Code, diagnostic.Phase = "repository_not_found", "reference_lookup"
		diagnostic.Message = "Repository did not advertise any readable references."
		diagnostic.Recommendation = "Confirm the repository path and that the account can read at least one branch or tag."
	case errors.Is(err, ErrInvalidReference):
		diagnostic.Code, diagnostic.Phase = "ref_not_found", "ref_validation"
		diagnostic.Message = "Requested branch, tag, or commit was not found."
		diagnostic.Recommendation = "Check the ref spelling and confirm that the remote advertises the requested branch or tag."
	case errors.Is(err, context.Canceled):
		diagnostic.Code = "git_operation_canceled"
		diagnostic.Message = "Git operation was canceled."
		diagnostic.Recommendation = "Retry the operation if it was canceled unintentionally."
	case errors.Is(err, context.DeadlineExceeded):
		diagnostic.Code = "git_probe_timeout"
		diagnostic.Message = "Git remote did not respond before the operation timeout."
		diagnostic.Recommendation = "Check network, VPN, firewall, and proxy connectivity, then increase the operation timeout if needed."
		diagnostic.Retryable = true
	case kind == ErrorAuthentication:
		diagnostic.Code, diagnostic.Phase = "credential_rejected", "authentication"
		diagnostic.Message = "Repository credentials were rejected or do not grant read access."
		diagnostic.Recommendation = "Check that the credential is current, has repository read permission, and is mapped to the exact Git host."
	case kind == ErrorNotFound:
		diagnostic.Code = "repository_not_found"
		diagnostic.Message = "Repository was not found or is not readable."
		diagnostic.Recommendation = "Check the repository path and confirm that the configured credential has read access."
	case kind == ErrorNetwork:
		diagnostic.Code = "git_host_unreachable"
		diagnostic.Message = "The repository host could not be reached."
		diagnostic.Recommendation = "Check network routes, firewall rules, VPN access, DNS, and proxy configuration."
		diagnostic.Retryable = true
	case kind == ErrorUnsupported:
		diagnostic.Code = "git_operation_unsupported"
		diagnostic.Message = "The selected Git engine does not support this operation."
		diagnostic.Recommendation = "Install system Git or use a supported transport and operation."
	case kind == ErrorResourceLimit:
		diagnostic.Code = "git_resource_limit"
		diagnostic.Message = "The Git operation exceeded a configured resource limit."
		diagnostic.Recommendation = "Reduce the checkout size or increase the configured resource limit."
	}
	return diagnostic
}

func limitedDiagnosticText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}

func errorKind(err error) ErrorKind {
	var operation *OperationError
	if errors.As(err, &operation) {
		return operation.Kind
	}
	kind, _ := classifyError(err)
	return kind
}

func diagnosticFromCommandOutput(output []byte, err error) Diagnostic {
	text := strings.ToLower(string(output))
	diagnostic := diagnosticForError(err, "reference_lookup", Repository{}, "", 0, 0)
	switch {
	case strings.Contains(text, "host key verification failed"), strings.Contains(text, "remote host identification has changed"), strings.Contains(text, "host key has changed"):
		diagnostic.Code, diagnostic.Phase = "git_ssh_host_key_rejected", "ssh_host_verification"
		diagnostic.Message = "The SSH host fingerprint could not be verified."
		diagnostic.Recommendation = "Verify the repository server fingerprint and update the known_hosts entry. Do not disable host key checking."
		diagnostic.Retryable = false
	case strings.Contains(text, "authentication"), strings.Contains(text, "permission denied"), strings.Contains(text, "access denied"), strings.Contains(text, "returned error: 401"), strings.Contains(text, "returned error: 403"):
		diagnostic.Code, diagnostic.Phase = "credential_rejected", "authentication"
		diagnostic.Message = "Repository credentials were rejected or do not grant read access."
		diagnostic.Recommendation = "Check that the credential is current, has repository read permission, and is mapped to the exact Git host."
		diagnostic.Retryable = false
	case strings.Contains(text, "could not resolve host"), strings.Contains(text, "name or service not known"), strings.Contains(text, "no such host"):
		diagnostic.Code, diagnostic.Phase = "git_host_dns_failed", "dns_lookup"
		diagnostic.Message = "The repository host could not be resolved by DNS."
		diagnostic.Recommendation = "Check the host name and DNS configuration available to the Git client."
		diagnostic.Retryable = true
	case strings.Contains(text, "certificate"), strings.Contains(text, "ssl error"), strings.Contains(text, "tls"), strings.Contains(text, "schannel"):
		diagnostic.Code, diagnostic.Phase = "git_tls_failed", "tls_handshake"
		diagnostic.Message = "The secure connection to the repository host failed certificate or TLS validation."
		diagnostic.Recommendation = "Check the host certificate chain, server time, and TLS configuration."
		diagnostic.Retryable = false
	case strings.Contains(text, "proxy"), strings.Contains(text, "tunnel connection"):
		diagnostic.Code, diagnostic.Phase = "git_proxy_failed", "proxy_connect"
		diagnostic.Message = "The configured proxy could not connect to the repository host."
		diagnostic.Recommendation = "Check the server proxy URL, credentials, and reachability."
		diagnostic.Retryable = true
	case strings.Contains(text, "failed to connect"), strings.Contains(text, "connection refused"), strings.Contains(text, "network is unreachable"), strings.Contains(text, "operation timed out"):
		diagnostic.Code, diagnostic.Phase = "git_host_unreachable", "remote_connect"
		diagnostic.Message = "The repository host could not be reached from the Git client."
		diagnostic.Recommendation = "Check network routes, firewall rules, VPN access, and proxy configuration."
		diagnostic.Retryable = true
	case strings.Contains(text, "repository not found"), strings.Contains(text, "does not appear to be a git repository"), strings.Contains(text, "returned error: 404"):
		diagnostic.Code, diagnostic.Phase = "repository_not_found", "repository_lookup"
		diagnostic.Message = "Repository was not found or is not readable."
		diagnostic.Recommendation = "Check the repository path and confirm that the configured credential has read access."
		diagnostic.Retryable = false
	}
	return diagnostic
}
