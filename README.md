# GitOps

GitOps 是一个结构化的 Go Git 工具包，用于探测和克隆 HTTPS、SSH Git 仓库。

## 功能

GitOps 提供以下操作：

- `ParseURL`：解析并校验仓库地址。
- `Client.Probe`：探测仓库并解析分支、标签和提交引用。
- `Client.Clone`：克隆仓库，可指定引用、浅克隆深度、稀疏路径和递归子模块策略。
- `Client.Checkout`：切换到指定分支、标签或提交。
- `Client.ValidateCheckout`：校验本地工作区的远程地址、提交和干净状态。
- `Client.TestCredential`：检查凭证是否具有指定仓库的读取权限。
- `Client.TestHostCredential`：独立检查服务商账号凭证或 SSH 凭证。

`TestCredential` 检查凭证是否能访问指定仓库。`TestHostCredential` 不依赖仓库路径：HTTPS 凭证通过相应服务商的用户 API 验证；SSH 凭证通过启用严格主机密钥校验的 OpenSSH 握手验证。服务商 API 验证只说明账号凭证有效，不代表凭证有权访问某个私有仓库；如需确认仓库权限，请使用 `Probe` 或 `TestCredential`。

服务商 API 请求不会跟随重定向，避免将令牌转发到重定向目标。自定义 API 地址必须使用 HTTPS，且不能包含用户信息、查询参数或片段。

## 快速开始

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

## 凭证和安全

`CredentialFor` 应只返回与传入规范化主机名对应的凭证。HTTPS 凭证使用用户名和 Token/密码；SSH 凭证使用未加密的私钥字节。加密 SSH 私钥目前不支持，会返回错误。

system Git 会强制验证 HTTPS 证书，并对 SSH 启用严格的 `known_hosts` 校验，同时保留 OpenSSH 主机配置。不要将密钥或令牌放进仓库 URL。

`AllowedHosts` 中只写主机名时，允许该主机使用自定义端口；写入主机名和端口时，只允许该端口。服务端应用应同时配置 `AllowedHosts` 和 `ValidateHost`，以便在访问仓库、服务商 API 和子模块前应用 DNS 与 SSRF 安全策略。

默认禁用 system Git 凭证助手，包括 Git Credential Manager。设置 `Config.UseCredentialHelpers` 后，当调用方没有提供显式 HTTPS 凭证时，system Git 才会使用已配置的凭证助手。显式提供 Token 或密码时，该操作不会使用凭证助手。go-git 回退引擎不能使用系统凭证助手。

## 克隆行为

每次操作受 `Config.Timeout` 和调用方的 `context.Context` 共同限制。默认浅克隆深度为 1，默认工作区大小上限为 5 GiB（`MaxCloneBytes`）。克隆目标目录必须不存在，除非设置 `ReplaceExisting`。`ReusePinned` 只会复用与指定固定提交匹配且状态干净的工作区。`DestinationRoot` 可将克隆路径限制在指定目录内。结果会标明实际执行引擎。

`SparsePaths` 指定相对于仓库根目录的目录，例如 `[]string{"cmd", "internal/source"}`。使用 system Git 时，GitOps 通过 `--filter=blob:none` 和 cone 模式稀疏检出，只会预先下载选中路径的文件内容；仓库提交和树等元数据仍会获取。路径列表为空时检出完整仓库。go-git 回退不支持稀疏克隆。

递归子模块默认关闭。设置 `CloneRequest.RecurseSubmodules` 后，GitOps 会按父仓库记录的 gitlink 提交克隆子模块，不会推进子模块分支。根仓库的浅克隆深度会传递给所有子模块。相对 URL 根据父仓库地址解析；解析出的每个子模块地址都会接受与根仓库相同的主机、SSRF 和凭证校验。总超时和克隆大小限制覆盖整个工作区；递归超过 32 层时会返回资源限制错误。`Probe` 只检查根仓库和指定引用，子模块权限在递归克隆时检查。

如果 go-git 回退要检出固定提交，可能需要下载该提交可达的完整历史，因为 go-git transport 可能拒绝原始 SHA refspec；克隆大小监控仍然生效。

## 执行引擎和回退

GitOps 首先通过 processkit 调用 system Git。只有找不到 Git 可执行文件时，才会对 HTTPS 仓库回退到 go-git。SSH 仓库不会回退到 go-git；稀疏克隆请求在 go-git 回退模式下也不受支持。认证、引用解析、网络、TLS、超时或取消错误都不会触发第二个引擎重试。

