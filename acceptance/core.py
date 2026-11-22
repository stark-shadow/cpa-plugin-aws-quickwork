#!/usr/bin/env python3
"""Exercise released core binaries and an exact packaged plugin, loopback only."""
import argparse
import json
import os
import select
import signal
import socket
import subprocess
import tempfile
import threading
import time
import zipfile
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

CLIENT = 'fixture-client-key'
MGMT = 'fixture-management-key'
ID = 'aws-quickwork'
API = '/v8/management'
CHAT = '/v1/chat/completions'
CALL = {'id': 'call_fixture_42', 'type': 'function', 'function': {'name': 'fixture_lookup', 'arguments': '{"number":7}'}}
TOOLS = [{'type': 'function', 'function': {'name': 'fixture_lookup', 'parameters': {'type': 'object', 'properties': {'number': {'type': 'integer'}}}}}]
MSGS = [{'role': 'system', 'content': 'fixture instructions'},
        {'role': 'developer', 'content': 'fixture developer instructions'},
        {'role': 'user', 'content': 'fixture prompt'},
        {'role': 'assistant', 'content': None, 'tool_calls': [CALL]},
        {'role': 'tool', 'tool_call_id': CALL['id'], 'content': 'fixture result'}]
USAGE = {'prompt_tokens': 12, 'completion_tokens': 3, 'total_tokens': 15, 'prompt_tokens_details': {'cached_tokens': 2}}
MODES = ['smart', 'fast', 'balanced', 'auto']


class Bridge(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def response(self, status, body, retry=None):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json; charset=utf-8')
        self.send_header('Content-Length', str(len(raw)))
        if retry:
            self.send_header('Retry-After', str(retry))
        self.end_headers()
        self.wfile.write(raw)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', '0'))))
        auth_ok = self.headers.get('Authorization') == 'Bearer fixture-bridge-' + self.server.account
        self.server.calls.append({'path': self.path, 'authorized': auth_ok, 'request': body,
                                  'thread_header': bool(self.headers.get('X-Thread-Id'))})
        if self.path != CHAT or not auth_ok:
            self.response(401, {'error': 'private-reflection-marker'})
            return
        model = body['model']
        if self.server.throttle and model == 'fast':
            self.response(429, {'error': 'private-reflection-marker', 'remaining': 0}, 37)
            return
        if not body.get('stream'):
            self.response(200, {'id': 'chatcmpl-fixture', 'object': 'chat.completion', 'created': 1, 'model': model,
                                'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': None, 'tool_calls': [CALL]},
                                             'finish_reason': 'tool_calls'}], 'usage': USAGE})
            return
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream; charset=utf-8')
        self.end_headers()

        def data(value):
            raw = value if isinstance(value, str) else json.dumps(value)
            self.wfile.write(('data: ' + raw + '\n\n').encode())
            self.wfile.flush()

        def chunk(delta, finish=None):
            return dict(id='chatcmpl-fixture', object='chat.completion.chunk', created=1, model=model,
                        choices=[dict(index=0, delta=delta, finish_reason=finish)])

        data(chunk({'role': 'assistant', 'tool_calls': [dict(CALL, index=0)]}))
        if self.server.gate:
            assert self.server.release.wait(7), 'stream waited for full upstream body before emitting'
        data(chunk({}, 'tool_calls'))
        data(dict(chunk({}), choices=[], usage=USAGE))
        data('[DONE]')


@contextmanager
def bridge(account):
    server = ThreadingHTTPServer(('127.0.0.1', 0), Bridge)
    server.account, server.calls, server.throttle, server.gate = account, [], False, False
    server.release = threading.Event()
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    try:
        yield server, f'http://127.0.0.1:{server.server_port}'
    finally:
        server.release.set()
        server.shutdown()
        server.server_close()
        t.join(2)


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


