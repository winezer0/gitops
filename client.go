package gitops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// NewClient validates configuration and creates a Git operation client.
func NewClient(config Config) (*Client, error) {
	if config.UseCredentialHelpers {
		return nil, ErrCredentialHelpersUnsupported
	}
	if config.Timeout < 0 {
		return nil, fmt.Errorf("timeout must not be negative")
	}
	if config.Timeout == 0 {
		config.Timeout = 15 * time.Minute
	}
	if config.MaxCloneBytes < 0 {
		return nil, fmt.Errorf("maximum clone size must not be negative")
	}
	if config.MaxCloneBytes == 0 {
		config.MaxCloneBytes = 5 << 30
	}
	return &Client{config: config}, nil
}

// Probe reads a repository's advertised references and resolves an optional ref.
func (client *Client) Probe(ctx context.Context, request ProbeRequest) (ProbeResult, error) {
	started := time.Now()
	if request.Timeout < 0 {
		return ProbeResult{}, operationError(fmt.Errorf("probe timeout must not be negative"))
	}
	timeout := client.requestTimeout(request.Timeout)
	request.Timeout = timeout
	if request.MaxReferences <= 0 {
		request.MaxReferences = 100
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	repository, credential, err := client.prepare(ctx, request.URL, request.Credential)
	if err != nil {
		repositoryHint, _ := ParseURL(request.URL)
		return client.probeFailure(ProbeResult{}, err, "host_validation", repositoryHint, request.Ref, timeout, started, request.OnEvent)
	}
	proxyURL, err := client.proxy(ctx, repository.Transport)
	if err != nil {
		return client.probeFailure(ProbeResult{}, err, "network_setup", repository, request.Ref, timeout, started, request.OnEvent)
	}
	result, err := (goGitEngine{config: client.config}).Probe(ctx, repository, request, credential, timeout, proxyURL)
	if err != nil {
		return client.probeFailure(result, err, "reference_lookup", repository, request.Ref, timeout, started, request.OnEvent)
	}
	if result.Diagnostic == nil {
		diagnostic := successDiagnostic(repository, request.Ref, timeout, time.Since(started))
		result.Diagnostic = &diagnostic
	}
	limitReferences(&result, request.MaxReferences)
	emitProgress(request.OnEvent, OperationEvent{Kind: EventDiagnostic, Operation: "probe", Engine: result.Engine, Diagnostic: result.Diagnostic})
	return result, nil
}

// Clone creates a working copy and checks out the requested branch, tag, or commit.
func (client *Client) Clone(ctx context.Context, request CloneRequest) (cloneResult CloneResult, returnErr error) {
	started := time.Now()
	var repository Repository
	defer func() {
		if returnErr == nil {
			return
		}
		phase := "clone"
		diagnostic := diagnosticForError(returnErr, phase, repository, request.Ref, request.Timeout, time.Since(started))
		returnErr = &OperationError{Kind: errorKind(returnErr), Retryable: diagnostic.Retryable, Cause: returnErr, Diagnostic: &diagnostic}
		emitProgress(request.OnEvent, OperationEvent{Kind: EventDiagnostic, Operation: "clone", Engine: cloneResult.Engine, Diagnostic: &diagnostic})
	}()
	if request.Timeout < 0 || request.MaxBytes < 0 {
		return CloneResult{}, operationError(fmt.Errorf("clone limits must not be negative"))
	}
	timeout := client.requestTimeout(request.Timeout)
	request.Timeout = timeout
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var credential Credential
	var err error
	repository, credential, err = client.prepare(ctx, request.URL, request.Credential)
	if err != nil {
		return CloneResult{}, operationError(err)
	}
	if strings.TrimSpace(request.Destination) == "" {
		return CloneResult{}, operationError(fmt.Errorf("clone destination is required"))
	}
	if request.Depth < 0 {
		return CloneResult{}, operationError(fmt.Errorf("clone depth must not be negative"))
	}
	if request.Commit != "" && !isCommit(request.Commit) {
		return CloneResult{}, operationError(ErrInvalidReference)
	}
	if isCommit(request.Ref) && isCommit(request.Commit) && !strings.EqualFold(request.Ref, request.Commit) {
		return CloneResult{}, operationError(ErrInvalidReference)
	}
	if err := validateSparsePaths(request.SparsePaths); err != nil {
		return CloneResult{}, operationError(err)
	}
	if request.Commit == "" && isCommit(request.Ref) {
		request.Commit = request.Ref
	}
	destination, exists, err := client.prepareDestination(request.Destination, (request.ReusePinned && request.Commit != "") || request.ReplaceExisting)
	if err != nil {
		return CloneResult{}, operationError(err)
	}
	if exists {
		valid, commit, validateErr := client.ValidateCheckout(destination, repository.URL, request.Commit)
		if validateErr == nil && valid && len(request.SparsePaths) == 0 {
			result := CloneResult{Repository: repository, Commit: commit, Engine: "workspace-reuse", Path: destination}
			result.SizeBytes, err = directorySize(destination)
			if err != nil {
				return CloneResult{}, operationError(err)
			}
			emitProgress(request.OnEvent, OperationEvent{Kind: EventProgress, Operation: "clone", Engine: result.Engine, Phase: "workspace_reuse", Percent: 100})
			diagnostic := Diagnostic{Code: "git_clone_succeeded", Phase: "workspace_reuse", Message: "A matching clean Git checkout was reused.", Host: repository.Host, Ref: request.Ref, Transport: repository.Transport, Duration: time.Since(started), Timeout: timeout}
			emitProgress(request.OnEvent, OperationEvent{Kind: EventDiagnostic, Operation: "clone", Engine: result.Engine, Diagnostic: &diagnostic})
			return result, nil
		}
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".gitops-stage-")
	if err != nil {
		return CloneResult{}, operationError(err)
	}
	defer os.RemoveAll(staging)
	request.Destination = staging
	maxBytes := client.config.MaxCloneBytes
	if request.MaxBytes > 0 {
		maxBytes = request.MaxBytes
	}
	cloneContext, cancelClone := context.WithCancel(ctx)
	monitorContext, cancelMonitor := context.WithCancel(ctx)
	monitorResult := make(chan error, 1)
	go func() {
		monitorResult <- monitorWorkspaceSize(monitorContext, staging, maxBytes, cancelClone)
	}()
	result, err := client.clone(cloneContext, repository, request, credential)
	if err == nil && request.RecurseSubmodules {
		err = client.cloneSubmodules(cloneContext, request, repository, credential, staging)
	}
	cancelMonitor()
	monitorErr := <-monitorResult
	cancelClone()
	if errors.Is(monitorErr, ErrCloneSizeLimit) {
		return CloneResult{}, operationError(ErrCloneSizeLimit)
	}
	if monitorErr != nil && !errors.Is(monitorErr, context.Canceled) {
		return CloneResult{}, operationError(monitorErr)
	}
	if err != nil {
		return CloneResult{}, operationError(err)
	}
	if err := checkWorkspaceSize(ctx, staging, maxBytes); err != nil {
		return CloneResult{}, operationError(err)
	}
	result.SizeBytes, err = directorySize(staging)
	if err != nil {
		return CloneResult{}, operationError(err)
	}
	if err := publishWorkspace(staging, destination, exists); err != nil {
		return CloneResult{}, operationError(err)
	}
	result.Path = destination
	diagnostic := Diagnostic{Code: "git_clone_succeeded", Phase: "checkout", Message: "Git repository was cloned successfully.", Host: repository.Host, Ref: result.ResolvedRef, Transport: repository.Transport, Duration: time.Since(started), Timeout: timeout}
	emitProgress(request.OnEvent, OperationEvent{Kind: EventDiagnostic, Operation: "clone", Engine: result.Engine, Diagnostic: &diagnostic})
	return result, operationError(err)
}

// TestCredential verifies that a credential can read the supplied repository.
func (client *Client) TestCredential(ctx context.Context, request ProbeRequest, credential Credential) (ProbeResult, error) {
	request.Credential = &credential
	return client.Probe(ctx, request)
}

// TestHostCredential verifies a host credential through its provider API or strict SSH handshake.
// Failed checks return the diagnostic result together with a *CredentialTestError.
func (client *Client) TestHostCredential(ctx context.Context, request CredentialTestRequest) (CredentialTestResult, error) {
	return client.testHostCredential(ctx, request)
}

// Checkout switches an existing working tree to a branch, tag, or commit.
func (client *Client) Checkout(ctx context.Context, request CheckoutRequest) (CheckoutResult, error) {
	if strings.TrimSpace(request.Path) == "" {
		return CheckoutResult{}, operationError(fmt.Errorf("checkout path is required"))
	}
	ref := request.Commit
	if ref == "" {
		ref = request.Ref
	}
	if ref == "" || strings.HasPrefix(ref, "-") {
		return CheckoutResult{}, operationError(ErrInvalidReference)
	}
	if request.Timeout < 0 {
		return CheckoutResult{}, operationError(fmt.Errorf("checkout timeout must not be negative"))
	}
	ctx, cancel := context.WithTimeout(ctx, client.requestTimeout(request.Timeout))
	defer cancel()
	result, err := (goGitEngine{config: client.config}).Checkout(ctx, request)
	return result, operationError(err)
}

// ValidateCheckout checks the origin URL and current commit of an existing worktree.
func (client *Client) ValidateCheckout(path, expectedURL, expectedCommit string) (bool, string, error) {
	if strings.TrimSpace(path) == "" {
		return false, "", operationError(fmt.Errorf("checkout path is required"))
	}
	repository, err := goGitOpen(path)
	if err != nil {
		return false, "", operationError(err)
	}
	config, err := repository.Config()
	if err != nil {
		return false, "", operationError(err)
	}
	remote := config.Remotes["origin"]
	if remote == nil || len(remote.URLs) == 0 {
		return false, "", operationError(fmt.Errorf("checkout has no origin URL"))
	}
	actual, err := ParseURL(remote.URLs[0])
	if err != nil {
		return false, "", operationError(err)
	}
	expected, err := ParseURL(expectedURL)
	if err != nil {
		return false, "", operationError(err)
	}
	if actual.URL != expected.URL {
		return false, "", nil
	}
	head, err := repository.Head()
	if err != nil {
		return false, "", operationError(err)
	}
	commit := head.Hash().String()
	worktree, err := repository.Worktree()
	if err != nil {
		return false, "", operationError(err)
	}
	status, err := worktree.Status()
	if err != nil {
		return false, "", operationError(err)
	}
	if !status.IsClean() {
		return false, commit, operationError(ErrDirtyWorktree)
	}
	if expectedCommit != "" && !strings.EqualFold(expectedCommit, commit) {
		return false, commit, nil
	}
	return true, commit, nil
}

func (client *Client) prepare(ctx context.Context, raw string, explicit ...*Credential) (Repository, Credential, error) {
	repository, err := ParseURL(raw)
	if err != nil {
		return Repository{}, Credential{}, err
	}
	if err := client.allowHost(repository.Host); err != nil {
		return Repository{}, Credential{}, err
	}
	if client.config.ValidateHost != nil {
		if err := client.config.ValidateHost(ctx, repository.Host); err != nil {
			return Repository{}, Credential{}, err
		}
	}
	if repository.Transport == "ssh" && client.config.DiscoverSSHKeys {
		discovered, err := discoverSSHConfig(repository, client.config)
		if err != nil {
			return Repository{}, Credential{}, err
		}
		targetHostname := discovered.HostName
		targetPort := discovered.Port
		if strings.Contains(repository.URL, "://") {
			parsed, parseErr := url.Parse(repository.URL)
			if parseErr != nil {
				return Repository{}, Credential{}, ErrInvalidURL
			}
			if parsed.Port() != "" {
				targetPort = parsed.Port()
			}
			if targetHostname == "" {
				targetHostname = parsed.Hostname()
			}
		} else if targetHostname == "" {
			targetHostname, _, err = sshAliasAndPort(repository.URL)
			if err != nil {
				return Repository{}, Credential{}, err
			}
		}
		targetHost := targetHostname
		if targetPort != "" {
			targetHost = net.JoinHostPort(targetHostname, targetPort)
		}
		if normalizeHost(targetHost) != normalizeHost(repository.Host) {
			if err := client.allowHost(targetHost); err != nil {
				return Repository{}, Credential{}, err
			}
			if client.config.ValidateHost != nil {
				if err := client.config.ValidateHost(ctx, targetHost); err != nil {
					return Repository{}, Credential{}, err
				}
			}
		}
	}
	var credential Credential
	explicitProvided := len(explicit) > 0 && explicit[0] != nil
	fallbackToLegacy := !explicitProvided && client.config.CredentialForRepository == nil
	if explicitProvided {
		credential = *explicit[0]
		if !credentialHasSecret(credential) {
			return Repository{}, Credential{}, ErrCredentialNotConfigured
		}
	} else if client.config.CredentialForRepository != nil {
		credential, err = client.config.CredentialForRepository(repository)
		if errors.Is(err, ErrCredentialNotConfigured) {
			fallbackToLegacy = true
			credential = Credential{}
			err = nil
		}
	}
	if fallbackToLegacy && err == nil && client.config.CredentialFor != nil {
		credential, err = client.config.CredentialFor(repository.Host)
		if errors.Is(err, ErrCredentialNotConfigured) {
			credential = Credential{}
			err = nil
		}
	}
	if !explicitProvided && err == nil && !credentialHasSecret(credential) && repository.Transport == "ssh" && !client.config.DiscoverSSHKeys {
		return Repository{}, Credential{}, ErrCredentialNotConfigured
	}
	if err == nil && repository.Transport == "https" && (len(credential.SSHPrivateKey) > 0 || credential.SSHPrivateKeyPath != "") {
		err = fmt.Errorf("SSH credentials cannot be used with HTTPS repositories")
	}
	if err == nil && repository.Transport == "ssh" && (credential.Token != "" || credential.Password != "") {
		err = fmt.Errorf("HTTPS credentials cannot be used with SSH repositories")
	}
	return repository, credential, err
}

func credentialHasSecret(credential Credential) bool {
	return credential.Token != "" || credential.Password != "" || len(credential.SSHPrivateKey) > 0 || credential.SSHPrivateKeyPath != ""
}

func (client *Client) allowHost(host string) error {
	return ValidateHost(host, client.config.AllowedHosts)
}

func (client *Client) prepareDestination(destination string, allowExisting bool) (string, bool, error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return "", false, err
	}
	_, statErr := os.Lstat(absolute)
	exists := statErr == nil
	if exists && !allowExisting {
		return "", false, fmt.Errorf("clone destination already exists")
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", false, statErr
	}
	if exists {
		info, err := os.Lstat(absolute)
		if err != nil {
			return "", false, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", false, fmt.Errorf("existing clone destination must be a real directory")
		}
	}
	var root string
	resolvedRoot := ""
	if client.config.DestinationRoot != "" {
		root, err = filepath.Abs(client.config.DestinationRoot)
		if err != nil {
			return "", false, err
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", false, err
		}
		resolvedRoot, err = filepath.EvalSymlinks(root)
		if err != nil {
			return "", false, err
		}
		if err := withinRoot(root, absolute); err != nil {
			return "", false, fmt.Errorf("clone destination is outside the configured root")
		}
	}
	parent := filepath.Dir(absolute)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", false, err
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", false, err
	}
	absolute = filepath.Join(resolvedParent, filepath.Base(absolute))
	if exists {
		resolvedDestination, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return "", false, err
		}
		absolute = resolvedDestination
	}
	if resolvedRoot != "" {
		if err := withinRoot(resolvedRoot, absolute); err != nil {
			return "", false, fmt.Errorf("clone destination resolves outside the configured root")
		}
	}
	return absolute, exists, nil
}

