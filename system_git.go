package gitops

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/winezer0/processkit/processrunner"
)

type systemEngine struct {
	binary               string
	useCredentialHelpers bool
}

func (engine systemEngine) Name() string { return "system-git" }

func (engine systemEngine) Probe(ctx context.Context, repository Repository, request ProbeRequest, credential Credential, timeout time.Duration, proxyURL string) (ProbeResult, error) {
	environment, cleanup, err := credentialEnvironment(repository, credential, engine.useCredentialHelpers)
	if err != nil {
		return ProbeResult{}, err
	}
	defer cleanup()
	environment, err = withProxy(environment, proxyURL)
	if err != nil {
		return ProbeResult{}, err
	}
	result, err := engine.run(ctx, environment, request.OnEvent, "probe", "ls-remote", "--symref", repository.URL)
	if errors.Is(err, exec.ErrNotFound) {
		return ProbeResult{}, ErrGitUnavailable
	}
	if err != nil {
		return ProbeResult{}, commandError(result.Output, err)
	}
	return parseReferences(repository, request.Ref, result.Output, engine.Name())
}

func (engine systemEngine) Clone(ctx context.Context, repository Repository, request CloneRequest, credential Credential, timeout time.Duration, proxyURL string) (CloneResult, error) {
	environment, cleanup, err := credentialEnvironment(repository, credential, engine.useCredentialHelpers)
	if err != nil {
		return CloneResult{}, err
	}
	defer cleanup()
	environment, err = withProxy(environment, proxyURL)
	if err != nil {
		return CloneResult{}, err
	}
	depth := request.Depth
	if depth == 0 {
		depth = 1
	}
	args := []string{"clone", "-c", "core.longpaths=true", "--depth", fmt.Sprint(depth), "--no-tags", "--no-recurse-submodules"}
	if request.OnProgress != nil || request.OnEvent != nil {
		args = append(args, "--progress")
	}
	if len(request.SparsePaths) > 0 {
		args = append(args, "--filter=blob:none", "--sparse")
	}
	if request.Ref != "" && !isCommit(request.Ref) {
		if strings.HasPrefix(request.Ref, "-") {
			return CloneResult{}, ErrInvalidReference
		}
		args = append(args, "--branch", request.Ref)
	}
	args = append(args, repository.URL, request.Destination)
	cloneResult, err := engine.runClone(ctx, environment, request, args...)
	if errors.Is(err, exec.ErrNotFound) {
		return CloneResult{}, ErrGitUnavailable
	} else if err != nil {
		return CloneResult{}, commandError(cloneResult.Output, err)
	}
	if len(request.SparsePaths) > 0 {
		args := append([]string{"sparse-checkout", "set", "--cone"}, request.SparsePaths...)
		if _, err := engine.runDirectory(ctx, request.Destination, request.OnEvent, "clone", args...); err != nil {
			return CloneResult{}, commandError(nil, err)
		}
	}
	commit := request.Commit
	if commit == "" && isCommit(request.Ref) {
		commit = request.Ref
	}
	if commit != "" {
		if !isCommit(commit) {
			return CloneResult{}, ErrInvalidReference
		}
		if _, err := engine.runDirectory(ctx, request.Destination, request.OnEvent, "clone", "fetch", "--depth", fmt.Sprint(depth), "origin", commit); err != nil {
			return CloneResult{}, err
		}
		if _, err := engine.runDirectory(ctx, request.Destination, request.OnEvent, "clone", "checkout", "--detach", commit); err != nil {
			return CloneResult{}, err
		}
	}
	result, err := engine.runDirectory(ctx, request.Destination, request.OnEvent, "clone", "rev-parse", "HEAD")
	if err != nil {
		return CloneResult{}, err
	}
	resolvedRef := strings.TrimSpace(request.Ref)
	if resolvedRef == "" || isCommit(resolvedRef) {
		branch, branchErr := engine.runDirectory(ctx, request.Destination, request.OnEvent, "clone", "rev-parse", "--abbrev-ref", "HEAD")
		if branchErr == nil {
			resolvedRef = strings.TrimSpace(string(branch.Output))
		}
	}
	return CloneResult{Repository: repository, Commit: strings.TrimSpace(string(result.Output)), Engine: engine.Name(), Path: request.Destination, ResolvedRef: resolvedRef}, nil
}

func (engine systemEngine) runClone(ctx context.Context, environment map[string]string, request CloneRequest, args ...string) (processrunner.Result, error) {
	if _, err := exec.LookPath(engine.binary); err != nil {
		return processrunner.Result{}, exec.ErrNotFound
	}
	var progress *progressWriter
	var stderr io.Writer
	if request.OnProgress != nil || request.OnEvent != nil {
		progress = &progressWriter{engine: engine.Name(), operation: "clone", callback: request.OnProgress, observer: request.OnEvent}
		stderr = progress
	}
	result, err := (processrunner.ProcessRunner{}).Run(ctx, processrunner.Invocation{
		Name: "git clone", Binary: engine.binary, Args: args, Environment: environment,
		Stderr: stderr, CaptureBytes: 1 << 20, RequireCompleteOutput: true,
	}, processrunner.RunOptions{})
	if progress != nil {
		progress.Flush()
	}
	emitProcessEvent(request.OnEvent, "clone", engine.Name(), result)
	return result, err
}

