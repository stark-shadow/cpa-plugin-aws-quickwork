# Amazon QuickWork CPA 原生插件

由 CPA 管理 QuickWork OAuth 登录和凭据，插件直接连接 QuickWork（已同步上游 `aws-quickwork-2api` v0.5.0 协议与特性）。**不需要 `quickproxy`、HTTP 桥接、桌面客户端或独立账号数据库。** 仅限获授权的账号和服务；本项目非 AWS 官方产品。

将 Windows amd64 的 `aws-quickwork.dll`、Linux amd64 的 `aws-quickwork.so` 或 macOS arm64/amd64 的 `aws-quickwork.dylib` 放入 CPA 插件目录，启用插件后重启核心。在 CPA 登录面板选择 **QuickWork Personal** 或 **QuickWork Enterprise**，并保持面板开启直到保存凭据。本地浏览器自动回调；远程部署可先粘贴发现链接、再通过 CPA 原生接口提交授权码回调，接口见[英文说明](README.md)。旧桥接版升级需要删除旧动态库、`bridge_origins` 和旧凭据，重新登录。

登录时参照 Trae 的双卡片方式区分个人账号与企业 SSO；这两个 QuickWork 入口不代表国内版和国际版。浏览器回调根据语言显示中英文进度、继续按钮及返回 CPA 的提示。Token 刷新时若上游未重发 `id_token` 会自动保留原值，按 `expires_in` 与 JWT `exp` 的较早时间写入 `expires_at` 和 `refresh_interval_seconds` 以接入 CPA 后台自动刷新，并在请求临近过期或遇到上游 401/403 时按需自动刷新并持久化。

调用 `/v1/models` 和 `/v1/chat/completions`，使用 `aws-quickwork-{smart,fast,balanced,auto}`，主要特性包括：

- **指令透传与强化工具桥接**：将 `system`/`developer` 指令与 `<caller_tool_interface>` 工具定义置于用户轮次头部（上游忽略 `system_prompt` 字段），支持 JSON/SSE、多格式工具调用解析、命名空间前缀匹配、原生 `tool_progress` 兜底及零宽空白回复检测。
- **64 KB 帧限制与超长附件上传**：单帧超限时优先走上游附件上传 + DLP 扫描（仅接受 `ALLOW`）+ 会话文件投递，失败或拦截时自动降级为 UTF-8 安全的中间截断。
- **会话复用**：默认开启 `session_reuse`，按账号与会话键（支持 `X-Conversation-Id` / `X-Thread-Id`）复用上游线程并增量发送新增消息；上游线程过期时在输出前自动重建并回退全量上下文。
- **官方额度查询**：通过上游 `qbs-event`（`profile/usage`）读取月度与超额分钟数配额及订阅等级，不伪造未上报的上限；当凭据或套餐不支持该接口时明确提示而非显示为零。
- **出站代理路由**：统一通过凭据级 `proxy_url` 或插件配置 `proxy_url`（支持 `http://`、`https://`、`socks5://`、`socks5h://`、`direct`）路由 WebSocket、附件上传、额度同步与 Token 刷新。若在特定环境中遇到 TLS 握手超时（上游 Issue #10），可为 CPA 进程临时设置 `GODEBUG=tlsmlkem=0`。

可选配置项见[英文说明](README.md)（`request_timeout`、`session_reuse`、`session_ttl`、`keep_sessions`、`thinking_effort`、`chat_agent_id`、`timezone`、`locale`、`proxy_url`）。不支持 Responses API 或配额重置。

运行 `go test -race ./...`（无 CGO 环境可用 `CGO_ENABLED=0 go test ./...`）、`./build.sh` 构建。GitHub Actions 的 **Build plugin** 工作流原生构建并测试以上四个平台；成功运行后可下载 `aws-quickwork-release-assets`，内含四个发布 ZIP 和合并后的 `checksums.txt`，不会自动发布。
