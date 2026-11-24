// Package gitops provides structured Git repository operations.
package gitops

import (
	"context"
	"errors"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	ErrInvalidURL                   = errors.New("invalid Git repository URL")
	ErrUnsupportedTransport         = errors.New("unsupported Git transport")
	ErrHostNotAllowed               = errors.New("Git host is not allowed")
	ErrProxy                        = errors.New("Git proxy could not be resolved or is invalid")
	ErrUnsupported                  = errors.New("Git operation is unsupported by the selected engine")
	ErrGitUnavailable               = errors.New("system Git is unavailable")
	ErrInvalidReference             = errors.New("invalid Git reference")
	ErrNoReferences                 = errors.New("Git remote advertised no readable references")
	ErrCredentialNotConfigured      = errors.New("Git credential is not configured for the host")
	ErrCredentialHelpersUnsupported = errors.New("system Git credential helpers are unsupported")
	ErrCloneSizeLimit               = errors.New("cloned repository exceeds configured size limit")
	// ErrSubmoduleLimit reports that recursive clone exceeded the nesting limit.
	ErrSubmoduleLimit               = errors.New("Git submodule nesting limit exceeded")
	ErrDirtyWorktree                = errors.New("Git worktree has uncommitted changes")
	ErrSSHHostKeyTrustNotConfigured = errors.New("SSH host key trust is not configured")
	ErrSSHHostKeyNotTrusted         = errors.New("SSH host key is not trusted")
	ErrSSHHostKeyChanged            = errors.New("SSH host key changed")
	ErrCommitUnavailableAtDepth     = errors.New("requested Git commit is unavailable at the requested shallow depth")
	ErrSSHKeyPassphraseMissing      = errors.New("encrypted SSH private key requires a passphrase")
	ErrSSHIdentityLimit             = errors.New("SSH identity candidate limit exceeded")
)

type ErrorKind string

const (
	ErrorInvalidInput          ErrorKind = "invalid_input"
	ErrorHostDenied            ErrorKind = "host_denied"
	ErrorCredentialMissing     ErrorKind = "credential_not_configured"
	ErrorAuthentication        ErrorKind = "authentication"
	ErrorNotFound              ErrorKind = "not_found"
	ErrorInvalidRef            ErrorKind = "invalid_ref"
	ErrorRefUnavailableAtDepth ErrorKind = "ref_unavailable_at_depth"
	ErrorTimeout               ErrorKind = "timeout"
	ErrorCanceled              ErrorKind = "canceled"
	ErrorNetwork               ErrorKind = "network"
	ErrorUnsupported           ErrorKind = "unsupported"
	ErrorResourceLimit         ErrorKind = "resource_limit"
	ErrorWorktreeDirty         ErrorKind = "worktree_dirty"
	ErrorEngineUnavailable     ErrorKind = "engine_unavailable"
	ErrorInternal              ErrorKind = "internal"
)

type OperationError struct {
	Kind       ErrorKind
	Retryable  bool
	Cause      error
	Diagnostic *Diagnostic
}

func (operationError *OperationError) Error() string {
	return string(operationError.Kind)
}

func (operationError *OperationError) Unwrap() error {
	return operationError.Cause
}

type Credential struct {
	Username          string
	Password          string
	Token             string
	SSHPrivateKey     []byte
	SSHPrivateKeyPath string
	Passphrase        string
	Provider          string
	APIURL            string
}

const (
	ProviderGitHub = "github"
	ProviderGitLab = "gitlab"
	ProviderGitea  = "gitea"
	ProviderCustom = "custom"
)

type Config struct {
	AllowedHosts    []string
	Timeout         time.Duration
	MaxCloneBytes   int64
	DestinationRoot string
	// UseCredentialHelpers is retained for source compatibility. NewClient returns
	// ErrCredentialHelpersUnsupported when it is enabled.
	UseCredentialHelpers bool
	// FallbackSSHOnHTTPSAuthFailure retries HTTPS authentication failures over SSH.
	FallbackSSHOnHTTPSAuthFailure bool
	// CredentialFor resolves credentials by host for compatibility. New code should
	// use request credentials or CredentialForRepository.
	CredentialFor func(host string) (Credential, error)
	ValidateHost  func(context.Context, string) error
	ProxyFor      func(context.Context) (string, error)
	// CABundle contains additional PEM certificates trusted for HTTPS operations.
	CABundle []byte
	// SSHKnownHostsFiles enables strict SSH host-key verification against these files.
	SSHKnownHostsFiles []string
	// UseUserSSHKnownHosts explicitly enables the user's and system's default known_hosts files.
	UseUserSSHKnownHosts bool
	// SSHHostKeyCallback can provide application-managed host-key trust, including pinned keys.
	SSHHostKeyCallback ssh.HostKeyCallback
	// DiscoverSSHKeys opts into reading the user's SSH config and private-key files.
	DiscoverSSHKeys bool
	// SSHConfigDir overrides the default ~/.ssh directory when SSH identity discovery is enabled.
	SSHConfigDir string
	// SSHKeyPassphraseFor supplies a passphrase for a discovered encrypted private key.
	SSHKeyPassphraseFor func(path string) (string, error)
	// MaxSSHIdentityFiles bounds the number of local key candidates tried; zero uses a default limit.
	MaxSSHIdentityFiles int
	// CredentialForRepository resolves credentials using the normalized repository protocol, host, port, and path.
	CredentialForRepository func(Repository) (Credential, error)
}

type Client struct {
	config Config
}

