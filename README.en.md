# GitOps

GitOps is a structured Go package for probing and cloning HTTPS and SSH Git repositories.

## Features

GitOps provides the following operations:

- `ParseURL`: parses and validates repository URLs.
- `Client.Probe`: probes a repository and resolves branches, tags, and commit references.
- `Client.Clone`: clones a repository with configurable ref, shallow depth, sparse paths, and recursive submodules.
- `Client.Checkout`: checks out a branch, tag, or commit.
- `Client.ValidateCheckout`: validates a local worktree's remote URL, commit, and clean status.
- `Client.TestCredential`: checks whether credentials can read a specified repository.
- `Client.TestHostCredential`: independently tests provider account credentials or SSH credentials.

`TestCredential` checks whether credentials can access a specified repository. `TestHostCredential` does not require a repository path: HTTPS credentials are checked through the selected provider's user API, while SSH credentials are checked with an OpenSSH handshake that enforces strict host-key verification. A successful provider API check confirms provider authentication, not access to a particular private repository. Use `Probe` or `TestCredential` to check repository access.

Provider API requests do not follow redirects, preventing tokens from being forwarded to redirect targets. Custom API endpoints must use HTTPS and must not include user information, query parameters, or fragments.

## Quick start

```go
client, err := gitops.NewClient(gitops.Config{
    AllowedHosts: []string{"github.com", "git.example.org"},
    Timeout:      2 * time.Minute,
    CredentialFor: func(host string) (gitops.Credential, error) {
        return credentials.ForHost(host)
    },
    ValidateHost: ssrfPolicy.ValidateGitHost,
})
if err != nil {
    return err
}

result, err := client.Probe(ctx, gitops.ProbeRequest{URL: repositoryURL, Ref: "main"})
```

## Credentials and security

`CredentialFor` should return credentials only for the normalized host passed to it. HTTPS credentials use a username and token/password; SSH credentials use unencrypted private-key bytes. Encrypted SSH private keys are currently unsupported and return an error.

System Git enforces HTTPS certificate validation and strict SSH `known_hosts` verification while retaining OpenSSH host configuration. Do not put keys or tokens in repository URLs.

A hostname-only entry in `AllowedHosts` permits custom ports on that host. An entry that includes a port restricts access to that port. Server applications should configure both `AllowedHosts` and `ValidateHost` to apply DNS and SSRF policies before repository, provider API, and submodule requests.

System Git credential helpers, including Git Credential Manager, are disabled by default. Set `Config.UseCredentialHelpers` to allow configured helpers when the caller has not supplied explicit HTTPS credentials. An explicit token or password disables the helper for that operation. The go-git fallback cannot use system credential helpers.

## Clone behavior

Each operation is bounded by `Config.Timeout` and the caller's `context.Context`. Clone defaults to depth 1 and a 5 GiB workspace size limit (`MaxCloneBytes`). The destination must not already exist unless `ReplaceExisting` is set. `ReusePinned` reuses a matching clean checkout only when a fixed commit was requested. `DestinationRoot` can restrict clone paths. Results identify the engine used.

`SparsePaths` contains repository-root-relative directories, for example `[]string{"cmd", "internal/source"}`. With system Git, GitOps uses `--filter=blob:none` and cone-mode sparse checkout, so file contents for selected paths are downloaded up front while repository metadata such as commits and trees is still fetched. An empty list checks out the full repository. Sparse clone is unsupported by the go-git fallback.

Submodule recursion is disabled by default. When `CloneRequest.RecurseSubmodules` is set, GitOps clones each submodule at the gitlink commit recorded by its parent; submodule branches are not advanced. The root clone depth is passed to every submodule. Relative URLs are resolved against the parent repository URL, and every resolved submodule URL receives the same host, SSRF, and credential checks as the root. The timeout and clone size limit cover the entire workspace. Recursion beyond 32 levels returns a resource-limit error. `Probe` checks only the root repository and requested ref; submodule access is checked during recursive cloning.

When a go-git fallback checks out a pinned commit, it may fetch the full reachable history because go-git transports may reject raw SHA refspecs. The clone size monitor remains active.

## Engines and fallback

GitOps invokes system Git through processkit first. It falls back to go-git for HTTPS repositories only when the Git executable cannot be found. SSH repositories do not fall back to go-git, and sparse clone requests are unsupported in fallback mode. Authentication, ref resolution, network, TLS, timeout, and cancellation failures do not trigger a retry with another engine.

GitOps does not automatically retry with another engine after authentication fails. This prevents credential, host-verification, or repository-permission problems from being masked by a different engine.

## Progress, diagnostics, and task logs

`ProbeRequest`, `CloneRequest`, and `CredentialTestRequest` accept an `OnEvent` callback for structured progress, process snapshots, and safe diagnostics. Process snapshots omit arguments, environment values, and command output. The caller owns task-log persistence and user-facing formatting.

