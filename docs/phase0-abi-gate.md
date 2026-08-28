# Phase 0：CLIProxyAPI v7.2.144 与 Core 增强 ABI 闸门

## 固定基线

```text
CLIProxyAPI tag:    v7.2.144
CLIProxyAPI commit: d36b776c790a4d58027fd4fb434800fb5334bceb
Private Core merge: 39bcde17
Plugin baseline:    cpa-antigravity-quota-guard v0.1.1
验证日期:           2026-08-28
```

本闸门不访问真实生产账号，不写生产 auth 文件。

## stock v7.2.141 已验证 ABI

插件单元和集成夹具已覆盖：

- `usage.handle` 的 `Provider`、`Model`、`AuthID`、`AuthIndex`、`Failure.StatusCode/Body`、`ResponseHeaders`。
- `scheduler.pick` 通过 `AuthID` 选择候选；插件通过 `host.auth.list` 维护 `AuthID ↔ AuthIndex` 映射。
- 非 Antigravity 请求返回 `Handled=false`。
- mixed-provider 路由会选择未被冷却的其他 Provider。
- plugin-only 全池 cooldown 的 Scheduler failure envelope 带 `http_status=429`。
- after-auth interceptor 可生成结构化 JSON、`Retry-After`，并在 Scheduler 已创建 half-open lease 时放行该次探测。
- CPA 只调用最高优先级 Scheduler 插件。
- CPA Home 模式会绕过 plugin Scheduler。
- CPA 原生全池 cooldown 产生 `modelCooldownError`，包含 HTTP 429 和 `Retry-After`。

stock Core 不提供以下长期 `enforce` 必需能力：

- Scheduler 与 Usage 之间可关联的请求标识。
- Scheduler rejection 的响应头和结构化 body。
- required-Scheduler 持续所有权与 fail-closed 回退策略。

## 2026-08-24 实际执行记录

在固定 commit 上执行：

```bash
go test ./sdk/cliproxy/auth \
  -run 'TestManagerPluginSchedulerSkippedWhenHomeEnabled|TestManagerPluginSchedulerErrorStopsPick|TestSelectorPick_AllCooldownReturnsModelCooldownError' \
  -count=1 -v

go test ./sdk/cliproxy/auth \
  -run 'TestManager_SchedulerTracksMarkResultCooldownAndRecovery|TestManager_MarkResult_TransientErrorCooldownDefault|TestRequestScopedErrors_ActionContinueAndCooldown' \
  -count=1 -v

go test ./internal/pluginhost \
  -run 'TestDecodeEnvelopeResultPreservesPluginHTTPStatus|TestHostPickAuthReturnsSchedulerError|TestHostPickAuthUsesHighestPrioritySchedulerOnly' \
  -count=1 -v

go test ./sdk/api/handlers \
  -run 'TestHandlerRequestInterceptorTerminatesAfterAuth|TestHandlerExecutorErrorSkipsResponseInterceptor' \
  -count=1 -v
```

四组测试均通过，实际确认：

1. Home 模式下 Scheduler 插件调用次数为 0。
2. Scheduler error 会直接停止 auth pick。
3. plugin RPC error 只构造带 message 和 `StatusCode()` 的 `rpcPluginError`；没有响应头和结构化 body 承载面。
4. CPA 原生 cooldown error 能提供 `Retry-After`。
5. after-auth interceptor 只有在 auth selection 成功后才有机会终止请求。
6. CPA `MarkResult` 的原生 cooldown 会被后续 Scheduler pick 立即看到；同一请求 retry 的即时跳过依赖这一原生机制，而不是异步 Usage 插件。

## Core 增强合同

本地 Core 候选保持通用，不在 CLIProxyAPI 中写 Antigravity 专属规则：

```text
required_scheduler_v1
scheduler_request_id_v1
scheduler_direct_response_v1
auth_inventory_ready_v1
```

对应行为：

1. 每个插件 instance 可配置：

   ```yaml
   required-scheduler-for:
     - antigravity
   ```

