# Changelog

## [0.0.2] - 2026-11-20

### Breaking changes / 破坏性变更

- Replaced `CredentialTestResult.Authenticated` with `CredentialStatus` and `ProfileAccess`. / 将 `CredentialTestResult.Authenticated` 替换为 `CredentialStatus` 和 `ProfileAccess`。
- `Client.TestHostCredential` now returns its diagnostic result together with `*CredentialTestError` when a check fails. Callers should use `errors.As` and inspect `Diagnostic.Code`. / `Client.TestHostCredential` 检查未通过时会同时返回诊断结果和 `*CredentialTestError`；调用方应使用 `errors.As` 并检查 `Diagnostic.Code`。
- A profile API scope denial reports an accepted credential with `ProfileAccessInsufficientScope`, while still returning a typed error. This does not establish repository access. / 资料 API 权限范围不足时，结果表示凭证已接受、`ProfileAccessInsufficientScope`，同时仍返回类型化错误；该结果不代表具备仓库访问权限。
