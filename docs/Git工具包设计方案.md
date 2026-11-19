# TanSpec 统一 Git 工具包设计方案

## 1. 背景与目标

TanSpec 当前支持 Git 仓库探测、浅克隆、ref 校验、工作区复用校验和主机凭证测试。实现主要位于 `internal/source/git`，HTTP 入口位于 `internal/server`，探测契约位于 `internal/probe`，凭证配置位于 `internal/systemconfig`。这些能力尚未通过稳定统一的 Git 操作 API 对外组织。

本方案设计一个统一的项目内 Git 工具包，供仓库检查、任务执行、凭证测试和其他 Git 来源复用。首期范围包括：

- 解析和校验 HTTPS、SSH URL（含 scp 风格 SSH 地址）。
- 探测远端引用、默认分支和指定 ref。
- 克隆仓库、检出 ref/commit、验证现有工作区。
- 认证匿名及受保护的 GitHub、GitLab、Gitea/Forgejo、Gitee、自建 Git 服务和其他标准 Git 服务器。
- 优先通过 processkit 调用系统 Git；仅在明确可安全回退时使用 go-git。
- 统一取消、超时、进度、错误分类和脱敏行为。

“平台支持”指通过标准 Git HTTPS/SSH 协议访问仓库，不意味着依赖平台 API 或支持每个厂商的全部专有认证扩展。平台用户 API 只用于有明确配置的 Token 认证测试。

## 2. 现状与设计约束

- `internal/source/git` 当前包含 URL 解析、凭证解析、SSRF/代理适配、探测、clone 和 HTTPS/SSH 凭证测试。
- `internal/server` 负责 HTTP 接口和依赖装配；`internal/probe` 定义探测数据契约；`internal/systemconfig` 管理允许主机和凭证配置。
- Git 子进程已通过 `github.com/winezer0/processkit/processrunner` 执行。
- 当前没有 go-git 执行引擎。引入前必须核对所选版本的协议和认证能力，不得假定其行为等价于系统 Git。
- 首期定位为 TanSpec 的 `internal` 包，不承诺外部项目 API 稳定性；不改变现有 HTTP 响应、数据库结构和任务状态契约。
- 继续采用现有 Git 服务安全策略；执行引擎不得自行决定是否跳过这些策略。

## 3. 包边界与依赖方向

首期在现有 `internal/source/git` 职责边界内统一门面，避免平行新增第二套 Git 实现。按职责组织源文件；若后续引擎实现规模或依赖隔离需求足够，再拆出 `internal/source/git/engine` 子包。

逻辑职责：

| 职责 | 内容 |
|---|---|
| Git 门面与 DTO | 操作请求/结果、能力策略、引擎选择和统一错误 |
| URL/ref | URL 解析、主机/传输信息、ref 输入校验和引用解析 |
| processkit 引擎 | 系统 Git 探测、clone、checkout、工作区检查及进度收集 |
| go-git 引擎 | 经兼容性验证的备用探测和 clone 能力 |
| 安全适配 | TanSpec 凭证、代理、SSRF、主机白名单、日志脱敏适配 |
| 凭证诊断 | 平台用户 API 测试和 SSH 主机认证测试；不混同仓库访问探测 |

依赖方向为 `server/worker/composition root → source/git 门面 → 引擎`。门面使用 TanSpec 的安全适配，但引擎只接受已解析、已授权的有限运行配置。`internal/source/git` 不反向依赖 `server`、`systemconfig` 或 Web DTO；配置包将存储配置映射成 Git 包配置。`internal/probe` 与 `internal/source` 的现有接口由适配层映射，不让 Git 引擎依赖它们的业务 DTO。

拟议 API（字段为示意，实施时需按现有调用收敛）：

```go
type Client struct { /* policy and engine selection */ }

type ProbeRequest struct {
    URL string
    Ref string
}

type CloneRequest struct {
    URL, Ref, Commit, Destination string
    Depth                         int
    SparsePaths                   []string
}

type RepositoryInfo struct { /* canonical host, transport, path */ }
type ProbeResult struct { /* refs, default branch, resolved commit */ }
type CloneResult struct { /* checked-out commit and repository metadata */ }
```

实际 API 使用请求结构体承载可选参数，避免长参数列表。回调和输出应可取消、限量，并在日志中屏蔽凭证。不要导出引擎私有参数或 TanSpec HTTP 响应类型。

## 4. 仓库地址与引用模型

支持 HTTPS、SSH URL 和标准 scp 风格 SSH 地址；拒绝不支持的 scheme、URL 内嵌口令/token、查询参数、fragment、空主机/路径和命令选项注入形式。规范化主机名用于白名单及凭证匹配，但保留 Git 实际需要的路径和端口语义。

