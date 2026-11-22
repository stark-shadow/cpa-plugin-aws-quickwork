# Loopback acceptance

Go 1.26.10+, Python 3, curl, and a real released Linux amd64 CPA core with plugin support are required. Verify the core archive against its release checksums first. Do not use a core binary from a running deployment.

```sh
./build.sh
(cd dist && sha256sum -c checksums.txt)
QUICKWORK_INSTALL_ARCHIVE="$PWD/dist/aws-quickwork_0.1.0_linux_amd64.zip" go test -race -count=2 -v ./...
python3 acceptance/core.py /absolute/path/to/cli-proxy-api \
  dist/aws-quickwork_0.1.0_linux_amd64.zip /absolute/path/to/evidence
```

`core.py` starts isolated core instances with temporary config, credentials, and home. Inference is against two loopback fixtures only. The built-in OpenAI compatibility route is tested against the same HTTP fixture before the native plugin route. The test checks the observed path, body fields (including tool results), outbound authorization scope, and stream delivery before the fixture emits its ending. Native acceptance also checks auth-file import, quota advertisement and credential binding, an unknown quota with no upstream I/O, unsupported reset, wrong/absent auth indexes, and per-model cooldown after an inference 429. `matrix.json` and per-profile core logs are written to the evidence directory. `TestArchiveWithCPAInstaller` exercises a published CPA SDK installer against the exact ZIP over loopback and verifies the installed library bytes and bad-checksum rejection.

There is no live AWS account, remote bridge, OMC/GUI run, or non-Linux/amd64 verification in this matrix.
