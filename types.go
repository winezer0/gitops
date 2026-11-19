// Package gitops provides structured Git repository operations.
package gitops

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalidURL              = errors.New("invalid Git repository URL")
	ErrUnsupportedTransport    = errors.New("unsupported Git transport")
	ErrHostNotAllowed          = errors.New("Git host is not allowed")
	ErrProxy                   = errors.New("Git proxy could not be resolved or is invalid")
	ErrUnsupported             = errors.New("Git operation is unsupported by the selected engine")
	ErrGitUnavailable          = errors.New("system Git is unavailable")
	ErrInvalidReference        = errors.New("invalid Git reference")
	ErrNoReferences            = errors.New("Git remote advertised no readable references")
	ErrCredentialNotConfigured = errors.New("Git credential is not configured for the host")
	ErrCloneSizeLimit          = errors.New("cloned repository exceeds configured size limit")
	// ErrSubmoduleLimit reports that recursive clone exceeded the nesting limit.
	ErrSubmoduleLimit = errors.New("Git submodule nesting limit exceeded")
	ErrDirtyWorktree  = errors.New("Git worktree has uncommitted changes")
)

type ErrorKind string

const (
	ErrorInvalidInput      ErrorKind = "invalid_input"
	ErrorHostDenied        ErrorKind = "host_denied"
	ErrorCredentialMissing ErrorKind = "credential_not_configured"
	ErrorAuthentication    ErrorKind = "authentication"
	ErrorNotFound          ErrorKind = "not_found"
	ErrorInvalidRef        ErrorKind = "invalid_ref"
	ErrorTimeout           ErrorKind = "timeout"
	ErrorCanceled          ErrorKind = "canceled"
	ErrorNetwork           ErrorKind = "network"
	ErrorUnsupported       ErrorKind = "unsupported"
	ErrorResourceLimit     ErrorKind = "resource_limit"
	ErrorWorktreeDirty     ErrorKind = "worktree_dirty"
	ErrorEngineUnavailable ErrorKind = "engine_unavailable"
	ErrorInternal          ErrorKind = "internal"
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
	Username      string
	Password      string
	Token         string
	SSHPrivateKey []byte
	Passphrase    string
	Provider      string
	APIURL        string
}

const (
	ProviderGitHub = "github"
	ProviderGitLab = "gitlab"
	ProviderGitea  = "gitea"
	ProviderCustom = "custom"
)

type Config struct {
	GitBinary       string
	SSHBinary       string
	AllowedHosts    []string
	Timeout         time.Duration
	MaxCloneBytes   int64
	DestinationRoot string
	// UseCredentialHelpers permits system Git to use configured HTTPS credential helpers when no explicit credential is supplied.
	UseCredentialHelpers bool
	CredentialFor        func(host string) (Credential, error)
	ValidateHost         func(context.Context, string) error
	ProxyFor             func(context.Context) (string, error)
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
	EventProcess    EventKind = "process"
	EventDiagnostic EventKind = "diagnostic"
)

// OperationEvent reports progress or a process outcome without arguments, environment values, or output.
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

// ProcessSnapshot reports a safe summary of one Git or SSH subprocess.
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

// CredentialTestRequest asks GitOps to verify a saved host credential.
type CredentialTestRequest struct {
	Host    string
	Timeout time.Duration
	OnEvent func(OperationEvent)
}

// CredentialTestResult reports host authentication and a safe diagnostic.
type CredentialTestResult struct {
	Authenticated bool
	CheckedAt     time.Time
	Diagnostic    Diagnostic
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
