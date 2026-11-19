# gitops

Structured Go package for probing and cloning HTTPS/SSH Git repositories.

## Operations

The package exposes `ParseURL`, `Client.Probe`, `Client.Clone`, `Client.Checkout`, `Client.ValidateCheckout`, `Client.TestCredential`, and `Client.TestHostCredential`.

`TestCredential` checks read access to a supplied repository URL. `TestHostCredential` checks a saved host credential independently of a repository path: HTTPS credentials use the selected provider's user API, while SSH credentials use a strict OpenSSH handshake. Provider API success verifies provider authentication, not access to a particular private repository; use `Probe` or `TestCredential` to check repository access. Redirects are not followed, so an API token is never forwarded to a redirect target. Custom API endpoints must use HTTPS and must not include user information, query parameters, or fragments.

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

`CloneRequest.RecurseSubmodules` opts into recursive submodule cloning; its zero value is `false`. Recursive clones use each parent's recorded gitlink commit, so submodule branches are not advanced. The root `Depth` is passed to every child clone. Relative submodule URLs resolve against the parent repository URL, and each resolved child URL passes through the same host, SSRF, and credential checks as the root. The configured timeout and clone-size limit cover the entire workspace. Recursion stops with a resource-limit error after 32 nested levels. `Probe` only checks the requested root repository and ref; submodule access is checked during recursive cloning. A go-git fallback for a pinned commit can still fetch full history as noted below.

`CredentialFor` should return credentials only for the requested normalized host. HTTPS credentials use a username plus token/password; SSH credentials use unencrypted private key bytes. System Git forces TLS certificate validation for HTTPS and strict known-host validation for SSH while retaining the OpenSSH host configuration. Encrypted SSH keys requiring a passphrase are currently rejected. Do not put secrets in repository URLs. A bare `AllowedHosts` hostname also permits custom ports on that hostname; a host-and-port entry restricts access to that port.

System Git credential helpers, including Git Credential Manager, are disabled by default. Set `Config.UseCredentialHelpers` to allow them when no explicit HTTPS credential is returned. An explicit token or password disables the helper for that operation, keeping the configured credential in control. go-git fallback cannot use system credential helpers.

## Engines and fallback

The client invokes system Git through processkit first. If the Git executable cannot be found, it can fall back to go-git for HTTPS operations. It does not retry authentication, ref, network, TLS, timeout, or cancellation failures with a second engine. go-git does not receive SSH keys in fallback mode and does not support sparse clone requests. Keep `AllowedHosts` and `ValidateHost` configured in server applications; `ValidateHost` can apply DNS and SSRF policy before repository, provider API, and submodule requests.

Each operation is bounded by `Config.Timeout` and the caller context. Clone defaults to depth one and enforces a 5 GiB default size limit (`MaxCloneBytes`). It requires a new destination unless `ReplaceExisting` is set; `ReusePinned` reuses a matching clean checkout only for a pinned commit. `DestinationRoot` can restrict clone paths. `SparsePaths` enables `--filter=blob:none` and cone-mode sparse checkout in system Git. Sparse clone is reported unsupported when the client falls back to go-git. Worktree validation checks the origin, commit, and clean status. Results identify which engine ran.

`SparsePaths` contains repository-root-relative directories, such as `[]string{"cmd", "internal/source"}`. The checkout contains those selected directories. Git still retrieves repository metadata, while file contents outside the selected paths are not downloaded up front. An empty list checks out the full repository.

Submodule recursion is disabled by default. `RecurseSubmodules` clones each child at the commit recorded by its parent, passes the root depth to every child, and validates every resolved child URL against the same host, SSRF, credential, timeout, and size policies. Recursion stops after 32 nested levels. Probe checks only the root repository; child repository access is checked during clone.

