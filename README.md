# CPA Antigravity Quota Guard

`cpa-antigravity-quota-guard` 是面向 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI) 的 Google Antigravity 配额熔断插件。它基于 CPA 官方插件 ABI，按 **账号 × 模型组** 维护配额状态，在账号额度耗尽或持续收到 429 时停止选择该账号，而不是修改 auth JSON 的 `priority` 或 `disabled`。

当前开发基线：

- 上游插件基线：`ygq-future/antigravity-priority` `v1.2.9` / `373f44b430c5eb770fb63657da9a7983c5cdabff`
- CPA ABI 基线：CLIProxyAPI `v7.2.144` / `d36b776c790a4d58027fd4fb434800fb5334bceb`
- 当前版本：`0.1.3`

> 默认运行在 `observe`。stock CLIProxyAPI `v7.2.144` 只支持 `observe`；`enforce` 必须配套本项目的 CLIProxyAPI Core 增强（当前兼容合并提交 `39bcde17`），并通过 host feature 协商与 `required-scheduler-for` 闸门。

## 核心行为

- 模型组独立：`gemini` 与 `claude_gpt` 互不影响。
- 状态机：`uninitialized → closed → open → half_open`。
- quota 证据为 0 且 reset 可信：立即打开 breaker，恢复时间使用 reset。
- 明确额度耗尽 429：立即打开 breaker。
- generic 429：60 秒内连续 2 次后冷却 15 分钟，再次失败最多提升到 30 分钟。
- reset 到达后只允许一个 half-open 请求；成功关闭 transient breaker，失败重新打开。
- quota probe 失败仅标记证据过期并计数，不改变调度状态。
- 未知账号和未知模型默认 fail-closed。
- mixed provider 路由会排除冷却中的 Antigravity，同时保留其他 Provider 候选。
- 人工 `disabled=true` 始终由 CPA 原生候选过滤负责，插件不会写回或重新启用账号。

## 官方 ABI 能力

插件只注册：

```text
scheduler
usage_plugin
request_interceptor
management_api
```

旧 `filter.*`、优先级写回、自动调度写回和 reset 接口均不注册；兼容方法会返回 `runtime: legacy auth mutation is disabled`。

## 构建

```bash
go build -buildmode=c-shared -trimpath -ldflags="-s -w" \
  -o cpa-antigravity-quota-guard.so .
sha256sum cpa-antigravity-quota-guard.so
```

CPA 根据动态库文件名识别插件，因此产物基础名必须是 `cpa-antigravity-quota-guard`。

本仓库不信任第三方预编译文件，也不建议通过外部 `main/registry.json` 自动升级。生产版本应使用本仓库自建 Release 和 SHA-256 校验。

## 配置

```yaml
plugins:
  configs:
    cpa-antigravity-quota-guard:
      enabled: true
      priority: -100
      required-scheduler-for:
        - antigravity
      mode: observe
      managed_auth: all_antigravity
      enforced_groups:
        - gemini
        - claude_gpt
      require_uniform_priority: true
      probe_interval: 15m
      evidence_max_age: 30m
      generic_429:
        threshold: 2
        window: 60s
        initial_cooldown: 15m
        max_cooldown: 30m
      half_open_lease: 30s
      unknown_auth_policy: fail_closed
      unknown_model_policy: fail_closed
      state_cache_path: data/cpa-antigravity-quota-guard/quota-cache.json
      state_path: data/cpa-antigravity-quota-guard/state.json
```

`auto_apply: true` 会被明确拒绝。插件不会修改 auth 文件的 `priority`、`disabled`、token 或其他字段。

### `enforce` 上线闸门

启用前必须全部满足：

1. 使用 CLIProxyAPI `v7.2.144` 基线上的 Core 增强版本（私有兼容合并提交 `39bcde17`），并由宿主声明全部 feature：
   - `required_scheduler_v1`
   - `scheduler_request_id_v1`
   - `scheduler_direct_response_v1`
   - `auth_inventory_ready_v1`
2. 插件配置包含 `required-scheduler-for: [antigravity]`。宿主会将 Antigravity 路由精确交给本插件；本插件缺失、fuse、卸载、单插件或全局插件开关关闭、decline 或返回无效结果时，Antigravity 路由本地返回 503，不回退内建调度器。其他 Provider 的 Scheduler 可以同时启用。对于未配置 required Scheduler 的路由，CPA 仍只调用全局最高插件 priority 的 Scheduler，因此应让本插件 priority 低于 `codex-token-usage` 等其他 Provider Scheduler，例如本插件 `-100`、`codex-token-usage` 为 `0`。只有显式删除 marker 才解除保护。
3. CPA Home 模式关闭。增强 Core 会在 Home 仍开启时对 Antigravity fail-closed，但这不是正常运行模式。
4. 所有受管 Antigravity 账号 priority 一致。
5. `gemini`、`claude_gpt` 均有 fresh baseline quota evidence。
6. 状态文件可安全读写，且没有未知或身份冲突账号。
7. `docs/phase0-abi-gate.md` 中的 Core 与插件集成门禁全部通过。

