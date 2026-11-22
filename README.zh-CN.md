# Amazon QuickWork CPA 原生插件

安装在 CLIProxyAPI 内核中，将 Chat Completions 请求转给**另行部署、明确获授权的** QuickWork HTTP 桥接服务。本插件不直接连接 AWS，不模拟桌面客户端，不登录 AWS 或提取、续期 AWS 凭证；也不执行客户端工具。使用的是桥接服务自己的 Bearer key，不是 AWS JWT、Cookie 或 refresh token。

- 实测 Linux amd64，支持插件的 CPA 内核 v8.0.78、v8.0.80（ABI 1、RPC schema 10）；更旧版本和其他平台未验证。
- 将 ZIP 根目录的 `aws-quickwork.so` 放进 CPA 插件目录，启用插件并配置 `bridge_origins`。配置与 auth 文件范例见 [英文 README](README.md)。凭据类型填 `aws-quickwork`，仅填已批准的 `bridge_origin` 与该服务自己的 `bridge_key`。凭据文件建议使用 `0600`。每个需要独立使用的 AWS 账号应使用独立的桥接实例/地址与密钥。同一内核需将账号路由分开时，为各凭据设置不同 `prefix`，启用 `routing.force-model-prefix: true`，并请求 `prefix/aws-quickwork-smart` 等带前缀的模型；前缀不是用户访问控制。
- 对外提供 `/v1/models` 和 `/v1/chat/completions`，支持 JSON/SSE；`aws-quickwork-{smart,fast,balanced,auto}` 是模式别名。保留请求体中的工具定义、调用历史和结果 ID，工具仍由客户端执行。不会透传原始 `X-Thread-Id`（避免跟随 CPA 调度跨账号复用）；没有原生 Responses API 或 AWS 客户端登录。
- CPA 管理端可按 `auth_index` 调用此插件的配额能力。桥接接口没有账户额度数据，返回未知而不是 0，不消耗推理次数、不探测未公开接口。HTTP 429 只作为限流提示处理，不当作余额或用量；不提供配额重置。

构建：`go test -race ./... && ./build.sh`（Go 1.26.10+、C 编译器、Python 3）。产物默认放至 `dist/`。