2. 配置中的 required-Scheduler 要求在插件加载失败、卸载、fuse、单插件 disabled 或全局 `plugins.enabled=false` 后仍保留；只有显式删除 marker 才解除保护。
3. 路由命中 required provider 时，只有“恰好一个 active Scheduler，且 ID 等于 required plugin ID”才允许调度。
4. Home、Scheduler 缺失/fuse/抢占、decline、错误或无效响应均返回本地 `required_scheduler_unavailable` / HTTP 503，不回退 built-in selector。
5. Scheduler request 与 Usage record 增加 `RequestID`，供插件精确关联 half-open lease。
6. Scheduler direct response 只接受 HTTP 400–599；响应头只允许 `Content-Type: application/json` 和数字 `Retry-After`；body 必须是合法 JSON 且不超过 64 KiB。
7. `host.auth.list` 增加 `inventory_ready`；Core 只在初始 auth store load 成功并完成配置 auth 注册后发布 `true`。
8. required Scheduler 支持 `DelegateBuiltin=configured`，用于 `observe` 和未 enforcement 的模型组，并保留宿主当前 configured selector 策略与 cursor。

插件在 `enforce` 注册、重配和动态配置切换时必须同时看到上述四个 host feature，并确认原始 plugin config 包含 `required-scheduler-for: [antigravity]`。stock Core 没有 feature 声明时，插件拒绝 `enforce`；`observe` 正常注册。

冷启动期间 `inventory_ready=false` 时，插件以 armed-not-ready 注册并保留已有状态文件，不把 bootstrap 空 roster 写回磁盘。required route 在 baseline 建立前本地 503；auth inventory ready 后的通用 plugin reconfigure 完成 roster reconcile，满足 fresh baseline 后才允许出站。

## 2026-08-24 最终源码真实进程复验

在隔离目录使用假 token、单条假 Antigravity auth 和 localhost 代理运行真实 Core 二进制与 `.dylib`：

- 插件在 auth manager 尚未 ready 时成功注册，不再出现 `plugin.register failed: empty_roster`。
- auth inventory ready 后状态 API 显示 `configured=true`、`enforcement_ready=true`，持久化的 Gemini `quota_zero/open` 与 Claude/GPT `closed` 均保留。
- `gemini-3-flash` 非流式请求返回本地 HTTP 429、数字 `Retry-After: 3543` 和 `model_cooldown` JSON；localhost 代理连接数保持 `7 -> 7`，确认该请求零 Provider 出站。
- 同账号 `claude-sonnet-4-6` 请求继续进入 Provider 路径；测试代理连接数 `7 -> 8`，并记录到新的 OAuth CONNECT，确认模型组隔离。
- 全局 `plugins.enabled=false` 但保留 `required-scheduler-for: [antigravity]` 时，请求返回本地 HTTP 503 `required_scheduler_unavailable`；测试代理连接数保持 `15 -> 15`。
- `observe` 且保留 required-Scheduler marker 时，同一 Gemini 请求通过 `DelegateBuiltin=configured` 进入宿主当前 selector；最终源码产物复验的测试代理连接数 `15 -> 16`。
- armed-not-ready 的 startup probe、Management probe、状态不擦除和本地 503 使用确定性集成测试覆盖；`TestEnforceColdStartArmsUntilAuthInventoryReadyWithoutErasingState` 验证两个 probe 均零出站，并在 inventory ready 后 reconfigure 恢复 enforcement。
- `TestSchedulerDoesNotMixObserveConfigWithEnforceEngineDuringReconfigure` 确定性阻塞 generation gate，验证并发 `observe -> enforce` 切换不会产生“旧 config + 新 engine”的 `DelegateBuiltin=configured`。
- 本次复验未访问真实生产账号，也未写生产 auth。

最终本地产物：

```text
plugin dylib SHA-256: 75cd2fccf3058ebd2b431c160573e4133a441c0c864bd41aeb4eaa56658ef4fb
Core binary SHA-256:  60427b1ddd84a0c1c0434223dd405bc208ac8df69829526eb69eef58826e825f
```

以上 SHA 仅标识本地隔离验收产物；尚未创建 Release，也不应作为未提交源码的长期分发标识。

## 闸门结论

