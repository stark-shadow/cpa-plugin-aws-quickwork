# Amazon QuickWork native CPA provider

This plugin routes OpenAI Chat Completions from CLIProxyAPI to a **separately operated, explicitly authorized** QuickWork bridge. It is not an AWS product, a direct AWS API client, or a desktop sign-in tool. The operator must already have an approved bridge and a bridge bearer key. Do not supply an AWS JWT, password, session cookie or refresh token here. The plugin neither provisions nor refreshes AWS access, starts a bridge, nor executes client tools.

- Verified released hosts: CLIProxyAPI **v8.0.78** and **v8.0.80** with plugin support enabled (native ABI 1, RPC schema 10), on Linux amd64. No minimum older release is asserted.
- For manual installation, extract the library at ZIP root to the configured plugin directory and restart the core. The SDK installer layout is also supported. There is no published store entry.
- Use a distinct bridge origin and key for each upstream account that should stay separate. Each bridge must enforce its bearer key; a process serving one AWS session does not become two accounts because it accepts two keys. To guarantee account routing on a shared core, assign each auth file a distinct `prefix`, set `routing.force-model-prefix: true`, and call `prefix/aws-quickwork-smart` (or another mode). Prefixes select routing; they are not a downstream access-control boundary between users. Only operators should be able to write gateway/plugin config and credential files.

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    aws-quickwork:
      enabled: true
      bridge_origins: ["http://127.0.0.1:8080"]
      request_timeout: "2m"
```

Put `quickwork-operator.json` under the core auth directory (0600 where supported), or import that JSON through `POST /v8/management/credentials?name=quickwork-operator.json` with the ordinary management key:

```json
{
  "type": "aws-quickwork",
  "label": "QuickWork account",
  "bridge_origin": "http://127.0.0.1:8080",
  "bridge_key": "REPLACE_WITH_BRIDGE_BEARER_KEY"
}
```

Only exact HTTPS origins or literal loopback HTTP origins are allowed. Redirects, outbound proxy inheritance and arbitrary downstream headers are disabled. There is no plugin/browser login; rotate the bridge key by replacing the auth file. With no enabled origin, the plugin will not send a request.

Endpoints exposed by CPA: `GET /v1/models` and `POST /v1/chat/completions` (JSON and SSE). Model IDs are `aws-quickwork-smart`, `aws-quickwork-fast`, `aws-quickwork-balanced`, and `aws-quickwork-auto`; these are mode aliases, not a remote model inventory. The plugin forwards OpenAI message history, system/developer messages, tool declarations and tool-result IDs without executing tools. Raw `X-Thread-Id` reuse is not exposed across CPA credentials; send the tool conversation history in the body. Native Responses API/response IDs, images or other modalities are not promised, and unsupported sampling knobs remain subject to the bridge. Streaming usage is returned if the bridge emits it. Request deadlines bound streaming even when the native plugin cannot immediately observe a disconnected client.

## Quota management

CPA advertises `aws-quickwork` as the quota provider for each recognized credential. `POST /v8/management/plugins/aws-quickwork/quota` with that credential's `auth_index` returns **unknown** (no balance, percentage, plan or reset time): the bridge does not expose authoritative account quota. Fetching quota neither makes a model request nor probes undocumented endpoints. A bridge HTTP 429 is an inference-side transient per-model throttle with optional `Retry-After`, not a balance reading. Quota resets are not supported. Some bridges instead return 502 or an SSE error for upstream refusals; these are not guessed to be 429. Core routing policy is separate; the plugin does not try another account or retry.

## Build

Go 1.26.10 or later, a native C compiler and Python 3 are needed. Run `go test -race ./...` and `./build.sh`. The resulting ZIP and `checksums.txt` are in `dist/` by default. See `acceptance/` for loopback-only verification.
