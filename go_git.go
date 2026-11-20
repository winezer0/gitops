package gitops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitHTTP "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitSSH "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

type goGitEngine struct{ config Config }

func (goGitEngine) Name() string { return "go-git" }

func (engine goGitEngine) Probe(ctx context.Context, repository Repository, request ProbeRequest, credential Credential, timeout time.Duration, proxyURL string) (ProbeResult, error) {
	if err := validateRequestedRef(request.Ref); err != nil {
		return ProbeResult{}, err
	}
	ctx = withGitHTTPRedirectPolicy(ctx, engine.config)
	proxy, err := parseProxyURL(proxyURL)
	if err != nil {
		return ProbeResult{}, err
	}
	auth, err := authMethod(repository, credential, engine.config, timeout)
	if err != nil {
		return ProbeResult{}, err
	}
	remoteURL, err := goGitRemoteURL(repository, engine.config, credential)
	if err != nil {
		return ProbeResult{}, err
	}
	remote := git.NewRemote(nil, &config.RemoteConfig{Name: "origin", URLs: []string{remoteURL}})
	options := &git.ListOptions{Auth: auth, Timeout: int(timeout.Seconds()), PeelingOption: git.AppendPeeled, ProxyOptions: proxy, CABundle: engine.config.CABundle}
	refs, err := remote.ListContext(ctx, options)
	if err != nil {
		return ProbeResult{}, err
	}
	var output strings.Builder
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference {
			fmt.Fprintf(&output, "ref: %s\tHEAD\n", ref.Target())
		}
		fmt.Fprintf(&output, "%s\t%s\n", ref.Hash().String(), ref.Name().String())
	}
	return parseReferences(repository, request.Ref, []byte(output.String()), engine.Name())
}

func (engine goGitEngine) Clone(ctx context.Context, repository Repository, request CloneRequest, credential Credential, timeout time.Duration, proxyURL string) (CloneResult, error) {
	if err := validateRequestedRef(request.Ref); err != nil {
		return CloneResult{}, err
	}
	if request.Commit != "" && !isCommit(request.Commit) {
		return CloneResult{}, ErrInvalidReference
	}
	ctx = withGitHTTPRedirectPolicy(ctx, engine.config)
	proxy, err := parseProxyURL(proxyURL)
	if err != nil {
		return CloneResult{}, err
	}
	auth, err := authMethod(repository, credential, engine.config, timeout)
	if err != nil {
		return CloneResult{}, err
	}
	remoteURL, err := goGitRemoteURL(repository, engine.config, credential)
	if err != nil {
		return CloneResult{}, err
	}
	depth := request.Depth
	if depth == 0 {
		depth = 1
	}
	commit := request.Commit
	if commit == "" && isCommit(request.Ref) {
		commit = request.Ref
	}
	options := &git.CloneOptions{URL: remoteURL, Auth: auth, Depth: depth, NoCheckout: len(request.SparsePaths) > 0, RecurseSubmodules: git.NoRecurseSubmodules, Tags: git.NoTags, SingleBranch: request.Ref != "" && !isCommit(request.Ref), ProxyOptions: proxy, CABundle: engine.config.CABundle}
	if request.OnProgress != nil || request.OnEvent != nil {
		options.Progress = &progressWriter{engine: engine.Name(), operation: "clone", callback: request.OnProgress, observer: request.OnEvent}
	}
	if request.Ref != "" && !isCommit(request.Ref) {
		options.ReferenceName = plumbing.NewBranchReferenceName(request.Ref)
		refs, err := git.NewRemote(nil, &config.RemoteConfig{Name: "origin", URLs: []string{remoteURL}}).ListContext(ctx, &git.ListOptions{Auth: options.Auth, Timeout: int(timeout.Seconds()), PeelingOption: git.AppendPeeled, ProxyOptions: proxy, CABundle: engine.config.CABundle})
		if err != nil {
			return CloneResult{}, err
		}
		branchExists, tagExists := false, false
		for _, ref := range refs {
			branchExists = branchExists || ref.Name() == plumbing.NewBranchReferenceName(request.Ref)
			tagExists = tagExists || ref.Name() == plumbing.NewTagReferenceName(request.Ref)
		}
		if !branchExists && tagExists {
			options.ReferenceName = plumbing.NewTagReferenceName(request.Ref)
			options.Tags = git.AllTags
		}
	}
	cloned, err := git.PlainCloneContext(ctx, request.Destination, false, options)
	if err != nil {
		return CloneResult{}, err
	}
	worktree, err := cloned.Worktree()
	if err != nil {
		return CloneResult{}, err
	}
	if commit != "" {
		if !isCommit(commit) {
			return CloneResult{}, ErrInvalidReference
		}
		if _, err := cloned.CommitObject(plumbing.NewHash(commit)); err != nil {
			remote, remoteErr := cloned.Remote("origin")
			if remoteErr != nil {
				return CloneResult{}, fmt.Errorf("%w: %v", ErrCommitUnavailableAtDepth, remoteErr)
			}
			fetchErr := remote.FetchContext(ctx, &git.FetchOptions{
				RemoteName: "origin", RefSpecs: []config.RefSpec{config.RefSpec("+" + commit + ":refs/gitops/commit")},
				Depth: depth, Auth: auth, CABundle: engine.config.CABundle, ProxyOptions: proxy, Tags: git.NoTags,
			})
			if fetchErr != nil && !errors.Is(fetchErr, git.NoErrAlreadyUpToDate) {
				return CloneResult{}, fmt.Errorf("%w: %v", ErrCommitUnavailableAtDepth, fetchErr)
			}
			if _, err := cloned.CommitObject(plumbing.NewHash(commit)); err != nil {
				return CloneResult{}, ErrCommitUnavailableAtDepth
			}
		}
		checkout := &git.CheckoutOptions{Hash: plumbing.NewHash(commit), Force: true, SparseCheckoutDirectories: request.SparsePaths}
		if err := worktree.Checkout(checkout); err != nil {
			return CloneResult{}, err
		}
	} else if len(request.SparsePaths) > 0 {
		head, err := cloned.Head()
		if err != nil {
			return CloneResult{}, err
		}
		checkout := &git.CheckoutOptions{Force: true, SparseCheckoutDirectories: request.SparsePaths}
		if head.Name().IsBranch() {
			checkout.Branch = head.Name()
		} else {
			checkout.Hash = head.Hash()
		}
		if err := worktree.Checkout(checkout); err != nil {
			return CloneResult{}, err
		}
	}
	if remoteURL != repository.URL {
		if err := setCloneOrigin(cloned, repository.URL); err != nil {
			return CloneResult{}, err
		}
	}
	head, err := cloned.Head()
	if err != nil {
		return CloneResult{}, err
	}
	resolvedRef := request.Ref
	if resolvedRef == "" {
		resolvedRef = "HEAD"
	}
	return CloneResult{Repository: repository, Commit: head.Hash().String(), Engine: engine.Name(), Path: request.Destination, ResolvedRef: resolvedRef}, nil
}