type Repository struct {
	URL       string
	Host      string
	Transport string
	Path      string
}

type ProbeRequest struct {
	URL           string
	Ref           string
	Credential    *Credential
	Timeout       time.Duration
	MaxReferences int
	OnEvent       func(OperationEvent)
}

type Reference struct {
	Name   string
	Type   string
	Commit string
}

type ProbeResult struct {
	Repository               Repository
	DefaultBranch            string
	ResolvedRef              string
	ResolvedCommit           string
	References               []Reference
	BranchCount              int
	TagCount                 int
	CheckedAt                time.Time
	Engine                   string
	CommitValidationDeferred bool
	Diagnostic               *Diagnostic
}

type CloneRequest struct {
	URL             string
	Ref             string
	Commit          string
	Credential      *Credential
	Destination     string
	Depth           int
	Timeout         time.Duration
	MaxBytes        int64
	SparsePaths     []string
	ReusePinned     bool
	ReplaceExisting bool
	// RecurseSubmodules clones child repositories at the commits recorded by the parent.
	RecurseSubmodules bool
	OnProgress        func(ProgressEvent)
	OnEvent           func(OperationEvent)
}

type ProgressEvent struct {
	Engine          string
	Phase           string
	Percent         int
	ReceivedObjects int64
	TotalObjects    int64
	BytesReceived   int64
}

type CloneResult struct {
	Repository  Repository
	Commit      string
	ResolvedRef string
	Engine      string
	Path        string
	SizeBytes   int64
}

// Diagnostic contains safe, actionable information about a Git operation.
type Diagnostic struct {
	Code           string
	Phase          string
	Message        string
	Recommendation string
	Host           string
	Ref            string
	Transport      string
	Duration       time.Duration
	Timeout        time.Duration
	Retryable      bool
}

// EventKind identifies the type of structured Git operation event.
type EventKind string

const (
	EventProgress   EventKind = "progress"
	EventProcess    EventKind = "process" // Retained for compatibility; the Go engine does not emit process events.
	EventDiagnostic EventKind = "diagnostic"
)

// OperationEvent reports progress and diagnostics. Process fields remain for
// source compatibility and are not populated by the Go engine.
type OperationEvent struct {
	Kind            EventKind
	Operation       string
	Engine          string
	Phase           string
	At              time.Time
	Percent         int
	ReceivedObjects int64
	TotalObjects    int64
	BytesReceived   int64
	Process         *ProcessSnapshot
	Diagnostic      *Diagnostic
}

// ProcessSnapshot is retained for source compatibility with earlier engines.
type ProcessSnapshot struct {
	Name        string
	State       string
	StopReason  string
	ExitCode    int
	StartedAt   time.Time
	FinishedAt  *time.Time
	OutputBytes int64
	Truncated   bool
}

// CredentialTestRequest asks GitOps to verify a host credential.
type CredentialTestRequest struct {
	Host       string
	Credential *Credential
	Timeout    time.Duration
	OnEvent    func(OperationEvent)
}

// CredentialStatus records what the credential check established about the credential.
type CredentialStatus string

const (
	// CredentialStatusUnknown means the check could not establish whether the credential was accepted.
	CredentialStatusUnknown CredentialStatus = "unknown"
	// CredentialStatusAccepted means the checked endpoint accepted the credential.
	CredentialStatusAccepted CredentialStatus = "accepted"
	// CredentialStatusRejected means the checked endpoint rejected the credential.
	CredentialStatusRejected CredentialStatus = "rejected"
)

// ProfileAccessStatus records whether the provider's user-profile endpoint was readable.
type ProfileAccessStatus string

const (
	// ProfileAccessNotChecked means no user-profile request was made.
	ProfileAccessNotChecked ProfileAccessStatus = "not_checked"
	// ProfileAccessAllowed means the provider allowed access to the user profile.
	ProfileAccessAllowed ProfileAccessStatus = "allowed"
	// ProfileAccessDenied means the provider denied access to the user profile.
	ProfileAccessDenied ProfileAccessStatus = "denied"
	// ProfileAccessInsufficientScope means the credential was accepted but lacks a scope for profile access.
	ProfileAccessInsufficientScope ProfileAccessStatus = "insufficient_scope"
)

// CredentialTestResult separates credential acceptance from user-profile access.
type CredentialTestResult struct {
	CredentialStatus CredentialStatus
	ProfileAccess    ProfileAccessStatus
	CheckedAt        time.Time
	Diagnostic       Diagnostic
}

// CredentialTestError reports an unsuccessful credential check with a safe diagnostic.
type CredentialTestError struct {
	Diagnostic Diagnostic
	cause      error
}

func (credentialTestError *CredentialTestError) Error() string {
	if credentialTestError == nil {
		return "credential test failed"
	}
	if credentialTestError.Diagnostic.Message != "" {
		return credentialTestError.Diagnostic.Message
	}
	return "credential test failed"
}

func (credentialTestError *CredentialTestError) Unwrap() error {
	if credentialTestError == nil {
		return nil
	}
	return credentialTestError.cause
}

type CheckoutRequest struct {
	Path    string
	Ref     string
	Commit  string
	Timeout time.Duration
	OnEvent func(OperationEvent)
}

type CheckoutResult struct {
	Path   string
	Commit string
	Engine string
}

type engine interface {
	Name() string
	Probe(context.Context, Repository, ProbeRequest, Credential, time.Duration, string) (ProbeResult, error)
	Clone(context.Context, Repository, CloneRequest, Credential, time.Duration, string) (CloneResult, error)
}
