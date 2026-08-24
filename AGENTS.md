# AGENTS.md

`cpa-antigravity-quota-guard` 是基于 CLIProxyAPI 官方插件 ABI 的 Google Antigravity 配额熔断插件。

## 核心原则

1. **只读 auth**：插件不得修改 auth JSON 的 `priority`、`disabled`、token 或其他字段。
2. **模型组隔离**：状态主键为 `(auth_index, model_group)`，模型组固定为 `gemini`、`claude_gpt`。
3. **官方 ABI**：生产只注册 `scheduler`、`usage_plugin`、`request_interceptor`、`management_api`。
4. **fail-closed**：未知模型、未知账号和 identity 冲突默认拒绝，不通过自动改写修复。
5. **证据优先**：probe 失败不改变可用状态；旧 Usage 不覆盖更新鲜 evidence。
6. **单 half-open lease**：每个账号/模型组同一时间最多一个探测请求。
7. **安全持久化**：状态文件 `0600`、原子写、fsync、回读验证，拒绝路径穿越和 symlink 跳出。
8. **默认 observe**：enforce 必须满足 `docs/phase0-abi-gate.md` 与 `docs/release-checklist.md`。

## 严格交互与 Git 约束

- 用户明确要求实施本计划后，可以修改代码并运行本地测试。
- 未经用户明确授权，不得 commit、push、创建 PR、发布 Release、部署或修改生产配置。
- 功能完成后先汇报变更、测试结果和目标环境验收步骤，等待用户明确说“可以提交”或“commit”。

## 架构所有权

- `internal/guard`：breaker 状态机、调度决策、指标和状态持久化；不得网络 IO 或 Host 写回。
- `internal/config`：默认值、解析、路径安全和动态配置校验；`auto_apply=true` 必须拒绝。
- `internal/runtime/official_*.go`：CPA 官方 ABI 适配、roster、后台 probe/persist、Management API。
- `internal/runtime/production_runner.go`：quota probe；不得执行 auth transition。
- `internal/host`：Host ABI 适配；生产 probe 只消费 `host.auth.get` 返回的 JSON，不跟随 Path。
- `internal/apply`、`internal/priority`、旧 `internal/management`：上游历史兼容代码，不得重新接入生产 Handle。

## 质量与发布

每次准备提交或发布前必须阅读 `docs/release-checklist.md`，至少运行：

```bash
golangci-lint run --timeout=5m
go build ./...
go vet ./...
go test -v ./...
go test -race ./...
git diff --check
```

CPA 兼容性必须使用 `docs/phase0-abi-gate.md` 固定的 tag/commit 复验。
