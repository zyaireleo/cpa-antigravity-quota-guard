# CPA Antigravity Quota Guard 发布检查清单

## 1. 版本与身份一致性

发布前必须保证：

| 位置 | 要求 |
|---|---|
| `registry.json` | id=`cpa-antigravity-quota-guard`，version 为三段式版本 |
| `internal/runtime/runtime.go` | `buildMetadata()` 名称、作者、仓库、版本一致 |
| `internal/management/feature_shell_assets.go` | 若保留 legacy shell，版本 badge 与 Release 一致 |
| `.github/workflows/release.yml` | `PLUGIN_NAME=cpa-antigravity-quota-guard` |
| `.github/release-notes/vX.Y.Z.md` | 文件名、标题、中文、English 一致 |
| `README.md` / `README.en.md` | 二进制名、配置键、Management 路由一致 |

历史 `v1.x` Release Notes 属于上游历史，可保留但不得作为当前 fork 的版本来源。

## 2. 架构安全闸门

- 只注册 `scheduler`、`usage_plugin`、`request_interceptor`、`management_api`。
- `filter.*` 调用返回 `invalid_request`。
- `auto_apply=true` 在 YAML、JSON、动态配置和遗留缓存恢复路径中均被拒绝或忽略。
- `ManualApply`、`AutoApply`、`ResetAllPriorities` 返回 `ErrLegacyMutationDisabled`。
- 生产 probe 只消费 `host.auth.get` 返回的 `AuthDocument.JSON`，不读取 `Path`。
- 官方 ABI、Management、后台 probe 和 shutdown 全程 `SaveAuth=0`。
- 人工 disabled 账号不会被插件重新启用。
- `Unavailable` 账号刷新 roster 时不得丢失 breaker。
- 同一 `(auth_index, model_group)` 只能有一个 active half-open lease。
- delayed Usage 不能覆盖更新鲜 quota evidence；旧成功不能关闭新 half-open。

## 3. 状态文件与敏感信息

- `state_path` 位于 `data/cpa-antigravity-quota-guard/`。
- 拒绝绝对路径、Windows 绝对路径、`..`、auth/credentials 目录、目标 symlink 和父级 symlink。
- 文件权限 `0600`。
- 使用临时文件、file `fsync`、atomic rename、directory `fsync`、read-back 验证。
- 状态、日志、Management API 不包含 access token、refresh token、完整 auth JSON、请求体或完整错误体。
- Management Key 不接受 URL query，不进入 `localStorage`/`sessionStorage`。

## 4. CPA 兼容闸门

对 `docs/phase0-abi-gate.md` 固定的 CPA commit 执行 Phase 0 测试，并记录日期和 commit。

`enforce` 前必须确认：

1. Core 基于 CLIProxyAPI `v7.2.141` / `dc3c3b1ec3ed04bb0917e76451eaf98c6842674d` 的增强改动构建。
2. lifecycle 协商同时包含：
   - `required_scheduler_v1`
   - `scheduler_request_id_v1`
   - `scheduler_direct_response_v1`
   - `auth_inventory_ready_v1`
3. 插件 host 配置包含 `required-scheduler-for: [antigravity]`。
4. 唯一 active Scheduler 是本插件；第二个 Scheduler、插件 fuse/卸载/加载失败时 Antigravity 必须本地 503，不能回退 built-in。
5. CPA Home disabled；若误开启，Antigravity 必须本地 503。
6. 受管账号 priority 一致。
7. 两个模型组 fresh evidence 完整。
8. plugin-only 全池 cooldown 返回本地 429、数字 `Retry-After` 和结构化 JSON，Provider 抓包为零出站。
9. Scheduler/Usage `RequestID` 能正确关联 half-open；旧 Usage 不得关闭新 lease。
10. 冷启动 `inventory_ready=false` 时插件仍 registered，但 `enforcement_ready=false`；受保护 Antigravity-only 请求本地 503，已有状态文件不得被空 roster 覆盖。
11. inventory ready 后通用 reconfigure 能恢复 `enforcement_ready=true`；`observe` / 未 enforcement 模型组通过 `DelegateBuiltin=configured` 保留当前 routing strategy。

stock Core 只能用于 `observe` 验证。插件必须在 host feature 缺失时拒绝 `enforce`。

## 5. 供应链

- GitHub Actions 使用完整 commit SHA。
- Release 不覆盖既有远端 asset。
- 每个压缩包生成 SHA-256。
- Release Note 记录插件 commit、上游插件基线、CPA 兼容版本。
- 不自动信任第三方预编译 `.so/.dylib/.dll`。

## 6. 完整质量门禁

```bash
golangci-lint run --timeout=5m
go build ./...
go vet ./...
go test -v ./...
go test -race ./...
git diff --check
```

并检查：

```bash
rg -n 'host\.auth\.save|SaveAuth\(|ReplaceAuth\(|ManualApply|AutoApply|ResetAllPriorities|triggerCooldown' \
  main.go internal/runtime internal/management cmd/devserver
```

命中项必须是明确的兼容 stub、Host ABI 适配或已隔离 legacy 代码，不能存在官方运行路径可达的 auth 写回。

## 7. 人工验收

- 本地 Dev Server 状态页可打开。
- Dev Server 默认仅监听 `127.0.0.1`；非 loopback 未带 `-unsafe-listen` 时拒绝启动。
- Dev Management API 未提供独立 key 时返回 401；key 不通过 query、URL 或浏览器持久化存储传递。
- Management status/config/probe/half-open 路由返回预期状态。
- 状态响应能看到 quota evidence、breaker、`recover_at`、最后一次选择和最后失败原因。
- observe 模式实际请求行为不变。
- 隔离环境验证 Gemini/Claude-GPT 组级隔离、全池零出站、half-open 单请求和重启恢复。
- required Scheduler 缺失、fuse、第二 Scheduler、Home 和 invalid response 场景均 fail-closed。
- 只有用户明确说“可以提交”或“commit”后才能 Git commit；push、PR、Release、部署仍需分别获得明确授权。