class Core:
    def __init__(self, core, archive, root, alpha, beta, profile, output):
        self.cwd, self.port = root / profile, free_port()
        for d in ('home', 'auth', 'plugins'):
            (self.cwd / d).mkdir(parents=True, exist_ok=True)
        self.log = output / (profile + '.core.log')
        cfg = f'''host: "127.0.0.1"
port: {self.port}
auth-dir: "{self.cwd / 'auth'}"
request-retry: 0
max-retry-credentials: 1
api-keys: ["{CLIENT}"]
remote-management:
  allow-remote: false
  secret-key: "{MGMT}"
  disable-control-panel: true
'''
        if profile == 'native':
            with zipfile.ZipFile(archive) as z:
                dest = self.cwd / 'plugins/linux/amd64/aws-quickwork-v0.1.0.so'
                dest.parent.mkdir(parents=True)
                dest.write_bytes(z.read(ID + '.so'))
            cfg += f'''routing:
  force-model-prefix: true
plugins:
  enabled: true
  dir: "{self.cwd / 'plugins'}"
  configs:
    {ID}:
      enabled: true
      bridge_origins: ["{alpha}", "{beta}"]
      request_timeout: "8s"
'''
            file = self.cwd / 'auth/qw-alpha.json'
            file.write_text(json.dumps(credential('alpha', alpha)))
            file.chmod(0o600)
            (self.cwd / 'auth/foreign.json').write_text(json.dumps(dict(type='foreign-fixture', label='foreign')))
        else:
            cfg += f'''openai-compatibility:
  - name: fixture-baseline
    base-url: "{alpha}/v1"
    api-key-entries:
      - api-key: "fixture-bridge-alpha"
    models:
      - name: smart
        alias: builtin-quickwork
'''
        (self.cwd / 'config.yaml').write_text(cfg)
        self.env = {'PATH': os.environ['PATH'], 'HOME': str(self.cwd / 'home'), 'XDG_CONFIG_HOME': str(self.cwd / 'home'), 'NO_PROXY': '127.0.0.1,localhost',
                    'HTTP_PROXY': 'http://127.0.0.1:1', 'HTTPS_PROXY': 'http://127.0.0.1:1', 'ALL_PROXY': 'http://127.0.0.1:1'}
        with self.log.open('wb') as log:
            self.proc = subprocess.Popen([str(core), '-local-model', '-config', 'config.yaml'], cwd=self.cwd,
                                         env=self.env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)

    def __enter__(self):
        return self

    def __exit__(self, *_):
        if self.proc.poll() is None:
            os.killpg(self.proc.pid, signal.SIGTERM)
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(self.proc.pid, signal.SIGKILL)
                self.proc.wait(timeout=5)
                raise

    def curl(self, route, body=None, method=None, extra=()):
        args = ['curl', '--noproxy', '*', '-sS', '-N', '--max-time', '9', '-H',
                'Authorization: Bearer ' + (MGMT if route.startswith(API) else CLIENT)]
        if body is not None:
            args += ['-H', 'Content-Type: application/json', '--data-binary', json.dumps(body)]
        if method:
            args += ['-X', method]
        return args + list(extra) + [f'http://127.0.0.1:{self.port}{route}']

    def call(self, route, body=None, method=None, include_headers=False):
        extra = ('-i',) if include_headers else ()
        result = subprocess.check_output(self.curl(route, body, method, extra + ('-w', '\n%{http_code}', '-H', 'X-Thread-Id: fixture-test')),
                                         stderr=subprocess.DEVNULL, env=self.env)
        raw, code = result.rsplit(b'\n', 1)
        assert b'private-reflection-marker' not in raw, 'upstream error leaked'
        return int(code), raw

    def obj(self, route, body=None):
        code, data = self.call(route, body)
        assert code == 200, (route, code, data[:180])
        return json.loads(data)

    def ready(self, model):
        for _ in range(150):
            assert self.proc.poll() is None, f'core exited; read {self.log}'
            try:
                if any(d['id'] == model for d in self.obj('/v1/models')['data']):
                    return
            except (OSError, subprocess.SubprocessError):
                pass
            time.sleep(.10)
        raise AssertionError(f'model {model} missing; read {self.log}')


