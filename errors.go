package gitops

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"strings"
)

func operationError(err error) error {
	if err == nil {
		return nil
	}
	var existing *OperationError
	if errors.As(err, &existing) {
		return err
	}
	kind, retryable := classifyError(err)
	return &OperationError{Kind: kind, Retryable: retryable, Cause: err}
}

// ErrorCode returns the stable, non-sensitive classification for an operation error.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var operationError *OperationError
	if errors.As(err, &operationError) {
		return string(operationError.Kind)
	}
	kind, _ := classifyError(err)
	return string(kind)
}

func classifyError(err error) (ErrorKind, bool) {
	switch {
	case errors.Is(err, context.Canceled):
		return ErrorCanceled, false
	case errors.Is(err, context.DeadlineExceeded):
		return ErrorTimeout, true
	case errors.Is(err, ErrInvalidURL):
		return ErrorInvalidInput, false
	case errors.Is(err, ErrUnsupportedTransport):
		return ErrorInvalidInput, false
	case errors.Is(err, ErrProxy):
		return ErrorNetwork, true
	case errors.Is(err, ErrInvalidReference):
		return ErrorInvalidRef, false
	case errors.Is(err, ErrNoReferences):
		return ErrorNotFound, false
	case errors.Is(err, ErrHostNotAllowed):
		return ErrorHostDenied, false
	case errors.Is(err, ErrCredentialNotConfigured):
		return ErrorCredentialMissing, false
	case errors.Is(err, ErrUnsupported):
		return ErrorUnsupported, false
	case errors.Is(err, ErrCloneSizeLimit):
		return ErrorResourceLimit, false
	case errors.Is(err, ErrSubmoduleLimit):
		return ErrorResourceLimit, false
	case errors.Is(err, ErrDirtyWorktree):
		return ErrorWorktreeDirty, false
	case errors.Is(err, ErrGitUnavailable):
		return ErrorEngineUnavailable, true
	}
	var networkError net.Error
	var urlError *url.Error
	var certificateError x509.UnknownAuthorityError
	if errors.As(err, &networkError) || errors.As(err, &urlError) || errors.As(err, &certificateError) {
		return ErrorNetwork, true
	}
	message := strings.ToLower(err.Error())
	switch {
	case isAuthenticationMessage(message):
		return ErrorAuthentication, false
	case strings.Contains(message, "repository not found"), strings.Contains(message, "remote not found"), strings.Contains(message, "not found"):
		return ErrorNotFound, false
	case strings.Contains(message, "could not resolve"), strings.Contains(message, "connection refused"), strings.Contains(message, "network is unreachable"), strings.Contains(message, "tls"):
		return ErrorNetwork, true
	default:
		return ErrorInternal, false
	}
}

func commandError(resultOutput []byte, err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(string(resultOutput))
	kind, retryable := classifyError(err)
	switch {
	case isAuthenticationMessage(message), strings.Contains(message, "returned error: 401"), strings.Contains(message, "returned error: 403"):
		kind, retryable = ErrorAuthentication, false
	case strings.Contains(message, "repository not found"), strings.Contains(message, "does not appear to be a git repository"), strings.Contains(message, "returned error: 404"):
		kind, retryable = ErrorNotFound, false
	case strings.Contains(message, "could not resolve host"), strings.Contains(message, "failed to connect"), strings.Contains(message, "connection timed out"):
		kind, retryable = ErrorNetwork, true
	case strings.Contains(message, "certificate"), strings.Contains(message, "ssl certificate problem"), strings.Contains(message, "tls"):
		kind, retryable = ErrorNetwork, false
	case strings.Contains(message, "host key verification failed"):
		kind, retryable = ErrorAuthentication, false
	}
	diagnostic := diagnosticFromCommandOutput(resultOutput, err)
	return &OperationError{Kind: kind, Retryable: retryable, Cause: err, Diagnostic: &diagnostic}
}

func isAuthenticationMessage(message string) bool {
	for _, marker := range []string{
		"authentication failed", "authentication required", "auth required", "authorization failed",
		"permission denied", "access denied", "unauthorized", "forbidden", "invalid credentials",
		"incorrect username or password", "invalid username or password", "http basic",
		"could not read username", "could not read password", "terminal prompts disabled",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
