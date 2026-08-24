# CPA Antigravity Quota Guard 本地 Dev Server

`cmd/devserver` 提供不访问真实 Antigravity 网络的本地 CPA 仿真环境。它使用生产 Runtime 和 Guard Management API，只把 CPA auth inventory 与 quota endpoint 替换成本地实现。

## 启动

```bash
go run ./cmd/devserver
```

状态页：

```text
http://localhost:8080/v0/resource/plugins/cpa-antigravity-quota-guard/status
```

启动日志会打印一次随机生成的 `Dev Management Key`。状态页本身可以打开，但刷新 Management 状态时需要把该 key 输入页面；也可用 `Authorization: Bearer <key>` 调用 `/v0/management/` 接口。

默认生成 10 个模拟 auth 文件：

```text
data/devserver/auth-files/
data/devserver/quota-state.json
data/devserver/refresh-cache.json
```

## 参数

```text
-addr        HTTP 监听地址，默认 127.0.0.1:8080
-unsafe-listen 允许非 loopback 监听；未显式提供时会拒绝启动
-management-key 独立 Dev Management Key；为空时随机生成并打印
-accounts    最少生成的账号数量，默认 10
-auth-dir    模拟 CPA auth JSON 目录
-quota-state 模拟配额状态文件
-state-cache Runtime quota evidence 缓存文件
-seed        随机种子；0 表示使用时间种子
```

## 安全边界

- 默认只监听 loopback。`0.0.0.0`、局域网 IP 或其他非 loopback 地址必须同时传入 `-unsafe-listen`。
- `/v0/management/` 接口必须携带独立 Dev Management Key；key 不接受 URL query。
- `/v0/resource/` 仅提供静态状态页，不包含 key。
- `host.http.do` 只在本地返回 quota summary 兼容 JSON，不发起真实网络请求。
- 一次 probe 同时返回 Gemini 和 Claude/GPT 两个模型组的窗口。
- Runtime 只读取 `host.auth.get` 返回的 JSON。
- `ManualApply`、`AutoApply`、priority reset 和旧 `filter.*` 均被禁用。
- 测试会校验 probe 与 Management 操作前后 auth JSON 字节不变。