`OperationError.Error()` and `Diagnostic` do not include raw process output or credentials. GitOps implements provider API routing and authentication checks; callers remain responsible for credential storage and lookup. `ProgressEvent` exposes only the engine, phase, and percentage. `ErrorCode` returns a stable, redacted error category.

```go
result, err := client.Clone(ctx, gitops.CloneRequest{
    URL:         repositoryURL,
    Destination: checkoutPath,
    Ref:         "main",
    Depth:       1,
    SparsePaths: []string{"cmd", "internal/source"},
    OnEvent: func(event gitops.OperationEvent) {
        if event.Kind == gitops.EventProgress {
            reportProgress(event.Percent, event.ReceivedObjects, event.TotalObjects)
        }
    },
})
```

## Compatibility matrix

The following results were verified on 2026-11-20:

| Service | Transport / credentials | Probe | Shallow clone | Notes |
| --- | --- | --- | --- | --- |
| GitHub.com | HTTPS, anonymous | Pass | Pass | Public repository |
| GitLab.com | HTTPS, anonymous | Pass | Pass | Public repository |
| Gitea.com | HTTPS, anonymous | Pass | Pass | Public repository |
| Gitee | HTTPS, anonymous | Pass | Pass | Public repository |
| GitHub.com | HTTPS, go-git fallback | Pass | Pass | System Git was disabled for this check |
| `git.imgo.tv/ott/inott-epg` | HTTPS, Git Credential Manager | Pass | Pass | Private repository; GCM supplied the username and token in process memory |
| `git.imgo.tv/ott/inott-epg` | SSH, identity selected by OpenSSH config | Pass | Pass | Strict host-key verification enabled |
| `git.imgo.tv` | SSH, empty known_hosts | Rejected | N/A | Strict host-key rejection verified |
| Self-managed Gitea / GitLab private instances | HTTPS | Not tested | Not tested | No other authorized private repository was available |

Private HTTPS negative checks also passed for an invalid token, anonymous access, cancellation, and timeout. Error categories are stable and contain no credential material. Authentication failures do not trigger go-git fallback; fallback is limited to a missing system Git executable.

## Local verification

```sh
go test ./...
go vet ./...
```

The default test suite needs no external repositories or credentials. It covers provider API routing and redirect rejection, no fallback after authentication failure, rejecting unknown SSH host keys before user authentication, branch/tag/commit resolution in both engines, and clone destination cleanup after authentication failure, cancellation, size-limit, and invalid-ref errors. GitHub Actions runs `go test ./...` and `go vet ./...` on Windows and Linux using the Go version declared in `go.mod`.

## Optional external compatibility tests

Provide test repositories through one of these environment variables: `GITOPS_TEST_GITHUB_URL`, `GITOPS_TEST_GITLAB_URL`, `GITOPS_TEST_GITEA_URL`, `GITOPS_TEST_GITEE_URL`, or `GITOPS_TEST_PRIVATE_URL`. Each accepts optional `_REF`, `_USERNAME`, `_TOKEN`, `_PASSWORD`, and `_SSH_KEY_FILE` variables. Set `GITOPS_TEST_GIT_BINARY` to a missing executable to exercise HTTPS fallback. Tests probe and clone the configured URL, then compare the resolved commit; each case has a 45-second timeout. Cases are skipped when no URL is configured.

For a private HTTPS repository with credentials already configured in Git Credential Manager, load credentials into the test process without printing them:

```powershell
$hostName = 'git.imgo.tv'
$repoPath = 'ott/inott-epg.git'
$credentialInput = "protocol=https`nhost=$hostName`npath=$repoPath`n`n"
$credentialLines = $credentialInput | git credential fill
$credential = @{}
foreach ($line in $credentialLines) {
    $name, $value = $line -split '=', 2
    $credential[$name] = $value
}
$env:GITOPS_TEST_PRIVATE_URL = "https://$hostName/$repoPath"
$env:GITOPS_TEST_PRIVATE_USERNAME = $credential.username
$env:GITOPS_TEST_PRIVATE_TOKEN = $credential.password
$env:GITOPS_TEST_PRIVATE_NEGATIVE = '1'
go test -run 'TestConfiguredPlatformCompatibility/PRIVATE' -count=1
Remove-Item Env:GITOPS_TEST_PRIVATE_URL, Env:GITOPS_TEST_PRIVATE_USERNAME, Env:GITOPS_TEST_PRIVATE_TOKEN, Env:GITOPS_TEST_PRIVATE_NEGATIVE
```

The test reads credentials from the process environment and reports only redacted error categories. Do not enable shell tracing, print the populated credential map, or redirect output containing credentials to a file. Remove the temporary environment variables when the test finishes.

For SSH, set `GITOPS_TEST_PRIVATE_URL` to an SSH clone URL, such as `git@git.example.org:team/repo.git`. OpenSSH can select an identity from its config, or set `GITOPS_TEST_PRIVATE_SSH_KEY_FILE` to an unencrypted key file. Host-key verification remains strict; unknown keys must be added to `known_hosts` through a trusted process.