GitHub、GitLab（含 Self-Managed）、Gitea/Forgejo、Gitee、自建服务和其他私有托管均作为普通 Git 主机处理。不得仅凭主机名推断私有仓库可以匿名访问，也不得为 clone 拼接供应商 API URL。

ref 输入包括：

- 空值：解析远端公布的默认分支/HEAD。
- 分支、标签：验证 ref 格式并从远端公告引用解析；标签需处理 annotated tag 的 peeled commit。
- 完整 commit SHA：验证格式和 fetch 后的实际对象身份；远端不公告该对象时，探测可报告“克隆时验证”，不能谎报已验证。

所有传给系统 Git 的 ref 作为独立参数传递，禁止 shell 拼接；路径、稀疏检出和目录边界需要单独校验。分支、标签和提交的结果语义在两个引擎间必须一致。

## 5. 凭证模型与能力矩阵

凭证与仓库 URL 分离，按规范化的精确主机配置，不从 URL、Git 全局配置或不受控的环境变量隐式读取秘密。配置层负责存储与解密，Git 门面只接收本次操作所需的短生命周期凭证。

首期凭证类型：匿名、HTTPS 用户名+Token/密码、SSH 私钥（可选 passphrase，若现有凭证存储支持安全提供）。自定义 HTTP 认证头、客户端 TLS 证书、SSH certificate、硬件密钥和任意 credential helper 不默认承诺；先评估秘密存储、进程暴露面、两引擎等价能力和测试后再纳入。

| 能力 | 系统 Git/processkit | go-git 备用 | 回退策略 |
|---|---|---|---|
| 匿名 HTTPS | 首期支持 | 验证后支持 | 可回退 |
| HTTPS 用户名+Token/密码 | 使用临时隔离凭证注入，不写 URL | 使用 go-git 原生 auth，验证后支持 | 仅认证表示等价时可回退 |
| SSH 私钥 | 使用受控 SSH 配置、非交互模式和严格 host key 校验 | 仅在 host key、密钥格式、passphrase 语义验证通过后支持 | 未证明等价则禁止自动回退 |
| 自定义 HTTP 头 | 首期不承诺 | 逐项验证 | 不支持时明确报能力错误 |
| 服务商用户 API Token 测试 | 由凭证诊断 HTTP 客户端调用 | 不适用 | 与 Git 引擎无关 |
| 自建服务凭证测试 | 需仓库 URL 才可做 ls-remote | 按相应协议能力 | 不能用通用用户 API 测试替代 |

认证测试区分三种结论：服务商用户 API 接受 Token；SSH 主机接受认证；给定仓库允许读取。前两者不证明仓库读取权限。自定义 Git 服务通常没有统一用户 API，通用仓库权限验证必须给出仓库 URL 并执行只读引用探测。

## 6. 引擎选择和回退语义

系统 Git 是主引擎，processkit 负责跨平台路径解析、进程执行、取消、超时和输出捕获。go-git 是能力受限的备用引擎，不是对任意系统 Git 错误的重试器。

错误分类及行为：

- **可回退**：系统 Git 可执行文件缺失/不可启动，或引擎在产生任何远端副作用前明确声明不支持该请求；备用引擎也必须声明支持完全相同的认证、安全和操作选项。
- **不可回退**：认证拒绝、仓库不存在、ref 不存在/无效、DNS/连接/TLS/代理失败、超时、取消、目标目录错误、资源限制触发及未知远端错误。
- **不得静默降级**：SSH host key 策略、代理、证书校验、凭证注入、浅克隆、稀疏路径或 ref 语义不等价时，返回明确的引擎能力错误。
- 每个操作开始时只选择一个引擎。引擎运行后失败不自动由另一引擎重放，避免重复联网、隐藏原始错误、认证方式变化和耗时翻倍。将来若要增加特定故障回退，必须通过风险评审和专项测试显式列出错误类别。
- 结果记录所用引擎和回退原因，供受控诊断使用；不向普通用户暴露敏感命令或凭证。

go-git 的兼容性矩阵是实现门槛，不是形式文档。实施时锁定依赖版本并验证官方兼容列表及实际行为；没有验证的组合视为不支持，不能自动选择备用引擎。

## 7. 安全、网络与资源限制

安全检查在引擎选择和网络访问之前统一执行：