缺失 host feature、`required-scheduler-for` 或安全状态路径会直接拒绝 `enforce`。冷启动时，插件可在 CPA 初始 auth inventory 尚未完成或 baseline evidence 暂不可用时以 **armed-not-ready** 状态完成注册：Management API 保持可用，所有仅含 Antigravity 的受保护请求本地返回 503，且不会覆盖已有 breaker 状态。宿主发布 `inventory_ready=true` 并完成 roster reconcile 后，fresh baseline 满足即自动进入 `enforcement_ready=true`。通过 Management API 从 `observe` 切换到 `enforce` 仍要求全部闸门当场通过。

已经建立 baseline 后，单次 probe 失败或 evidence 随时间变旧不会把健康账号整体停掉；原有 breaker 状态保持不变。新增或换号账号独立保持 `uninitialized` 并被排除，直到其首次成功 quota probe。

## Management API

```text
GET  /v0/management/cpa-antigravity-quota-guard/status
GET  /v0/management/cpa-antigravity-quota-guard/config
PUT  /v0/management/cpa-antigravity-quota-guard/config
POST /v0/management/cpa-antigravity-quota-guard/actions/probe
POST /v0/management/cpa-antigravity-quota-guard/actions/half-open
GET  /v0/resource/plugins/cpa-antigravity-quota-guard/status
```

Management Key 不接受 URL query，不写入 `localStorage` 或 `sessionStorage`；最小状态页只在页面内存中保存密钥。

## 状态与安全

- breaker 状态：`data/cpa-antigravity-quota-guard/state.json`
- quota evidence/cache：`data/cpa-antigravity-quota-guard/quota-cache.json`
- 状态文件权限：`0600`
- 写入流程：临时文件、`fsync`、原子 rename、目录 `fsync`、回读验证
- 相对状态路径拒绝绝对路径、`..`、目标 symlink 和父级 symlink
- auth 内容只从 `host.auth.get` 返回的 `JSON` 读取；插件不会跟随 `AuthDocument.Path`
- 状态和日志不保存 access token、refresh token、完整 auth JSON、请求体或完整错误体

## CPA Core 兼容边界

历史说明：CLIProxyAPI `v7.2.141` 中，`usage.handle` 是异步派发，因此插件不能单独保证在同一入站请求的下一次 credential retry 之前收到刚发生的 429；该场景仍依赖 CPA 原生 cooldown。

长期 `enforce` 已确认需要 Core 增强：

- Scheduler/Usage 通过 `RequestID` 关联 half-open lease，避免旧请求关闭新 lease。
- Scheduler rejection 可安全返回 HTTP 429、数字 `Retry-After` 和最大 64 KiB 的合法 JSON body。
- `required-scheduler-for` 将 Antigravity 精确路由到本插件，并在其缺失、fuse、inactive、decline 或返回无效结果时本地 503，禁止静默回退；无关 Provider Scheduler 可同时保持 active。
- `auth_inventory_ready_v1` 让插件区分“CPA 仍在冷启动加载 auth”与“已加载但 roster 为空”，避免早期空列表擦除持久化 cooldown。
- required Scheduler 可用 `DelegateBuiltin=configured` 在 `observe` 或未启用的模型组中委托宿主当前 configured selector，保留原 routing strategy 和 cursor；宿主不会重新选中已由插件排除的账号。
- Home 开启时同样 fail-closed。

stock Core 不提供这些保证，因此插件会拒绝在 stock Core 上进入 `enforce`；`observe` 仍兼容 stock `v7.2.144`。

## 本地 Dev Server

```bash
go run ./cmd/devserver
```

打开：

```text
http://localhost:8080/v0/resource/plugins/cpa-antigravity-quota-guard/status
```

Dev Server 只模拟 auth inventory 和 quota endpoint；probe、状态页和 guard Management API 使用生产 Runtime。所有 legacy apply/auto-apply 操作均被禁用。

默认仅监听 `127.0.0.1:8080`，启动时生成并打印独立 Dev Management Key。状态页可直接打开，调用 `/v0/management/` 接口时必须在页面输入该 key 或发送 Bearer header；非 loopback 监听必须显式传入 `-unsafe-listen`。

## 验证

```bash
golangci-lint run --timeout=5m
go build ./...
go vet ./...
go test -v ./...
go test -race ./...
```

详细发布检查见 `docs/release-checklist.md`，Phase 0 ABI 闸门见 `docs/phase0-abi-gate.md`。

## 上游与许可证

本项目 fork 自 `ygq-future/antigravity-priority`，保留原项目 MIT 许可证与历史归属；当前 fork 由 `zyaireleo` 维护，架构目标已从 priority 写回转为只读配额熔断。