func (goGitEngine) Checkout(ctx context.Context, request CheckoutRequest) (CheckoutResult, error) {
	if err := ctx.Err(); err != nil {
		return CheckoutResult{}, err
	}
	if request.Commit != "" && !isCommit(request.Commit) {
		return CheckoutResult{}, ErrInvalidReference
	}
	if err := validateRequestedRef(request.Ref); err != nil {
		return CheckoutResult{}, err
	}
	repository, err := git.PlainOpen(request.Path)
	if err != nil {
		return CheckoutResult{}, err
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return CheckoutResult{}, err
	}
	if request.Commit != "" {
		err = worktree.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(request.Commit), Force: true})
	} else {
		name := plumbing.NewBranchReferenceName(request.Ref)
		tagName := plumbing.NewTagReferenceName(request.Ref)
		if _, branchErr := repository.Reference(name, true); branchErr != nil {
			if _, err := repository.Reference(tagName, true); err == nil {
				name = tagName
			}
		}
		if strings.HasPrefix(name.String(), "refs/tags/") {
			hash, resolveErr := repository.ResolveRevision(plumbing.Revision(name.String()))
			if resolveErr != nil {
				return CheckoutResult{}, resolveErr
			}
			err = worktree.Checkout(&git.CheckoutOptions{Hash: *hash, Force: true})
		} else {
			err = worktree.Checkout(&git.CheckoutOptions{Branch: name, Force: true})
		}
	}
	if err != nil {
		return CheckoutResult{}, err
	}
	head, err := repository.Head()
	if err != nil {
		return CheckoutResult{}, err
	}
	return CheckoutResult{Path: request.Path, Commit: head.Hash().String(), Engine: "go-git"}, nil
}

func goGitOpen(path string) (*git.Repository, error) { return git.PlainOpen(path) }

