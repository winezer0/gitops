# GitOps

[English](README.en.md)

GitOps 是一个 Go 工具包，用于在应用内受控地探测、克隆和校验 HTTPS、SSH Git 仓库。它提供远程引用解析、限额克隆、工作区校验和凭证检查；凭证存储及主机/DNS 安全策略由调用方提供。

## 功能

GitOps 提供以下操作：

- `ParseURL`：解析并校验仓库地址。
- `Client.Probe`：探测仓库并解析分支、标签和提交引用。
- `Client.Clone`：克隆仓库，可指定引用、浅克隆深度、稀疏路径和递归子模块策略。
- `Client.Checkout`：切换到指定分支、标签或提交。
- `Client.ValidateCheckout`：校验本地工作区的远程地址、提交和干净状态。
- `Client.TestCredential`：检查凭证是否具有指定仓库的读取权限。
- `Client.TestHostCredential`：HTTPS 支持 GitHub、GitLab、Gitea 和自定义 HTTPS 用户 API；SSH 通过严格校验主机密钥的握手检查密钥。

`TestCredential` 检查凭证是否能访问指定仓库。`TestHostCredential` 不依赖仓库路径：HTTPS 凭证通过相应服务商的用户 API 验证；SSH 凭证通过启用严格主机密钥校验的 Go SSH 握手验证。结果分别提供 `CredentialStatus` 和 `ProfileAccess`，区分令牌是否被接受与用户资料是否可读。检查未通过时，方法会同时返回结果和 `*CredentialTestError`；通过 `Diagnostic.Code` 查看具体原因。用户资料 API 验证不代表凭证有权访问某个私有仓库；如需确认仓库权限，请使用 `Probe` 或 `TestCredential`。

调用方应同时检查结果和错误。比如资料 API 返回权限范围不足时，`CredentialStatus` 仍可能是 `accepted`，而 `ProfileAccess` 为 `insufficient_scope`：

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

示例中的 `ctx`、`client` 和 `request` 由调用方提供；需导入 `errors`、`log` 和 `github.com/winezer0/gitops`。

服务商 API 请求不会跟随重定向，避免将令牌转发到重定向目标。自定义 API 地址必须使用 HTTPS，且不能包含用户信息、查询参数或片段。

## 快速开始

以下片段假定写在返回 `error` 的调用方函数中；`ctx`、`repositoryURL`、`credentials` 和 `ssrfPolicy` 由应用提供。示例使用仓库级凭证回调，并假设已导入 `fmt` 和 `github.com/winezer0/gitops`。

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

## 凭证和安全

凭证按以下顺序解析：请求显式凭证、基于规范化传输协议/主机/端口/路径的 `CredentialForRepository`，最后才在仓库回调返回 `ErrCredentialNotConfigured` 时调用旧的主机级 `CredentialFor`。仓库回调应只返回对应仓库范围的凭证。HTTPS 凭证使用用户名和 Token/密码；SSH 凭证可使用私钥字节或私钥文件路径，并可提供解密口令。

Go 引擎默认验证 HTTPS 证书，并要求显式配置 SSH 主机密钥信任来源，例如 `known_hosts` 文件或应用回调。本机 SSH 配置和密钥发现通过 `DiscoverSSHKeys` 显式启用。不要将密钥或令牌放进仓库 URL。

`AllowedHosts` 中只写主机名时，允许该主机使用自定义端口；写入主机名和端口时，只允许该端口。服务端应用应同时配置 `AllowedHosts` 和 `ValidateHost`，以便在访问仓库、服务商 API 和子模块前应用 DNS 与 SSRF 安全策略。

不支持系统凭证助手。保留 `Config.UseCredentialHelpers` 仅用于源码兼容；启用时 `NewClient` 会返回 `ErrCredentialHelpersUnsupported`。集成方应通过请求或凭证回调加载凭证。

## 克隆行为

网络探测、克隆、凭证检查和引用切换使用调用方的 `context.Context`，并遵守请求或客户端超时。本地 `ValidateCheckout` 是同步检查，不接收 context。默认浅克隆深度为 1，默认工作区大小上限为 5 GiB（`MaxCloneBytes`）。克隆目标目录必须不存在，除非设置 `ReplaceExisting`。`ReusePinned` 只会复用与指定固定提交匹配且状态干净的工作区。`DestinationRoot` 可将克隆路径限制在指定目录内。结果会标明实际执行引擎。