func (engine systemEngine) Checkout(ctx context.Context, request CheckoutRequest) (CheckoutResult, error) {
	ref := request.Commit
	args := []string{"checkout", "--force"}
	if ref != "" {
		args = append(args, "--detach")
	} else {
		ref = request.Ref
	}
	args = append(args, ref)
	if _, err := engine.runDirectory(ctx, request.Path, request.OnEvent, "checkout", args...); errors.Is(err, exec.ErrNotFound) {
		return CheckoutResult{}, ErrGitUnavailable
	} else if err != nil {
		return CheckoutResult{}, err
	}
	result, err := engine.runDirectory(ctx, request.Path, request.OnEvent, "checkout", "rev-parse", "HEAD")
	if err != nil {
		return CheckoutResult{}, err
	}
	return CheckoutResult{Path: request.Path, Commit: strings.TrimSpace(string(result.Output)), Engine: engine.Name()}, nil
}

func (engine systemEngine) run(ctx context.Context, environment map[string]string, observer func(OperationEvent), operation string, args ...string) (processrunner.Result, error) {
	if _, err := exec.LookPath(engine.binary); err != nil {
		return processrunner.Result{}, exec.ErrNotFound
	}
	result, err := (processrunner.ProcessRunner{}).Run(ctx, processrunner.Invocation{
		Name: "git", Binary: engine.binary, Args: args, Environment: environment,
		CaptureBytes: 1 << 20, RequireCompleteOutput: true,
	}, processrunner.RunOptions{})
	emitProcessEvent(observer, operation, engine.Name(), result)
	return result, err
}

func (engine systemEngine) runDirectory(ctx context.Context, directory string, observer func(OperationEvent), operation string, args ...string) (processrunner.Result, error) {
	if _, err := exec.LookPath(engine.binary); err != nil {
		return processrunner.Result{}, exec.ErrNotFound
	}
	result, err := (processrunner.ProcessRunner{}).Run(ctx, processrunner.Invocation{
		Name: "git", Binary: engine.binary, Args: args, Directory: directory,
		CaptureBytes: 1 << 20, RequireCompleteOutput: true,
	}, processrunner.RunOptions{})
	emitProcessEvent(observer, operation, engine.Name(), result)
	return result, err
}

func withProxy(environment map[string]string, proxyURL string) (map[string]string, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return environment, nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid Git proxy URL")
	}
	if environment == nil {
		environment = make(map[string]string)
	}
	count := 0
	if value := environment["GIT_CONFIG_COUNT"]; value != "" {
		if _, err := fmt.Sscan(value, &count); err != nil || count < 0 {
			return nil, fmt.Errorf("invalid Git configuration environment")
		}
	}
	environment[fmt.Sprintf("GIT_CONFIG_KEY_%d", count)] = "http.proxy"
	environment[fmt.Sprintf("GIT_CONFIG_VALUE_%d", count)] = proxyURL
	environment["GIT_CONFIG_COUNT"] = fmt.Sprint(count + 1)
	environment["HTTP_PROXY"], environment["HTTPS_PROXY"] = proxyURL, proxyURL
	environment["http_proxy"], environment["https_proxy"] = proxyURL, proxyURL
	return environment, nil
}

func credentialEnvironment(repository Repository, credential Credential, useCredentialHelpers bool) (map[string]string, func(), error) {
	if credential.Passphrase != "" {
		return nil, nil, fmt.Errorf("encrypted SSH keys require an agent and are not supported by this client")
	}
	secret := credential.Token
	if secret == "" {
		secret = credential.Password
	}
	username := credential.Username
	if username == "" {
		username = "git"
	}
	environment := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	configCount := 0
	if repository.Transport == "https" {
		if !useCredentialHelpers || secret != "" {
			environment[fmt.Sprintf("GIT_CONFIG_KEY_%d", configCount)] = "credential.helper"
			environment[fmt.Sprintf("GIT_CONFIG_VALUE_%d", configCount)] = ""
			configCount++
		}
		environment[fmt.Sprintf("GIT_CONFIG_KEY_%d", configCount)] = "http." + repository.URL + ".sslVerify"
		environment[fmt.Sprintf("GIT_CONFIG_VALUE_%d", configCount)] = "true"
		configCount++
	}
	if secret != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + secret))
		if repository.Transport == "https" {
			environment[fmt.Sprintf("GIT_CONFIG_KEY_%d", configCount)] = "http." + repository.URL + ".extraheader"
			environment[fmt.Sprintf("GIT_CONFIG_VALUE_%d", configCount)] = "Authorization: Basic " + auth
			configCount++
		}
	}
	if configCount > 0 {
		environment["GIT_CONFIG_COUNT"] = fmt.Sprint(configCount)
	}
	cleanup := func() {}
	sshOptions := "ssh -o BatchMode=yes -o StrictHostKeyChecking=yes"
	if len(credential.SSHPrivateKey) > 0 {
		directory, err := os.MkdirTemp("", "gitops-ssh-")
		if err != nil {
			return nil, nil, err
		}
		keyPath := filepath.Join(directory, "id_key")
		if err := os.WriteFile(keyPath, credential.SSHPrivateKey, 0o600); err != nil {
			_ = os.RemoveAll(directory)
			return nil, nil, err
		}
		sshOptions += " -i \"" + keyPath + "\" -o IdentitiesOnly=yes"
		cleanup = func() { _ = os.RemoveAll(directory) }
	}
	if repository.Transport == "ssh" {
		environment["GIT_SSH_COMMAND"] = sshOptions
	}
	return environment, cleanup, nil
}
