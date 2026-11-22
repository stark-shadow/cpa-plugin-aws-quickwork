# Amazon QuickWork native CPA plugin

QuickWork OAuth and direct OpenAI-compatible chat execution in CLIProxyAPI. No `quickproxy`, HTTP bridge, desktop client, or separate credential database is required. Use only with accounts and endpoints you are authorized to access. This is not an AWS product.

## Install and sign in

Place `aws-quickwork.dll` (Windows amd64) or `aws-quickwork.so` (Linux amd64) in the CPA plugin directory and restart CPA. The optional OMC descriptor does not change the installation or login process.

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    aws-quickwork:
      enabled: true
      request_timeout: 2m
```

Open **QuickWork Personal** or **QuickWork Enterprise** from CPA OAuth and keep the panel polling while completing the browser flow. Start requests can set `region` (default `us-east-1`). Both cards create native `aws-quickwork` credentials. Keep CPA's auth directory private; CPA persists the OAuth credential there. Upgrading from 0.1.0 requires removing the old DLL/library, `bridge_origins`, and old bridge credentials, then signing in again. There is no bridge fallback.

For a remote CPA host, start the same native OAuth flow using the management key and `GET /v8/management/oauth/auth-url?provider=aws-quickwork-personal` (or `aws-quickwork-enterprise`). After discovering the account in the browser, submit the loopback discovery URL with its `nonce` and the CPA flow's `state` as `{"state":"…","redirect_url":"…"}` to the authenticated plugin endpoint `POST /v0/management/plugins/aws-quickwork/oauth/discovery`. Open the returned `authorize_url`. Submit the final loopback URL containing `code` and `state` as `{"redirect_url":"…"}` to CPA's `POST /v8/management/oauth/callback`. Poll `GET /v8/management/oauth/status?state=…` with the management key to save the credential. Do not paste callback links into chat or logs. A stock panel may not offer a UI for the first (discovery) request.

## Chat and scope

Call `/v1/models` and `/v1/chat/completions` with `aws-quickwork-smart`, `aws-quickwork-fast`, `aws-quickwork-balanced`, or `aws-quickwork-auto`. The models are mode aliases. Chat is text-only and supports JSON, SSE, client-side tools, usage when reported upstream, and `stream_options.include_usage`. Send complete conversation and tool history; raw `X-Thread-Id`, Responses API, and quota reset are unsupported. The OAuth credential has no verified balance endpoint; opening the quota panel explains this instead of showing an empty result or zero. A verified throttle and per-request token usage are not balance readings.

## Build and validation

Use Go 1.26.10+, a CGO-compatible C compiler and Python 3. Run `go test -race ./...` and `./build.sh` (`PYTHON=python` if appropriate). The archive and checksums go to `dist/`. See `acceptance/` for local host verification. The 0.2.0 native implementation is a preview until authorized live AWS sign-in, refresh, and chat have been validated.
