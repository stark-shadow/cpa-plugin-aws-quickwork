# Amazon QuickWork native CPA plugin

QuickWork OAuth and direct OpenAI-compatible chat execution in CLIProxyAPI (synced with upstream `aws-quickwork-2api` v0.5.0 protocol behavior). No `quickproxy`, HTTP bridge, desktop client, or separate credential database is required. Use only with accounts and endpoints you are authorized to access. This is not an AWS product.

## Install and sign in

Place `aws-quickwork.dll` (Windows amd64), `aws-quickwork.so` (Linux amd64), or `aws-quickwork.dylib` (macOS arm64/amd64) in the CPA plugin directory and restart CPA. The optional OMC descriptor does not change the installation or login process.

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    aws-quickwork:
      enabled: true
      request_timeout: 5m
      session_reuse: true
      session_ttl: 30m
      keep_sessions: false
      thinking_effort: ""        # "", off, low, medium, high
      chat_agent_id: ""          # optional custom QuickWork agent ID
      timezone: UTC
      locale: en-US
      proxy_url: ""              # optional http://, https://, socks5://, socks5h://, or direct
```

Open **QuickWork Personal** or **QuickWork Enterprise** from CPA OAuth and keep the panel polling while completing the browser flow. Start requests can set `region` (default `us-east-1`). Both cards create native `aws-quickwork` credentials. Keep CPA's auth directory private; CPA persists the OAuth credential there. Upgrading from 0.1.0 requires removing the old DLL/library, `bridge_origins`, and old bridge credentials, then signing in again. There is no bridge fallback.

Token refresh preserves the existing `id_token` when the upstream refresh response omits it, tracks the earlier expiry between `expires_in` and JWT `exp`, exposes `expires_at` and `refresh_interval_seconds` metadata for CPA's background auto-refresh loop, proactively refreshes and persists expired/near-expiry credentials on demand (and retries once on upstream 401/403), and routes per-account refresh, quota sync, WebSocket, and attachment calls consistently through the credential's `proxy_url` (or plugin/host proxy fallback).

For a remote CPA host, start the same native OAuth flow using the management key and `GET /v8/management/oauth/auth-url?provider=aws-quickwork-personal` (or `aws-quickwork-enterprise`). After discovering the account in the browser, submit the loopback discovery URL with its `nonce` and the CPA flow's `state` as `{"state":"…","redirect_url":"…"}` to the authenticated plugin endpoint `POST /v0/management/plugins/aws-quickwork/oauth/discovery`. Open the returned `authorize_url`. Submit the final loopback URL containing `code` and `state` as `{"redirect_url":"…"}` to CPA's `POST /v8/management/oauth/callback`. Poll `GET /v8/management/oauth/status?state=…` with the management key to save the credential. Do not paste callback links into chat or logs. A stock panel may not offer a UI for the first (discovery) request.

## Chat, tools, attachments, and quota

- **Models:** Call `/v1/models` and `/v1/chat/completions` with `aws-quickwork-smart`, `aws-quickwork-fast`, `aws-quickwork-balanced`, or `aws-quickwork-auto`.
- **Prompt relay & tool bridge:** Caller `system`/`developer` messages and `<caller_tool_interface>` definitions are relayed at the head of the user turn (QuickWork ignores `user_message.payload.system_prompt` and treats `custom_prompt` as low-trust preferences). Supports JSON and SSE, client-side `tools`/`functions`/`tool_choice`/`parallel_tool_calls`, alternate tool markers/fenced/bare JSON shapes, namespace prefix resolution, native `tool_progress` fallback, and `stream_options.include_usage`. Tool-enabled streams are buffered until turn completion so markers never leak into prose; whitespace-only or zero-width-only replies fail cleanly instead of returning an empty stop.
- **64 KB frame limit & attachments:** Oversized user prompts (≥ 8 KB pushing `user_message` over 63 KB) first attempt out-of-band attachment upload with DLP scan polling (`ALLOW` only) and `files/upload/<thread>` delivery while keeping caller instructions in the message text. Unavailable or blocked uploads fall back to UTF-8-safe middle trimming.
- **Session reuse:** When `session_reuse` is enabled (default), each `(authID, conversation)` reuses its upstream thread and sends only incremental message deltas (`X-Conversation-Id` / `X-Thread-Id` headers are honored for conversation isolation). Expired upstream threads automatically reset and retry with the full transcript before any output is emitted.
- **Official quota sync:** The quota provider queries QuickWork's official `profile/usage` endpoint (`qbs-event`) and normalizes monthly and monthly-overage minute allowances and subscription tier into CPA's quota UI. Uncapped overage limits are never fabricated as zero; if a credential or plan does not expose official usage, the quota panel explains this clearly instead of displaying zero. Quota reset and Responses API remain unsupported.
- **TLS handshake compatibility:** If Go hits a TLS handshake timeout against QuickWork endpoints on affected hosts (upstream Issue #10), run CPA with process-scoped `GODEBUG=tlsmlkem=0` while keeping default TLS verification enabled.

## Build and validation

Use Go 1.26.10+, a CGO-compatible C compiler and Python 3. Run `go test -race ./...` (or `CGO_ENABLED=0 go test ./...` for pure-Go unit tests) and `./build.sh` (`PYTHON=python` if appropriate). The archive and checksums go to `dist/`. The GitHub Actions **Build plugin** workflow builds and tests all four platforms natively. Download `aws-quickwork-release-assets` from a successful run for the four release ZIPs and their combined `checksums.txt`; the workflow does not publish a release. See `acceptance/` for local host verification.
