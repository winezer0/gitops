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
	var credentialTestError *CredentialTestError
	if errors.As(err, &credentialTestError) && credentialTestError.Diagnostic.Code != "" {
		return credentialTestError.Diagnostic.Code
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
	case errors.Is(err, ErrCommitUnavailableAtDepth):
		return ErrorRefUnavailableAtDepth, false
	case errors.Is(err, ErrNoReferences):
		return ErrorNotFound, false
	case errors.Is(err, ErrHostNotAllowed):
		return ErrorHostDenied, false
	case errors.Is(err, ErrCredentialNotConfigured):
		return ErrorCredentialMissing, false
	case errors.Is(err, ErrCredentialHelpersUnsupported):
		return ErrorUnsupported, false
	case errors.Is(err, ErrSSHHostKeyTrustNotConfigured):
		return ErrorCredentialMissing, false
	case errors.Is(err, ErrSSHKeyPassphraseMissing):
		return ErrorCredentialMissing, false
	case errors.Is(err, ErrSSHIdentityLimit):
		return ErrorResourceLimit, false
	case errors.Is(err, ErrSSHHostKeyNotTrusted), errors.Is(err, ErrSSHHostKeyChanged):
		return ErrorAuthentication, false
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

func isAuthenticationMessage(message string) bool {
	for _, marker := range []string{
		"authentication failed", "authentication required", "unable to authenticate", "auth required", "authorization failed",
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