| 场景 | stock v7.2.141 | Core 增强候选 | 判定 |
|---|---|---|---|
| Usage 字段完整性 | 满足 | 满足并增加 `RequestID` | PASS |
| Scheduler 指定 AuthID | 满足 | 满足并增加 `RequestID` | PASS |
| mixed-provider 排除 Antigravity | 插件可处理，但 Scheduler 失效后可能回退 | required route 失效时整体 503 | PASS WITH CORE |
| plugin-only 全池 cooldown | 只能稳定传 HTTP status | HTTP 429 + 数字 `Retry-After` + JSON body | PASS WITH CORE |
| 同一请求 429 后立即跨账号 retry | 依赖 CPA 原生 cooldown | 仍依赖原生 cooldown；Usage `RequestID` 解决 half-open 归属，不替代同步 MarkResult | PASS WITH NATIVE COOLDOWN |
| Home 模式 | 绕过 plugin Scheduler | required route 本地 503 | PASS WITH CORE FAIL-CLOSED |
| 插件加载失败、卸载或 fuse | 回退 built-in | required route 本地 503 | PASS WITH CORE FAIL-CLOSED |
| 第二个/更高优先级 Scheduler | 可能抢占 | required route 本地 503 | PASS WITH CORE FAIL-CLOSED |
| Scheduler decline/invalid response | 回退 built-in | required route 本地 503 | PASS WITH CORE FAIL-CLOSED |

## Core Fork 判定

长期目标已经实际触发 Core Fork 条件，**Core 增强是 `enforce` 的必要依赖，不再是可选项**。原因不仅是结构化 429，还包括 Home 绕过、Scheduler fuse/卸载、第二 Scheduler 抢占和无效响应时 stock Core 的 fail-open 回退。

Core 采用 host-owned required-Scheduler，而不是只把 Scheduler/Home/fuse 状态暴露给插件后进行一次性 preflight。这样即使插件随后失效，宿主仍能持续 fail-closed。

## 本地 Core Fork 候选

已在本地创建未提交分支：

```text
路径:   /Users/zyaire/Documents/API Router/cpa-cliproxyapi-core
分支:   feat/plugin-scheduler-rejection-response
基线:   dc3c3b1ec3ed04bb0917e76451eaf98c6842674d
remote: upstream=https://github.com/router-for-me/CLIProxyAPI.git
```

候选改动保持通用：

- per-plugin `required-scheduler-for` host 配置。
- auth manager required route fail-closed，覆盖 Home、单 provider、mixed provider、fast path 和 legacy path。
- lifecycle `host_features` 协商。
- `host.auth.list.inventory_ready` 与冷启动 auth inventory 发布时序。
- Scheduler 与 Usage `RequestID`。
- `pluginabi.Error` 的受限 `response_headers` / `response_body` direct response。
- direct response 的状态码、header、JSON 类型与 64 KiB 上限校验。

尚未创建 GitHub Core Fork、commit、push 或 PR。

## 当前上线边界

当前支持边界：

- stock CLIProxyAPI `v7.2.141`：只允许 `observe`；代码会拒绝 `enforce`。
- 本地 Core 增强候选：自动化测试和隔离环境 `enforce` 已通过；生产灰度仍需提交、构建、Release 和上线审批。
- 不得宣称异步 `usage.handle` 替代了 CPA 原生 cooldown；同请求 retry 仍必须由 Core 原生 MarkResult/cooldown 跳过刚失败账号。
- GitHub Core Fork、commit、push、PR、Release 和生产部署均尚未执行。

## 2026-08-28 v7.2.144 兼容性复核

- 上游基线为 tag `v7.2.144`、commit `d36b776c790a4d58027fd4fb434800fb5334bceb`；本轮私有 Core 合并提交为 `39bcde17`。
- v7.2.144 将 WebSocket response observer 作为 ABI schema version 4 的新增能力；本插件不使用该 observer，继续声明 schema version 3，不改插件接口或版本 `0.1.1`。
- 插件 `main` 固定源码提交为 `13e6c84877e939c9ab467a9b75c1db742796efaa`，相对 `quota-guard-v0.1.1`（`40031b1ebf86cf2fb317ea7b577a92cf85166b8b`）仅包含测试变更，运行时代码无差异。
- 本地已通过 `go test -race ./...`、`go vet ./...` 与 pinned `golangci-lint v2.12.2`；Linux `CGO_ENABLED=1` 动态库构建、注册和真实请求验收仍以 216 受控环境证据为准。

本节记录目标版本的兼容性门禁；上文 v7.2.141 测试与行为描述保留为历史基线，不替代 v7.2.144 的生产验收。
