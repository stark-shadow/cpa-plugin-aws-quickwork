# Native loopback acceptance

Build the archive with Go 1.26.10+, a CGO C compiler and Python 3. Use an isolated, matching-platform CPA binary with native plugins enabled; the smoke script starts a temporary instance without touching another running CPA or accessing AWS.

```sh
./build.sh
QUICKWORK_INSTALL_ARCHIVE="$PWD/dist/aws-quickwork_0.3.3_linux_amd64.zip" go test -count=1 ./...
python3 acceptance/core.py /path/to/cli-proxy-api dist/aws-quickwork_0.3.3_linux_amd64.zip
```

The installer test verifies ZIP layout, the library format and architecture, installed library bytes, and rejection of an invalid checksum. Use the ZIP for your platform to repeat the CI check. The core smoke checks the descriptor, CPA's two native login routes, state polling, and both the plugin-specific discovery validation and CPA's final pasted-callback route. Go tests simulate login/exchange/refresh and the direct WebSocket flow, including framed tool calls, usage, truncation and session deletion. Use a separately authorized AWS account for live sign-in, token refresh and chat before releasing the plugin.
