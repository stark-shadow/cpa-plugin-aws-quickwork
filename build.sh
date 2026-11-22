#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "$0")"
output=${1:-dist}
os=$(go env GOOS)
arch=$(go env GOARCH)
python=${PYTHON:-python3}
mkdir -p -- "$output"
version=$("$python" - "$os/$arch" <<'PY'
import json, sys
m = json.load(open('manifest.json'))
if sys.argv[1] not in m['platforms']:
    raise SystemExit(f'unsupported build target: {sys.argv[1]}')
print(m['version'])
PY
)
case "$os" in
    windows) lib="aws-quickwork.dll" ;;
    linux) lib="aws-quickwork.so" ;;
esac
archive="aws-quickwork_${version}_${os}_${arch}.zip"
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o "$output/$lib" .
"$python" - "$output" "$lib" "$archive" <<'PY'
from pathlib import Path
import hashlib, sys, zipfile
out, lib, archive = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
with zipfile.ZipFile(out / archive, 'w', zipfile.ZIP_DEFLATED) as z:
    z.write(out / lib, lib)
    for item in ['manifest.json', 'aws-quickwork.omc.json', 'README.md', 'README.zh-CN.md', 'LICENSE']:
        z.write(item, item)
with (out / 'checksums.txt').open('w') as f:
    f.write(f'{hashlib.sha256((out / archive).read_bytes()).hexdigest()}  {archive}\n')
print(out / archive)
PY
