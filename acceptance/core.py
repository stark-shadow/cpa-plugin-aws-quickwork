import argparse
import json
import os
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from pathlib import Path


def run(core, archive):
    management_key = 'local-quickwork-smoke-key'
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with tempfile.TemporaryDirectory(prefix='quickwork-native-') as tmp, zipfile.ZipFile(archive) as release:
        root = Path(tmp)
        manifest = json.loads(release.read('manifest.json'))
        plugin_id = manifest['id']
        version = manifest['version']
        descriptor = json.loads(release.read(plugin_id + '.omc.json'))
        target, extension = ('windows', '.dll') if os.name == 'nt' else ('linux', '.so')
        plugins = root / 'plugins' / target / 'amd64'
        plugins.mkdir(parents=True)
        (plugins / f'{plugin_id}-v{version}{extension}').write_bytes(release.read(plugin_id + extension))
        auth = root / 'auth'
        auth.mkdir()
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        (root / 'config.yaml').write_text(f'''host: '127.0.0.1'
port: {port}
auth-dir: '{auth.as_posix()}'
remote-management:
  secret-key: '{management_key}'
  disable-control-panel: true
plugins:
  enabled: true
  dir: '{(root / 'plugins').as_posix()}'
  configs:
    {plugin_id}:
      enabled: true
''', encoding='utf-8')

        def request(path, payload=None, authorized=True):
            headers = {'Authorization': f'Bearer {management_key}'} if authorized else {}
            body = None
            if payload is not None:
                headers['Content-Type'] = 'application/json'
                body = json.dumps(payload).encode()
            req = urllib.request.Request(f'http://127.0.0.1:{port}{path}', body, headers)
            try:
                with opener.open(req, timeout=3) as response:
                    return response.status, json.load(response)
            except urllib.error.HTTPError as failure:
                return failure.code, None

        logfile = root / 'core.log'
        with logfile.open('wb') as log:
            flags = subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0
            process = subprocess.Popen([str(core), '-local-model', '-config', 'config.yaml'],
                                       stdout=log, stderr=subprocess.STDOUT, cwd=root, creationflags=flags)
            try:
                for _ in range(100):
                    assert process.poll() is None, f'CPA exited: {process.returncode}'
                    try:
                        status, result = request(f'/v0/resource/plugins/{plugin_id}/omc.json', authorized=False)
                        if status == 200:
                            assert result == descriptor
                            break
                    except urllib.error.URLError:
                        pass
                    time.sleep(.1)
                else:
                    raise AssertionError('CPA did not expose the plugin descriptor')
                status, listing = request('/v8/management/plugins')
                assert status == 200
                plugin = next(entry for entry in listing['plugins'] if entry['id'] == plugin_id)
                entries = {entry['identifier']: entry for entry in plugin['oauth_provider_entries']}
                assert set(entries) == {f'{plugin_id}-personal', f'{plugin_id}-enterprise'}, set(entries)
                for provider in entries.values():
                    with opener.open(f"http://127.0.0.1:{port}{provider['logo']}", timeout=3) as image:
                        assert image.headers.get_content_type() == 'image/svg+xml' and image.read().startswith(b'<svg ')
                for account in ('personal', 'enterprise'):
                    provider = f'{plugin_id}-{account}'
                    status, login = request('/v8/management/oauth/auth-url?provider=' + provider)
                    assert status == 200 and login['state'] and login['url'].startswith('https://'), (provider, status)
                    url = urllib.parse.urlsplit(login['url'])
                    discovery = urllib.parse.parse_qs(url.query)['qwSurface'][0].split(':')
                    state = login['state']
                    first = f'http://127.0.0.1:{discovery[2]}/callback?nonce=wrong'
                    status, _ = request(f'/v0/management/plugins/{plugin_id}/oauth/discovery',
                                        {'state': state, 'redirect_url': first})
                    assert status == 400, (provider, status)
                    status, _ = request(f'/v0/management/plugins/{plugin_id}/oauth/discovery',
                                        {'state': state, 'redirect_url': first}, authorized=False)
                    assert status in (401, 403), (provider, status)
                    status, pending = request('/v8/management/oauth/status?state=' + state)
                    assert status == 200 and pending['status'] == 'wait', (provider, status, pending)
                    final = f'http://127.0.0.1:{discovery[2]}/?code=fixture&state={state}'
                    status, _ = request('/v8/management/oauth/callback', {'redirect_url': final})
                    assert status == 200 and (auth / f'.oauth-{provider}-{state}.oauth').is_file(), (provider, status)
                print(f'CPA loaded {plugin_id} {version}; both native OAuth routes and pasted callback handoff passed.')
            except Exception:
                print(logfile.read_text(encoding='utf-8', errors='replace')[-5000:])
                raise
            finally:
                if process.poll() is None:
                    process.terminate()
                    process.wait(timeout=10)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('core', type=Path)
    parser.add_argument('archive', type=Path)
    args = parser.parse_args()
    run(args.core.resolve(), args.archive.resolve())