def credential(account, origin):
    return dict(type=ID, label=f'fixture-{account}', prefix=account, bridge_origin=origin,
                bridge_key='fixture-bridge-' + account)


def request(model, stream):
    value = dict(model=model, messages=MSGS, tools=TOOLS, tool_choice='auto', parallel_tool_calls=False, stream=stream)
    if stream:
        value['stream_options'] = {'include_usage': True}
    return value


def trace(item, mode, stream):
    data = item['request']
    expected = request(mode, stream)
    return dict(path=item['path'], authorized=item['authorized'], thread_forwarded=item['thread_header'],
                **{field: data.get(field) == val for field, val in expected.items()})


def streamed(core, server):
    server.gate = True
    model = f'beta/{ID}-fast'
    p = subprocess.Popen(core.curl(CHAT, request(model, True), extra=['-w', '\n%{http_code}', '-H', 'X-Thread-Id: fixture-test']),
                         env=core.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        assert select.select([p.stdout], [], [], 5)[0], 'no stream data before fixture released final chunks'
        first = p.stdout.readline()
        assert first.startswith(b'data:'), first[:100]
        server.release.set()
        # Read from the same buffered stream object (communicate may otherwise
        # bypass data already pulled into this buffer by readline).
        raw = first + p.stdout.read()
        assert p.wait(timeout=3) == 0
    finally:
        server.release.set()
        server.gate = False
        if p.poll() is None:
            p.kill()
            p.wait()
    raw, status = raw.rsplit(b'\n', 1)
    assert status == b'200', status
    events = [line[len(b'data: '):].strip() for line in raw.splitlines() if line.startswith(b'data: ')]
    assert events[-1] == b'[DONE]' and events.count(b'[DONE]') == 1, events
    chunks = [json.loads(event) for event in events[:-1]]
    assert chunks[0]['choices'][0]['delta']['tool_calls'][0]['id'] == CALL['id'], chunks
    assert chunks[-1]['usage'] == USAGE, chunks
    return 'first SSE frame reached client before upstream completion'


def run(core, archive, root, output):
    matrix = {}
    with bridge('alpha') as (a, alpha), bridge('beta') as (b, beta):
        with Core(core, archive, root, alpha, beta, 'built-in', output) as server:
            server.ready('builtin-quickwork')
            for stream in (False, True):
                code, data = server.call(CHAT, request('builtin-quickwork', stream))
                assert code == 200 and CALL['id'].encode() in data, ('baseline', code)
                matrix['built-in-sse' if stream else 'built-in-json'] = trace(a.calls[-1], 'smart', stream)
        a.calls.clear()
        with Core(core, archive, root, alpha, beta, 'native', output) as server:
            server.ready(f'alpha/{ID}-smart')
            assert server.obj(API + '/credentials?name=qw-beta.json', credential('beta', beta)) == {'status': 'ok'}
            server.ready(f'beta/{ID}-fast')
            known = {d['id'] for d in server.obj('/v1/models')['data']}
            assert all(f'{account}/{ID}-{mode}' in known for account in ('alpha','beta') for mode in MODES), known
            plugins = server.obj(API + '/plugins')['plugins']
            item = next(d for d in plugins if d['id'] == ID)
            assert item.get('registered') and item.get('supports_quota') and item.get('quota_provider') == ID, item
            creds = server.obj(API + '/credentials')['files']
            creds = {d['name']:d for d in creds}
            indexes = {account: creds[f'qw-{account}.json']['auth_index'] for account in ('alpha', 'beta')}
            assert all(indexes.values()) and indexes['alpha'] != indexes['beta'], indexes
            for account in indexes:
                entry = creds[f'qw-{account}.json']
                assert entry.get('provider') == ID and entry.get('supports_quota') and entry.get('quota_plugin_id') == ID, entry
            matrix['registration'] = dict(provider=ID, models_per_credential=4, two_distinct_auth_indexes=True,
                                          startup_file=True, management_upload=True)
            code, _ = server.call(CHAT, request(f'{ID}-smart', False))
            assert code >= 400 and not a.calls and not b.calls, ('unprefixed route was not fenced', code)
            matrix['registration']['unprefixed_route_fenced'] = True

            def quota(index):
                return server.obj(API + '/plugins/' + ID + '/quota', {'auth_index':index})

            unknown = {'groups': [{'displayName': 'Amazon QuickWork — quota unknown'}]}
            for index in indexes.values():
                assert quota(index) == unknown
            assert not a.calls and not b.calls, 'quota incorrectly makes an upstream request'
            assert server.call(API + '/plugins/' + ID + '/quota', {'auth_index': 'unknown-index'})[0] == 404
            foreign = creds['foreign.json']['auth_index']
            assert server.call(API + '/plugins/' + ID + '/quota', {'auth_index': foreign})[0] in (404, 502)
            reset_code, _ = server.call(API + '/plugins/' + ID + '/quota', {'auth_index': indexes['alpha']}, 'DELETE')
            assert reset_code >= 400

            result = server.obj(CHAT, request(f'alpha/{ID}-smart', False))
            assert result['choices'][0]['message']['tool_calls'] == [CALL]
            assert result['usage'] == USAGE
            check = trace(a.calls[-1], 'smart', False)
            assert not check.pop('thread_forwarded') and check.pop('path') == CHAT and all(check.values()), check
            matrix['native-json'] = trace(a.calls[-1], 'smart', False)
            matrix['early_stream'] = streamed(server, b)
            check = trace(b.calls[-1], 'fast', True)
            assert not check.pop('thread_forwarded') and check.pop('path') == CHAT and all(check.values()), check
            matrix['native-sse'] = trace(b.calls[-1], 'fast', True)

            a.throttle = True
            before = (len(a.calls), len(b.calls))
            code, data = server.call(CHAT, request(f'alpha/{ID}-fast', False), include_headers=True)
            assert code == 429 and b'temporarily rate limited' in data, (code, data)
            assert (len(a.calls), len(b.calls)) == (before[0]+1,before[1]), 'unexpected retry or credential change'
            first_retry = next((line.split(b':', 1)[1].strip().decode() for line in data.splitlines() if line.lower().startswith(b'retry-after:')), None)
            assert first_retry is not None and 30 <= int(first_retry) <= 37, first_retry
            assert quota(indexes['alpha']) == quota(indexes['beta']) == unknown
            assert (len(a.calls), len(b.calls)) == (before[0]+1,before[1])
            a.throttle = False
            cooldown_code, _ = server.call(CHAT, request(f'alpha/{ID}-fast', False))
            assert cooldown_code in (429, 503) and (len(a.calls), len(b.calls)) == (before[0]+1,before[1]), cooldown_code
            assert server.call(CHAT, request(f'alpha/{ID}-balanced', False))[0] == 200
            assert server.call(CHAT, request(f'beta/{ID}-fast', False))[0] == 200
            matrix['quota'] = dict(two_indexes='unknown', missing_index=404, cross_provider_rejected=True,
                                   read_only_no_probe=True, reset_rejected=reset_code, inference_429=code, retry_after_seconds=int(first_retry), cooldown_status=cooldown_code, cooldown_rejected_without_upstream=True,
                                   other_mode_and_account_unaffected=True)
    (output / 'matrix.json').write_text(json.dumps(matrix, indent=2, ensure_ascii=False) + '\n')
    print(json.dumps(matrix, ensure_ascii=False))


if __name__ == '__main__':
    p = argparse.ArgumentParser()
    p.add_argument('core', type=Path)
    p.add_argument('archive', type=Path)
    p.add_argument('output', type=Path)
    opt = p.parse_args()
    opt.output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='quickwork-core-', dir=opt.output) as tmp:
        run(opt.core.resolve(), opt.archive.resolve(), Path(tmp).resolve(), opt.output)
