# GitOps

[简体中文](README.md)

GitOps is a Go package for controlled probing, cloning, and validation of HTTPS and SSH Git repositories inside applications. It provides remote reference resolution, bounded cloning, worktree validation, and credential checks; callers provide credential storage and host/DNS security policies.

## Features

GitOps provides the following operations:

- `ParseURL`: parses and validates repository URLs.
- `Client.Probe`: probes a repository and resolves branches, tags, and commit references.
- `Client.Clone`: clones a repository with configurable ref, shallow depth, sparse paths, and recursive submodules.
- `Client.Checkout`: checks out a branch, tag, or commit.
- `Client.ValidateCheckout`: validates a local worktree's remote URL, commit, and clean status.
- `Client.TestCredential`: checks whether credentials can read a specified repository.
- `Client.TestHostCredential`: supports GitHub, GitLab, Gitea, and configured custom HTTPS user APIs; verifies SSH keys with a host-key-verified handshake.

`TestCredential` checks whether credentials can access a specified repository. `TestHostCredential` does not require a repository path: HTTPS credentials are checked through the selected provider's user API, while SSH credentials are checked with a Go SSH handshake that enforces strict host-key verification. Results expose separate `CredentialStatus` and `ProfileAccess` fields. Failed checks return the result together with `*CredentialTestError`; inspect `Diagnostic.Code` for the specific reason. A successful provider API check does not prove access to a particular private repository. Use `Probe` or `TestCredential` to check repository access.

Callers should inspect both the result and the error. For example, a profile API scope denial can have `CredentialStatus` set to `accepted` while `ProfileAccess` is `insufficient_scope`:

```go
result, err := client.TestHostCredential(ctx, request)
if err != nil {
	var testErr *gitops.CredentialTestError
	if errors.As(err, &testErr) {
		log.Printf("credential=%s profile_access=%s code=%s",
			result.CredentialStatus, result.ProfileAccess, testErr.Diagnostic.Code)
	}
	return err
}
log.Printf("credential=%s profile_access=%s", result.CredentialStatus, result.ProfileAccess)
```

The caller supplies `ctx`, `client`, and `request`; import `errors`, `log`, and `github.com/winezer0/gitops`.

Provider API requests do not follow redirects, preventing tokens from being forwarded to redirect targets. Custom API endpoints must use HTTPS and must not include user information, query parameters, or fragments.

## Quick start

The snippet assumes it is inside a caller function that returns `error`; the application provides `ctx`, `repositoryURL`, `credentials`, and `ssrfPolicy`. It uses a repository-scoped credential callback and assumes imports for `fmt` and `github.com/winezer0/gitops`.

```go
client, err := gitops.NewClient(gitops.Config{
    AllowedHosts: []string{"github.com", "git.example.org"},
    Timeout:      2 * time.Minute,
    CredentialForRepository: func(repository gitops.Repository) (gitops.Credential, error) {
        return credentials.ForRepository(repository.Host, repository.Path)
    },
    ValidateHost: ssrfPolicy.ValidateGitHost,
})
if err != nil {
    return err
}

result, err := client.Probe(ctx, gitops.ProbeRequest{
    URL: repositoryURL, Ref: "main",
})
if err != nil {
    return err
}
fmt.Printf("resolved commit: %s\n", result.ResolvedCommit)
```

## Credentials and security

Credentials are resolved in this order: the request's explicit credential, `CredentialForRepository` using the normalized transport/host/port/path, then the legacy host-only `CredentialFor` callback when the repository callback reports `ErrCredentialNotConfigured`. Repository callbacks should return credentials only for the requested repository scope. HTTPS credentials use a username and token/password. SSH credentials can use private-key bytes or a private-key path, with an optional passphrase.

The Go engine verifies HTTPS certificates by default and requires an explicit SSH host-key trust source, such as configured `known_hosts` files or an application callback. Local SSH config and key discovery are opt-in through `DiscoverSSHKeys`. Do not put keys or tokens in repository URLs.

A hostname-only entry in `AllowedHosts` permits custom ports on that host. An entry that includes a port restricts access to that port. Server applications should configure both `AllowedHosts` and `ValidateHost` to apply DNS and SSRF policies before repository, provider API, and submodule requests.

System credential helpers are not supported. `Config.UseCredentialHelpers` remains for source compatibility and causes `NewClient` to return `ErrCredentialHelpersUnsupported` when enabled. Integrations should load credentials through a request or credential callback.

## Clone behavior