1. 解析 URL、限制 scheme 和格式，拒绝 URL 内凭证。
2. 执行主机允许列表策略及 DNS/IP SSRF 校验；重定向/协议跳转不可绕过主机策略。
3. 仅解析并注入匹配目标主机的凭证，禁止凭证发送至不同主机。
4. HTTPS 代理沿用 TanSpec 已配置的代理策略；SSH 沿用当前 OpenSSH 行为。备用引擎只有在能等价执行这些策略时才能使用。
5. 保持 TLS 证书校验和 SSH known_hosts 验证开启；不得因回退而关闭校验。
6. 统一操作 deadline、上下文取消、输出上限、目标目录所有权/边界、clone 深度、稀疏路径和克隆后最大工作区大小限制。
7. 对临时凭证文件设置最小权限并保证清理；进程参数、环境、错误、日志和 API 诊断不得包含 Token、私钥或带秘密 URL。
8. 明确 git submodule 默认不递归、不执行仓库 hooks/filter 等非必要代码；如将来支持子模块，必须对每个新 URL 重新实施主机与 SSRF 校验。

## 8. 结果和错误契约

门面返回结构化错误类别及底层 cause，TanSpec 适配层映射到现有 `probe.Diagnostic`、任务错误和 API 响应。用户诊断保持稳定英文 code，由前端本地化展示；底层 stderr 仅保留经长度限制和秘密脱敏后的受控摘要。

探测结果至少包含可达性、默认分支、解析 ref/commit、分支/标签（受数量/字节上限约束）、检查时间及 warning/blocker。clone 结果至少包含最终 commit、实际引擎、工作目录元信息和可复用工作区校验信息。不能把“SSH 主机认证通过”映射为“仓库可读”。

## 9. 迁移步骤

1. **现状盘点**：列全 `Prober`、`Fetcher`、credential probe 和 `vulnscore` Git fetch 调用方、配置字段、诊断码与测试；确认包边界和 API，不复制既有校验逻辑。
2. **统一门面**：为现有 processkit 系统 Git 实现定义操作 DTO 和引擎接口；保留当前 URL、凭证、SSRF、代理、重用工作区、进度和诊断行为。
3. **适配现有入口**：`internal/server`、worker 与其他 Git 调用方通过门面；保留 HTTP、任务和数据库契约。配置装配仍留在 composition root。
4. **能力清单和 go-git spike**：锁定 go-git 版本，分别验证 HTTPS/SSH、自定义主机、凭证、known_hosts、代理、取消、浅克隆、分支/标签/commit、稀疏路径和跨平台行为；产出能力矩阵与禁止回退组合。
5. **受控回退**：仅开放已通过等价性测试的能力组合；默认关闭未知组合回退。记录引擎选择但不记录秘密。
6. **清理旧入口**：迁移完成后再次搜索旧执行路径，避免保留第二套 URL、凭证和错误分类规则。

各阶段均可单独审查和回滚；在 go-git 达到安全/语义验收前，生产行为继续使用现有系统 Git。

## 10. 测试和验收

### 单元测试

- URL scheme、scp URL、端口、路径、内嵌凭证、query/fragment、非法空白与选项注入。
- 主机规范化、白名单、逐主机凭证路由和秘密脱敏。
- ref 类型、annotated tag、未公告 commit、ref 参数注入和 commit 一致性。
- 引擎错误分类、选择、禁止回退分支、取消/超时不回退。
- 两引擎共享的大小、输出、稀疏路径和目录边界规则。

### 集成测试

- 本地临时 Git bare 仓库覆盖默认分支、分支、轻量/annotated tag、指定 commit、浅克隆、稀疏检出和现有工作区验证。
- 使用可控 HTTP/SSH 测试端或平台测试仓库验证匿名及凭证认证；不能将公共互联网可用性作为普通单测前提。
- 验证 GitHub、GitLab、Gitea/Forgejo、Gitee 和自定义主机的 HTTPS/SSH 组合。平台名本身不应导致 Git 协议行为分叉。
- 验证主机切换、重定向、私有/保留 IP、代理、错误 host key、无效证书及 clone 取消均不会泄漏凭证或绕过策略。
- 逐项比较 processkit 与 go-git 的 refs、commit、目录结果和错误分类；仅把等价项加入自动回退白名单。

### 结构验收

- 调用依赖单向、无循环，无第二套 URL/凭证/SSRF 规则。
- 新包公开 API 少且稳定，手写文件职责单一，配套测试达到项目要求。
- 现有探测、clone、密钥测试 API 行为和任务状态不回归；针对性测试和全量测试通过。
- 文档明确记录 go-git 不支持项、回退白名单、所需 Git 安装/版本要求和操作限制。

## 11. 非目标与待实施验证

- 本方案不实现通用 Git 服务端 API 聚合，也不为所有托管平台维护用户 API 适配。
- 首期不支持任意凭证插件、任意 Git helper、子模块递归或推送/写入远端。
- 不承诺所有 Git CLI 功能都能由 go-git 回退实现。
- go-git 的 SSH host key、代理、自定义凭证及平台兼容性需通过所选依赖版本的实测后，才能在能力矩阵中标记支持。
- 本文是待实施设计，不表示上述统一门面或 go-git 回退已经存在。