`SparsePaths` 指定相对于仓库根目录的目录，例如 `[]string{"cmd", "internal/source"}`。Go 引擎只会在工作区物化选中的目录；当前 go-git 传输仍可能下载其他路径对应的仓库对象。路径列表为空时检出完整仓库。

递归子模块默认关闭。设置 `CloneRequest.RecurseSubmodules` 后，GitOps 会按父仓库记录的 gitlink 提交克隆子模块，不会推进子模块分支。根仓库的浅克隆深度会传递给所有子模块。相对 URL 根据父仓库地址解析；解析出的每个子模块地址都会接受与根仓库相同的主机、SSRF 和凭证校验。总超时和克隆大小限制覆盖整个工作区；递归超过 32 层时会返回资源限制错误。`Probe` 只检查根仓库和指定引用，子模块权限在递归克隆时检查。

固定提交会在请求的浅克隆深度内获取。若该深度内没有目标提交，克隆返回 `ErrCommitUnavailableAtDepth`，不会静默下载完整历史。

## 传输支持

| 协议 | 引擎 | 凭证 | 服务端校验 |
| --- | --- | --- | --- |
| HTTPS | go-git | 用户名和 Token/密码 | 默认验证 TLS 证书 |
| SSH | 进程内 Go SSH | 私钥字节或路径，可选口令 | 严格验证 `known_hosts` 或应用回调 |

运行时不调用 system Git、OpenSSH 或 processkit。认证、主机校验、引用解析、网络、超时和取消错误会直接返回，不会通过其他引擎重试，避免掩盖凭证、主机校验或仓库权限问题。

## 进度、诊断和任务日志

`ProbeRequest`、`CloneRequest` 和 `CredentialTestRequest` 可通过 `OnEvent` 接收结构化进度和安全诊断事件。`CloneRequest.OnProgress` 还会提供引擎、阶段、百分比、已接收/总对象数和接收字节数。任务日志的持久化和面向用户的格式由调用方负责。

错误消息和 `Diagnostic` 不包含原始进程输出或凭证。服务商 API 路由与认证检查由 GitOps 实现；凭证的保存和查找由调用方负责。`ErrorCode` 返回稳定且经过脱敏的错误分类。

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

## 本地验证

```sh
go test ./...
go vet ./...
```

默认测试套件无需访问外部仓库或凭证，覆盖服务商 API 路由与重定向拒绝、请求/仓库/旧式凭证优先级、HTTPS 与 SSH 操作、拒绝未知 SSH 主机密钥、分支/标签/提交解析、稀疏工作区检出，以及失败后的克隆目录清理。GitHub Actions 会使用 `go.mod` 声明的 Go 版本，在 Windows 和 Linux 上运行 `go test ./...` 与 `go vet ./...`。

## 可选的外部兼容性测试

可通过以下环境变量提供测试仓库：`GITOPS_TEST_GITHUB_URL`、`GITOPS_TEST_GITLAB_URL`、`GITOPS_TEST_GITEA_URL`、`GITOPS_TEST_GITEE_URL` 或 `GITOPS_TEST_PRIVATE_URL`。每个变量可配套 `_REF`、`_USERNAME`、`_TOKEN`、`_PASSWORD` 和 `_SSH_KEY_FILE`。私有凭证应通过获批的密钥存储或 CI 密钥注入提供；测试只报告脱敏后的错误类别。默认测试会探测并克隆仓库、比较解析出的提交；每个用例的超时为 45 秒。未配置仓库时会跳过。

私有仓库还可启用额外用例：`GITOPS_TEST_PRIVATE_REF_MATRIX=1` 测试默认引用、分支、标签和提交；`GITOPS_TEST_PRIVATE_NEGATIVE=1` 测试缺失/错误凭证和引用等失败路径；`GITOPS_TEST_PRIVATE_USER_API=1` 测试 GitLab 用户资料 API，并在该用例中跳过仓库探测和克隆。引用矩阵的超时为 2 分钟。

SSH 测试可将 `GITOPS_TEST_PRIVATE_URL` 设为 SSH 克隆地址，例如 `git@git.example.org:team/repo.git`，并提供私钥文件。`GITOPS_TEST_SSH_DISCOVERY=1` 可启用本机 SSH 密钥发现，`GITOPS_TEST_SSH_USER_KNOWN_HOSTS=1` 可启用用户 `known_hosts`。主机密钥校验始终严格；未知主机密钥应通过可信流程加入已配置的信任源。
