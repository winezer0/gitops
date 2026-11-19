package gitops

import (
	"context"
	"fmt"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitHTTP "github.com/go-git/go-git/v5/plumbing/transport/http"
)

type goGitEngine struct{}

func (goGitEngine) Name() string { return "go-git" }

func (engine goGitEngine) Probe(ctx context.Context, repository Repository, request ProbeRequest, credential Credential, timeout time.Duration, proxyURL string) (ProbeResult, error) {
	proxy, err := parseProxyURL(proxyURL)
	if err != nil {
		return ProbeResult{}, err
	}
	remote := git.NewRemote(nil, &config.RemoteConfig{Name: "origin", URLs: []string{repository.URL}})
	options := &git.ListOptions{Auth: authMethod(repository, credential), Timeout: int(timeout.Seconds()), PeelingOption: git.AppendPeeled, ProxyOptions: proxy}
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
	if repository.Transport != "https" || len(credential.SSHPrivateKey) > 0 {
		return CloneResult{}, ErrUnsupported
	}
	if len(request.SparsePaths) > 0 {
		return CloneResult{}, ErrUnsupported
	}
	proxy, err := parseProxyURL(proxyURL)
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
	if commit != "" {
		depth = 0
	}
	options := &git.CloneOptions{URL: repository.URL, Auth: authMethod(repository, credential), Depth: depth, NoCheckout: false, RecurseSubmodules: git.NoRecurseSubmodules, Tags: git.NoTags, SingleBranch: request.Ref != "" && !isCommit(request.Ref), ProxyOptions: proxy}
	if request.OnProgress != nil || request.OnEvent != nil {
		options.Progress = &progressWriter{engine: engine.Name(), operation: "clone", callback: request.OnProgress, observer: request.OnEvent}
	}
	if request.Ref != "" && !isCommit(request.Ref) {
		options.ReferenceName = plumbing.NewBranchReferenceName(request.Ref)
		refs, err := git.NewRemote(nil, &config.RemoteConfig{Name: "origin", URLs: []string{repository.URL}}).ListContext(ctx, &git.ListOptions{Auth: options.Auth, Timeout: int(timeout.Seconds()), PeelingOption: git.AppendPeeled, ProxyOptions: proxy})
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
			return CloneResult{}, err
		}
		if err := worktree.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(commit), Force: true}); err != nil {
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
	repository, err := git.PlainOpen(request.Path)
	if err != nil {
		return CheckoutResult{}, err
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return CheckoutResult{}, err
	}
	if request.Commit != "" {
		if !isCommit(request.Commit) {
			return CheckoutResult{}, ErrInvalidReference
		}
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

func authMethod(repository Repository, credential Credential) transport.AuthMethod {
	if repository.Transport != "https" || credential.Token == "" && credential.Password == "" {
		return nil
	}
	username := credential.Username
	if username == "" {
		username = "git"
	}
	password := credential.Token
	if password == "" {
		password = credential.Password
	}
	return &gitHTTP.BasicAuth{Username: username, Password: password}
}