func authMethod(repository Repository, credential Credential, config Config, timeout time.Duration) (transport.AuthMethod, error) {
	if repository.Transport == "https" {
		if credential.Token == "" && credential.Password == "" {
			return nil, nil
		}
		username := credential.Username
		if username == "" {
			username = "git"
		}
		password := credential.Token
		if password == "" {
			password = credential.Password
		}
		return &gitHTTP.BasicAuth{Username: username, Password: password}, nil
	}
	if repository.Transport != "ssh" {
		return nil, ErrUnsupportedTransport
	}
	if credential.SSHPrivateKeyPath != "" && len(credential.SSHPrivateKey) > 0 {
		return nil, fmt.Errorf("SSH private key bytes and path are mutually exclusive")
	}
	var discovered discoveredSSHConfig
	if config.DiscoverSSHKeys {
		var err error
		discovered, err = discoverSSHConfig(repository, config)
		if err != nil {
			return nil, err
		}
	}
	username := strings.TrimSpace(credential.Username)
	if username == "" {
		username = repositorySSHUser(repository.URL)
	}
	if username == "" {
		username = strings.TrimSpace(discovered.User)
	}
	if username == "" {
		username = "git"
	}
	var authMethods []ssh.AuthMethod
	var err error
	if credential.SSHPrivateKeyPath != "" || len(credential.SSHPrivateKey) > 0 {
		var auth *gitSSH.PublicKeys
		if credential.SSHPrivateKeyPath != "" {
			auth, err = gitSSH.NewPublicKeysFromFile(username, credential.SSHPrivateKeyPath, credential.Passphrase)
		} else {
			auth, err = gitSSH.NewPublicKeys(username, credential.SSHPrivateKey, credential.Passphrase)
		}
		if err != nil {
			return nil, fmt.Errorf("load SSH private key: %w", err)
		}
		authMethods = []ssh.AuthMethod{ssh.PublicKeys(auth.Signer)}
	} else {
		if !config.DiscoverSSHKeys {
			return nil, ErrCredentialNotConfigured
		}
		signers, err := loadSSHIdentitySigners(discovered.IdentityFiles, config)
		if err != nil {
			return nil, err
		}
		authMethods = []ssh.AuthMethod{ssh.PublicKeys(signers...)}
	}
	callback, err := config.sshHostKeyCallback()
	if err != nil {
		return nil, err
	}
	clientConfig := &ssh.ClientConfig{User: username, Auth: authMethods, HostKeyCallback: callback}
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	clientConfig.Timeout = timeout
	return &goGitSSHAuth{clientConfig: clientConfig, timeout: timeout}, nil
}

func goGitRemoteURL(repository Repository, config Config, credential Credential) (string, error) {
	if repository.Transport == "https" {
		parsed, err := url.Parse(repository.URL)
		if err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" {
			parsed.Scheme = gitOpsHTTPSProtocol
			return parsed.String(), nil
		}
		return repository.URL, nil
	}
	if repository.Transport != "ssh" {
		return repository.URL, nil
	}
	var discovered discoveredSSHConfig
	if config.DiscoverSSHKeys {
		var err error
		discovered, err = discoverSSHConfig(repository, config)
		if err != nil {
			return "", err
		}
	}
	alias, explicitPort, err := sshAliasAndPort(repository.URL)
	if err != nil {
		return "", err
	}
	host := firstNonEmpty(discovered.HostName, alias)
	port := firstNonEmpty(explicitPort, discovered.Port)
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	user := firstNonEmpty(strings.TrimSpace(credential.Username), repositorySSHUser(repository.URL), strings.TrimSpace(discovered.User), "git")
	if strings.Contains(repository.URL, "://") {
		parsed, err := url.Parse(repository.URL)
		if err != nil {
			return "", ErrInvalidURL
		}
		parsed.Scheme = gitOpsSSHProtocol
		parsed.Host = host
		if parsed.User == nil {
			parsed.User = url.User(user)
		}
		return parsed.String(), nil
	}
	_, path, found := strings.Cut(repository.URL, ":")
	if !found {
		return "", ErrInvalidURL
	}
	return (&url.URL{Scheme: gitOpsSSHProtocol, User: url.User(user), Host: host, Path: "/" + path}).String(), nil
}

func setCloneOrigin(repository *git.Repository, originalURL string) error {
	configuration, err := repository.Config()
	if err != nil {
		return err
	}
	origin := configuration.Remotes["origin"]
	if origin == nil {
		return errors.New("cloned repository has no origin remote")
	}
	origin.URLs = []string{originalURL}
	return repository.SetConfig(configuration)
}

func repositorySSHUser(repositoryURL string) string {
	if strings.Contains(repositoryURL, "://") {
		parsed, err := url.Parse(repositoryURL)
		if err == nil && parsed.User != nil {
			return parsed.User.Username()
		}
		return ""
	}
	left, _, found := strings.Cut(repositoryURL, ":")
	if !found {
		return ""
	}
	user, _, found := strings.Cut(left, "@")
	if found {
		return user
	}
	return ""
}
