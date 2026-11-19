# SSH 代理支持（暂缓）

## 状态

暂未实现，按当前决定延期。GitOps 的 `ProxyFor` 目前用于 HTTP/HTTPS Git 操作和服务商凭证 API；SSH transport 不调用该代理回调。用户机器上的 OpenSSH 配置可能自行设置 `ProxyCommand`，但它不属于 GitOps 提供的统一能力，也不能保证 system Git 与 go-git 行为一致。

## 何时实施

出现以下实际需求时再排期：仓库必须使用 SSH，且运行环境禁止直连 SSH；同时该仓库不能或不允许改用 HTTPS。

## 设计约束

- 新增独立的 SSH 代理配置，避免将 HTTPS Git 代理配置直接复用于 SSH。协议范围实施前明确，优先考虑 HTTP CONNECT、HTTPS CONNECT；是否包含 SOCKS5 另行确定。
- 确保 system Git 和 go-git 两条执行路径行为一致。若只能由其中一条路径处理代理 SSH，必须明确路由策略，不能在另一条路径上静默退化为直连。
- 在建立隧道前继续执行仓库主机 allowlist 和 SSRF 检查。隧道只负责网络转发，不得跳过严格的 SSH host-key 校验。
- 代理凭证与 Git 仓库凭证分开管理，不得写入进程参数、诊断消息或进度事件；代理连接和 SSH 握手均须支持超时及取消。
- 诊断应区分代理连接/认证失败、目标不可达、SSH 认证失败和 host-key 拒绝，并保持现有安全脱敏规则。

## 实现前的依赖评估

- [`elastic/proxy-connect-dialer-go`](https://github.com/elastic/proxy-connect-dialer-go)：提供 HTTP/HTTPS CONNECT 的 Go 拨号器，Apache-2.0，适合作为候选；当前版本为 `v0.1.1`，需先审查实现、测试和维护状态，暂不视为已选定依赖。
- [`golang.org/x/net/proxy`](https://pkg.go.dev/golang.org/x/net/proxy)：可提供 SOCKS5 拨号能力；HTTP CONNECT 需要额外拨号器。
- [`Psiphon upstreamproxy`](https://github.com/Psiphon-Labs/psiphon-tunnel-core/tree/master/psiphon/upstreamproxy)：支持多种代理认证，但模块较重且为 GPL-3.0，除非重新评估许可和依赖体积，否则不作为首选。

当前 go-git v5 SSH transport 接受 `transport.ProxyOptions`，并通过 `x/net/proxy` 建立连接；system Git 则需要 SSH `ProxyCommand` 或其他隧道接入方式。[go-git v5.19.3 SSH 实现](https://github.com/go-git/go-git/blob/v5.19.3/plumbing/transport/ssh/common.go)。因此，仅给 go-git 增加 HTTP CONNECT 拨号器不足以完成 GitOps 的统一支持。

## 验收条件

- 无代理时，SSH 探测、凭证测试和克隆行为不变；HTTPS Git 代理行为不变。
- Windows 和 WSL 下分别覆盖代理成功、代理认证失败、目标拒绝、超时和取消。
- 覆盖探测、浅克隆、递归子模块，以及 host-key 接受和拒绝；验证浅克隆规则仍传递到递归子模块。
- 验证两种 Git 执行路径的代理路由一致，并确认代理地址中的用户名、密码不会出现在日志、事件、诊断或错误文本中。

