# Amazon QuickWork CPA 原生插件

由 CPA 管理 QuickWork OAuth 登录和凭据，插件直接连接 QuickWork。**不需要 `quickproxy`、HTTP 桥接、桌面客户端或独立账号数据库。** 仅限获授权的账号和服务；本项目非 AWS 官方产品。

将 Windows amd64 的 `aws-quickwork.dll` 或 Linux amd64 的 `aws-quickwork.so` 放入 CPA 插件目录，启用插件后重启核心。在 CPA 登录面板选择 **QuickWork Personal** 或 **QuickWork Enterprise**，并保持面板开启直到保存凭据。本地浏览器自动回调；远程部署可先粘贴发现链接、再通过 CPA 原生接口提交授权码回调，接口见[英文说明](README.md)。旧桥接版升级需要删除旧动态库、`bridge_origins` 和旧凭据，重新登录。

登录时参照 Trae 的双卡片方式区分个人账号与企业 SSO；这两个 QuickWork 入口不代表国内版和国际版。浏览器回调根据语言显示中英文进度、继续按钮及返回 CPA 的提示。

调用 `/v1/models` 和 `/v1/chat/completions`，使用 `aws-quickwork-{smart,fast,balanced,auto}`。支持文本 JSON/SSE、客户端工具调用及上游报告的用量。每轮发送完整消息和工具历史；不复用 `X-Thread-Id`，不支持 Responses API 或配额重置。目前未确认该登录凭据可读取的 Amazon Quick 余额接口；额度面板会明确提示，不会将余额显示为零。

运行 `go test -race ./...`、`./build.sh` 构建。0.2.0 是原生预览版，仍需用实际获授权账号验证登录、刷新和聊天后才能用于正式发布。
