"""Stage verified per-platform artifacts and assemble release assets."""

import hashlib
import json
import shutil
import sys
from pathlib import Path


CHECKSUMS = "checksums.txt"


def verify(source, filename):
    archive = source / filename
    with archive.open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    checksum = f"{digest}  {filename}\n"
    if (source / CHECKSUMS).read_text(encoding="ascii") != checksum:
        raise ValueError(f"checksum or filename mismatch for {archive}")
    return checksum


def require_entries(directory, expected):
    actual = {entry.name for entry in directory.iterdir()}
    if actual != set(expected):
        raise ValueError(f"unexpected entries in {directory}: got {sorted(actual)}, expected {sorted(expected)}")


def main(mode, source, destination, *targets):
    source, destination = Path(source), Path(destination)
    manifest = json.loads(Path("manifest.json").read_text(encoding="utf-8"))
    prefix = manifest["id"]
    version = manifest["version"]
    platforms = manifest["platforms"]
    archives = {}
    for target in platforms:
        os, arch = target.split("/")
        archives[f"{prefix}-build-{os}-{arch}"] = f"{prefix}_{version}_{os}_{arch}.zip"
    if len(archives) != len(platforms) or len(set(archives.values())) != len(platforms):
        raise ValueError("duplicate artifact names in manifest")

    if mode == "stage" and len(targets) == 1:
        filename = archives[f"{prefix}-build-{targets[0].replace('/', '-')}"]
        checksum = verify(source, filename)
        destination.mkdir()
        shutil.copyfile(source / filename, destination / filename)
        (destination / CHECKSUMS).write_text(checksum, encoding="ascii", newline="\n")
        print((destination / filename).as_posix())
    elif mode == "merge" and not targets:
        require_entries(source, archives)
        verified = []
        for artifact, filename in sorted(archives.items(), key=lambda item: item[1]):
            folder = source / artifact
            require_entries(folder, {filename, CHECKSUMS})
            verified.append((folder, filename, verify(folder, filename)))
        destination.mkdir()
        for folder, filename, _ in verified:
            shutil.copyfile(folder / filename, destination / filename)
            print(f"Verified {filename}")
        (destination / CHECKSUMS).write_text("".join(line for _, _, line in verified), encoding="ascii", newline="\n")
    else:
        raise ValueError("expected stage SOURCE DEST OS/ARCH or merge SOURCE DEST")


if __name__ == "__main__":
    try:
        main(*sys.argv[1:])
    except (OSError, ValueError, KeyError) as error:
        sys.exit(str(error))