func withinRoot(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("path is outside root")
	}
	return nil
}

func (client *Client) clone(ctx context.Context, repository Repository, request CloneRequest, credential Credential) (CloneResult, error) {
	proxyURL, err := client.proxy(ctx, repository.Transport)
	if err != nil {
		return CloneResult{}, err
	}
	return (goGitEngine{config: client.config}).Clone(ctx, repository, request, credential, client.requestTimeout(request.Timeout), proxyURL)
}

func (client *Client) requestTimeout(requested time.Duration) time.Duration {
	if requested > 0 {
		return requested
	}
	return client.config.Timeout
}

func (client *Client) proxy(ctx context.Context, transport string) (string, error) {
	if transport == "ssh" || client.config.ProxyFor == nil {
		return "", nil
	}
	value, err := client.config.ProxyFor(ctx)
	if err != nil {
		return "", ErrProxy
	}
	if strings.TrimSpace(value) != "" {
		if _, err := parseProxyURL(value); err != nil {
			return "", ErrProxy
		}
	}
	return strings.TrimSpace(value), nil
}

func (client *Client) probeFailure(result ProbeResult, err error, phase string, repository Repository, ref string, timeout time.Duration, started time.Time, observer func(OperationEvent)) (ProbeResult, error) {
	diagnostic := diagnosticForError(err, phase, repository, ref, timeout, time.Since(started))
	result.Diagnostic = &diagnostic
	emitProgress(observer, OperationEvent{Kind: EventDiagnostic, Operation: "probe", Engine: result.Engine, Diagnostic: &diagnostic})
	return result, &OperationError{Kind: errorKind(err), Retryable: diagnostic.Retryable, Cause: err, Diagnostic: &diagnostic}
}

func sortReferences(references []Reference) {
	sort.Slice(references, func(left, right int) bool {
		if references[left].Type == references[right].Type {
			return references[left].Name < references[right].Name
		}
		return references[left].Type < references[right].Type
	})
}