GitOps 不会在认证失败时自动换引擎重试。这样可以避免凭证、主机校验或仓库权限问题被其他引擎掩盖。

## 进度、诊断和任务日志

`ProbeRequest`、`CloneRequest` 和 `CredentialTestRequest` 可通过 `OnEvent` 接收结构化进度、进程快照和安全诊断事件。进程快照不包含参数、环境变量或命令输出。任务日志的持久化和面向用户的格式由调用方负责。

`OperationError.Error()` 和 `Diagnostic` 不包含原始进程输出或凭证。服务商 API 路由与认证检查由 GitOps 实现；凭证的保存和查找由调用方负责。`ProgressEvent` 仅提供引擎、阶段和百分比；`ErrorCode` 返回稳定且经过脱敏的错误类别。

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

## 兼容性矩阵

以下结果于 2026-11-20 验证：

| 服务 | 传输方式 / 凭证 | 探测 | 浅克隆 | 说明 |
| --- | --- | --- | --- | --- |
| GitHub.com | HTTPS，匿名 | 通过 | 通过 | 公开仓库 |
| GitLab.com | HTTPS，匿名 | 通过 | 通过 | 公开仓库 |
| Gitea.com | HTTPS，匿名 | 通过 | 通过 | 公开仓库 |
| Gitee | HTTPS，匿名 | 通过 | 通过 | 公开仓库 |
| GitHub.com | HTTPS，go-git 回退 | 通过 | 通过 | 验证时禁用了 system Git |
| `git.imgo.tv/ott/inott-epg` | HTTPS，Git Credential Manager | 通过 | 通过 | 私有仓库；GCM 在进程内提供用户名和令牌 |
| `git.imgo.tv/ott/inott-epg` | SSH，OpenSSH 配置的密钥 | 通过 | 通过 | 启用严格主机密钥校验 |
| `git.imgo.tv` | SSH，known_hosts 为空 | 拒绝 | 不适用 | 验证了严格主机密钥拒绝行为 |
| 自建 Gitea / GitLab 私有实例 | HTTPS | 未测试 | 未测试 | 当时没有其他获授权的私有仓库 |

私有 HTTPS 仓库的负向检查也已通过，涵盖错误 Token、匿名访问、主动取消和超时。错误类别保持稳定且不包含凭证内容。认证失败不会触发 go-git 回退；回退条件仅为找不到 system Git。

## 本地验证

```sh
go test ./...
go vet ./...
```

默认测试套件无需访问外部仓库或凭证，覆盖服务商 API 路由与重定向拒绝、认证失败时不回退、用户认证前拒绝未知 SSH 主机密钥、两种引擎下的分支/标签/提交解析，以及认证失败、取消、大小超限和无效引用后的克隆目录清理。GitHub Actions 会使用 `go.mod` 声明的 Go 版本，在 Windows 和 Linux 上运行 `go test ./...` 与 `go vet ./...`。

## 可选的外部兼容性测试

可通过以下环境变量提供测试仓库：`GITOPS_TEST_GITHUB_URL`、`GITOPS_TEST_GITLAB_URL`、`GITOPS_TEST_GITEA_URL`、`GITOPS_TEST_GITEE_URL` 或 `GITOPS_TEST_PRIVATE_URL`。每个变量可配套 `_REF`、`_USERNAME`、`_TOKEN`、`_PASSWORD` 和 `_SSH_KEY_FILE`。设置 `GITOPS_TEST_GIT_BINARY` 为不存在的可执行文件，可以测试 HTTPS 回退。测试会探测并克隆仓库、比较解析出的提交；每个用例的超时为 45 秒。未配置仓库时会跳过。

对于已在 Git Credential Manager 中配置凭证的 HTTPS 私有仓库，可在不打印凭证的情况下将凭证注入测试进程：

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

测试从进程环境读取凭证，只报告经过脱敏的错误类别。不要启用 shell 跟踪、打印凭证映射或将可能包含凭证的输出重定向到文件；测试完成后应移除临时环境变量。

SSH 测试可将 `GITOPS_TEST_PRIVATE_URL` 设为 SSH 克隆地址，例如 `git@git.example.org:team/repo.git`。OpenSSH 可从其配置中选择密钥，也可以通过 `GITOPS_TEST_PRIVATE_SSH_KEY_FILE` 指定未加密私钥文件。主机密钥校验始终严格；未知主机密钥应通过可信流程加入 `known_hosts`。