`ProbeRequest`, `CloneRequest`, and `CredentialTestRequest` accept an `OnEvent` callback. Events report structured progress, process snapshots, and safe diagnostics. Process snapshots omit arguments, environment values, and command output. The caller owns task-log persistence and user-facing formatting. `OperationError.Error()` and `Diagnostic` never include raw process output or credential values. Provider API routing and authentication checks are implemented by GitOps; credential storage and lookup remain the caller's responsibility.

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

Clone size is monitored while files are materialized and the operation context is canceled at the limit. A go-git fallback that targets a commit SHA fetches full reachable history because go-git transports may reject raw SHA refspecs; the size monitor remains active. `ProgressEvent` exposes only engine, phase, and percentage. `ErrorCode` returns a stable safe category; `OperationError.Error()` does not include process output or secrets.

## Compatibility matrix

Checks run on 2026-11-20:

| Service | Transport / credentials | Probe | Depth-one clone | Notes |
| --- | --- | --- | --- | --- |
| GitHub.com | HTTPS, anonymous | Pass | Pass | Public repository |
| GitLab.com | HTTPS, anonymous | Pass | Pass | Public repository |
| Gitea.com | HTTPS, anonymous | Pass | Pass | Public repository |
| Gitee | HTTPS, anonymous | Pass | Pass | Public repository |
| GitHub.com | HTTPS, go-git fallback | Pass | Pass | System Git intentionally unavailable |
| `git.imgo.tv/ott/inott-epg` | HTTPS, Git Credential Manager | Pass | Pass | Private repository; GCM supplied the username/token in process memory |
| `git.imgo.tv/ott/inott-epg` | SSH, configured OpenSSH identity | Pass | Pass | Strict host-key checking enabled |
| `git.imgo.tv` | SSH, empty known-hosts | Rejected | N/A | Strict host-key rejection verified |
| Self-managed Gitea / GitLab private instances | HTTPS | Not run | Not run | No additional authorized private repository was available |

The private HTTPS negative checks also passed for an invalid token, anonymous access, canceled context, and expired context. Error codes are stable and do not include credential material. Authentication failures do not trigger go-git fallback; fallback is limited to a missing system Git executable.

## Local verification

```sh
go test ./...
go vet ./...
```

The default suite includes local security and lifecycle checks that need no external repositories or credentials. These cover provider API routing and redirect rejection, no go-git retry after authentication failure, SSH host-key rejection before user authentication, branch/tag/commit resolution in both engines, and clone destination cleanup after authentication failure, cancellation, size-limit, and invalid-ref errors. GitHub Actions runs `go test ./...` and `go vet ./...` on Windows and Linux using the Go version in `go.mod`.

Optional external compatibility checks use `GITOPS_TEST_GITHUB_URL`, `GITOPS_TEST_GITLAB_URL`, `GITOPS_TEST_GITEA_URL`, `GITOPS_TEST_GITEE_URL`, or `GITOPS_TEST_PRIVATE_URL`. Each accepts optional `_REF`, `_USERNAME`, `_TOKEN`, `_PASSWORD`, and `_SSH_KEY_FILE` variables. Set `GITOPS_TEST_GIT_BINARY` to a missing executable to exercise HTTPS fallback. Cases probe and clone the configured URL and compare the resolved commit; each has a 45-second timeout. They are skipped when no URL is configured.

For a private HTTPS repository already configured in Git Credential Manager, load credentials directly into the test process without printing them:

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

The test reads credentials only from the child process environment. `git credential fill` is captured into a PowerShell variable and the test reports only safe error categories. Do not enable shell tracing, print the populated credential map, or redirect process output to a file containing credentials. Always remove the temporary environment variables when the test finishes.

For SSH, set `GITOPS_TEST_PRIVATE_URL` to an SSH clone URL, such as `git@git.example.org:team/repo.git`. OpenSSH can select an identity from its config, or set `GITOPS_TEST_PRIVATE_SSH_KEY_FILE` to an unencrypted key file. Host-key verification remains strict; unknown keys must be added to `known_hosts` through a trusted verification process.