Network probes, clones, credential checks, and ref checkouts use the caller's `context.Context` and obey request or client timeouts. `ValidateCheckout` is a synchronous local check and does not accept a context. Clone defaults to depth 1 and a 5 GiB workspace size limit (`MaxCloneBytes`). The destination must not already exist unless `ReplaceExisting` is set. `ReusePinned` reuses a matching clean checkout only when a fixed commit was requested. `DestinationRoot` can restrict clone paths. Results identify the engine used.

`SparsePaths` contains repository-root-relative directories, for example `[]string{"cmd", "internal/source"}`. The Go engine materializes only the selected directories in the worktree; the current go-git transport may still download repository objects for other paths. An empty list checks out the full repository.

Submodule recursion is disabled by default. When `CloneRequest.RecurseSubmodules` is set, GitOps clones each submodule at the gitlink commit recorded by its parent; submodule branches are not advanced. The root clone depth is passed to every submodule. Relative URLs are resolved against the parent repository URL, and every resolved submodule URL receives the same host, SSRF, and credential checks as the root. The timeout and clone size limit cover the entire workspace. Recursion beyond 32 levels returns a resource-limit error. `Probe` checks only the root repository and requested ref; submodule access is checked during recursive cloning.

Pinned commits are fetched within the requested shallow depth. If the commit is not available at that depth, cloning returns `ErrCommitUnavailableAtDepth` instead of silently downloading full history.

## Transport support

| Transport | Engine | Credentials | Server verification |
| --- | --- | --- | --- |
| HTTPS | go-git | Username and token/password | TLS certificates are verified by default |
| SSH | In-process Go SSH | Private-key bytes or path, optional passphrase | Strict `known_hosts` or application callback |

Runtime operations do not invoke system Git, OpenSSH, or processkit. Authentication, host verification, ref resolution, network, timeout, and cancellation errors are returned directly without retrying through another engine. This avoids masking credential, host-verification, or repository-permission failures.

## Progress, diagnostics, and task logs

`ProbeRequest`, `CloneRequest`, and `CredentialTestRequest` accept an `OnEvent` callback for structured progress and safe diagnostics. `CloneRequest.OnProgress` also reports the engine, phase, percentage, received/total object counts, and bytes received. The caller owns task-log persistence and user-facing formatting.

Error messages and `Diagnostic` do not include raw process output or credentials. GitOps implements provider API routing and authentication checks; callers remain responsible for credential storage and lookup. `ErrorCode` returns a stable, redacted error classification.

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

## Local verification

```sh
go test ./...
go vet ./...
```

The default test suite needs no external repositories or credentials. It covers provider API routing and redirect rejection, request/repository/legacy credential precedence, HTTPS and SSH operations, unknown SSH host-key rejection, branch/tag/commit resolution, sparse worktree materialization, and clone destination cleanup after failures. GitHub Actions runs `go test ./...` and `go vet ./...` on Windows and Linux using the Go version declared in `go.mod`.

## Optional external compatibility tests

Provide test repositories through one of these environment variables: `GITOPS_TEST_GITHUB_URL`, `GITOPS_TEST_GITLAB_URL`, `GITOPS_TEST_GITEA_URL`, `GITOPS_TEST_GITEE_URL`, or `GITOPS_TEST_PRIVATE_URL`. Each accepts optional `_REF`, `_USERNAME`, `_TOKEN`, `_PASSWORD`, and `_SSH_KEY_FILE` variables. Supply private credentials through your approved secret store or CI secret injection; the test reports only redacted error categories. By default, tests probe and clone the configured URL, then compare the resolved commit; each case has a 45-second timeout. Cases are skipped when no URL is configured.

Private-repository cases can be expanded with `GITOPS_TEST_PRIVATE_REF_MATRIX=1` (default ref, branches, tags, and commits), `GITOPS_TEST_PRIVATE_NEGATIVE=1` (missing/wrong credentials and refs), or `GITOPS_TEST_PRIVATE_USER_API=1` (GitLab user-profile API, which skips repository probing and cloning in that case). The ref matrix uses a 2-minute timeout.

For SSH, set `GITOPS_TEST_PRIVATE_URL` to an SSH clone URL, such as `git@git.example.org:team/repo.git`, and provide a private-key file. Set `GITOPS_TEST_SSH_DISCOVERY=1` to enable local SSH key discovery or `GITOPS_TEST_SSH_USER_KNOWN_HOSTS=1` to enable the user's `known_hosts`. Host-key verification remains strict; unknown keys must be added to a configured trust source through a trusted process.
