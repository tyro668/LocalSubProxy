#!/usr/bin/env python3
"""Multi-SaaS acceptance against the native kernel library.

The kernel (liblsp.so) runs in a separate child process that is SIGKILLed
and restarted to check recovery after a real process exit. A slow streaming
OpenAI-compatible model runs in this process so cancellations are visible.

  go build -buildmode=c-shared -o /tmp/liblsp.so ./cmd/localsubproxy_native
  LMBP_MOCK_DIR=/tmp/msmock LMBP_MOCK_GATEWAYS=2 scripts/mock-start.sh go 18480
  scripts/multisaas-acceptance.py /tmp/liblsp.so /tmp/msmock 18480
"""
import json, os, signal, subprocess, sys, tempfile, threading, time, urllib.parse, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

if len(sys.argv) > 1 and sys.argv[1] == '--child':  # kernel process
    import ctypes
    lib = ctypes.CDLL(sys.argv[2])
    for f in (lib.LSP_Start, lib.LSP_Call): f.restype = ctypes.c_void_p; f.argtypes = [ctypes.c_char_p]
    lib.LSP_Free.argtypes = [ctypes.c_void_p]
    for line in sys.stdin:
        req = json.loads(line)
        fn = lib.LSP_Start if req.pop('_start', False) else lib.LSP_Call
        p = fn(json.dumps(req).encode()); s = ctypes.string_at(p).decode(); lib.LSP_Free(p)
        sys.stdout.write(s + '\n'); sys.stdout.flush()
    sys.exit(0)

lib_path, mock_dir, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
env = dict(l.strip().split('=', 1) for l in open(os.path.join(mock_dir, 'env')) if '=' in l)
T0 = time.time()
def log(*a): print('[%6.2fs]' % (time.time() - T0), *a, flush=True)

# --- slow model: "slow N" streams N chunks, 0.15s apart --------------------
model_events = []  # (tag, event)
class Model(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def log_message(self, *a): pass
    def do_GET(self):
        b = json.dumps({'object': 'list', 'data': [{'id': 'slow-1', 'object': 'model'}]}).encode()
        self.send_response(200); self.send_header('Content-Type', 'application/json'); self.send_header('Content-Length', str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', 0))) or b'{}')
        text = body['messages'][-1]['content'] if isinstance(body['messages'][-1]['content'], str) else 'x'
        tag = text.split()[-1]; n = int(text.split()[1]) if text.startswith('slow') else 3
        model_events.append((tag, 'start'))
        self.send_response(200); self.send_header('Content-Type', 'text/event-stream'); self.send_header('Connection', 'close'); self.end_headers()
        try:
            for i in range(n):
                self.wfile.write(('data: ' + json.dumps({'id': 'c', 'object': 'chat.completion.chunk', 'model': 'slow-1', 'choices': [{'index': 0, 'delta': {'content': f'{i} '}}]}) + '\n\n').encode()); self.wfile.flush()
                time.sleep(0.15)
            self.wfile.write(('data: ' + json.dumps({'id': 'c', 'object': 'chat.completion.chunk', 'model': 'slow-1', 'choices': [{'index': 0, 'delta': {}, 'finish_reason': 'stop'}]}) + '\n\ndata: [DONE]\n\n').encode()); self.wfile.flush()
            model_events.append((tag, 'done'))
        except (BrokenPipeError, ConnectionResetError):
            model_events.append((tag, 'cancelled'))
srv = ThreadingHTTPServer(('127.0.0.1', 0), Model); threading.Thread(target=srv.serve_forever, daemon=True).start()
model_url = f'http://127.0.0.1:{srv.server_port}'

# --- kernel child -------------------------------------------------------------
class Kernel:
    def __init__(self, data):
        self.p = subprocess.Popen([sys.executable, __file__, '--child', lib_path], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        self.lock = threading.Lock()
        log('kernel process started pid', self.p.pid)
        self.req({'_start': True, 'data_dir': data})
    def req(self, obj, ok=True):
        with self.lock:
            self.p.stdin.write(json.dumps(obj) + '\n'); self.p.stdin.flush()
            r = json.loads(self.p.stdout.readline())
        if ok and not r['ok']: raise SystemExit(f"{obj.get('action', 'start')}: {r['error']}")
        return r if not ok else r.get('data')
    def call(self, **kw): return self.req(kw)
    def kill(self):
        self.p.send_signal(signal.SIGKILL); self.p.wait(); log('kernel process SIGKILLed, exit', self.p.returncode)

def http(gw, key, method, path, body=None):
    req = urllib.request.Request(gw + path, method=method, data=json.dumps(body).encode() if body is not None else None,
        headers={'Authorization': 'Bearer ' + key, 'X-End-User': 'alice', 'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=30) as r: return json.loads(r.read() or b'{}')

class Stream(threading.Thread):
    """Streams 'slow N tag' through one gateway; can be aborted (client cancel)."""
    def __init__(self, s, n, tag):
        super().__init__(daemon=True); self.s, self.n, self.tag = s, n, tag
        self.chunks, self.result, self.first, self.last, self.resp = 0, None, None, None, None
    def run(self):
        tenant, gw, key = self.s
        mid = model_id(self.s)
        req = urllib.request.Request(gw + '/v1/chat/completions', method='POST', data=json.dumps({'model': mid, 'stream': True,
            'messages': [{'role': 'user', 'content': f'slow {self.n} {self.tag}'}]}).encode(),
            headers={'Authorization': 'Bearer ' + key, 'X-End-User': 'alice', 'Content-Type': 'application/json'})
        try:
            self.resp = urllib.request.urlopen(req, timeout=60)
            for line in self.resp:
                line = line.decode().strip()
                if not line.startswith('data:'): continue
                if line == 'data: [DONE]': self.result = 'done'; break
                d = json.loads(line[5:])
                if 'error' in d: self.result = 'error: ' + json.dumps(d['error'])[:160]; break
                if d.get('choices') and d['choices'][0].get('delta', {}).get('content'):
                    self.chunks += 1; self.last = time.time(); self.first = self.first or self.last
            self.result = self.result or 'eof'
        except Exception as e:
            self.result = self.result or f'error: {type(e).__name__}: {str(e)[:160]}'
    def cancel(self):
        try: self.resp.fp.raw._sock.shutdown(2)
        except Exception as e: log('cancel error', e)
    def wait_chunks(self, k, t=20):
        end = time.time() + t
        while self.chunks < k and time.time() < end: time.sleep(0.05)
        assert self.chunks >= k, (self.tag, self.chunks, self.result)

_mids = {}
def model_id(s):
    if s[0] not in _mids:
        for _ in range(100):
            ids = [m['id'] for m in http(s[1], s[2], 'GET', '/v1/models')['data'] if m['id'].endswith('/slow/slow-1')]
            if ids: _mids[s[0]] = ids[0]; break
            time.sleep(0.2)
    return _mids[s[0]]

def wait_status(k, cond, what, t=30):
    end = time.time() + t
    while time.time() < end:
        st = k.call(action='status')
        if cond(st): return st
        time.sleep(0.2)
    raise SystemExit(f'timeout: {what}: ' + json.dumps(st)[:400])

def states(st): return {c['tenant']: c['state'] for c in st['connections']}
def ev(tag): return [e for t, e in model_events if t == tag]
def wait_ev(tag, e, t=10):
    end = time.time() + t
    while e not in ev(tag) and time.time() < end: time.sleep(0.05)
    assert e in ev(tag), (tag, ev(tag))

data = tempfile.mkdtemp(prefix='lsp-accept-')
k = Kernel(data)
k.call(action='create_adapter', adapter={'id': 'slow', 'type': 'openai_compatible', 'name': 'Slow', 'base_url': model_url + '/v1', 'api_key': 'k',
    'models': ['slow-1'], 'enabled': True, 'skip_model_probe': True, 'concurrency': 8})
A = ('acme', f'http://127.0.0.1:{port}', env['LSP_TENANT_KEY_ACME'])
B = ('beta', f'http://127.0.0.1:{port + 1}', env['LSP_TENANT_KEY_BETA'])
ids = {}
for tenant, gw, key in (A, B):
    c = k.call(action='pair_start', link='lmbp://pair?v=2&gateway=' + urllib.parse.quote(gw, safe='') + '&tenant=' + tenant)
    a = k.call(action='pair_approve', pair_id=c['pair_id'], allow=['slow/*'])
    http(gw, key, 'POST', '/lmbp/v1/tenant/devices/approvals', {'user_code': a['user_code']})
    for _ in range(100):
        s = k.call(action='pair_status', pair_id=c['pair_id'])
        if s['state'] != 'pending': break
        time.sleep(0.2)
    assert s['state'] == 'paired', s
    ids[tenant] = s['connection_id']; log(tenant, 'paired as', ids[tenant])
wait_status(k, lambda s: s['connections_online'] == 2, 'both online')
log('STEP 1 both online 2/2')

# 2. concurrent streaming on both
sa, sb = Stream(A, 12, 'a1'), Stream(B, 12, 'b1'); sa.start(); sb.start(); sa.join(); sb.join()
log('STEP 2 concurrent streams:', 'acme', sa.result, sa.chunks, 'beta', sb.result, sb.chunks,
    'overlap %.2fs' % (min(sa.last, sb.last) - max(sa.first, sb.first)))
assert sa.result == sb.result == 'done' and sa.chunks == sb.chunks == 12 and min(sa.last, sb.last) > max(sa.first, sb.first)

# 3. client cancels acme mid-stream; beta keeps streaming
sa, sb = Stream(A, 40, 'a2'), Stream(B, 20, 'b2'); sa.start(); sb.start(); sa.wait_chunks(3); sb.wait_chunks(3)
sa.cancel(); wait_ev('a2', 'cancelled'); sb.join()
log('STEP 3 cancel acme mid-stream: acme model', ev('a2'), '| beta', sb.result, sb.chunks, 'model', ev('b2'))
assert sb.result == 'done' and sb.chunks == 20
wait_status(k, lambda s: s['running_tasks'] == 0, 'slots freed after cancel')

# 4. disconnect acme (refused while it runs), then while beta streams
sa = Stream(A, 20, 'a3'); sa.start(); sa.wait_chunks(2)
r = k.req({'action': 'connection_disconnect', 'connection_id': ids['acme']}, ok=False)
log('STEP 4a disconnect acme while its task runs ->', r)
assert not r['ok']
sa.join()
sb = Stream(B, 20, 'b3'); sb.start(); sb.wait_chunks(2)
st = k.call(action='connection_disconnect', connection_id=ids['acme'])
log('STEP 4b disconnect acme while beta streams:', states(st))
sb.join(); log('       beta stream', sb.result, sb.chunks)
assert sb.result == 'done' and sb.chunks == 20 and states(st)['acme'] == 'disconnected'
k.call(action='connection_connect', connection_id=ids['acme'])
wait_status(k, lambda s: s['connections_online'] == 2, 'acme reconnected')

# 5. real process exit with both kept, restart, recovery
k.kill(); k = Kernel(data)
st = wait_status(k, lambda s: s['connections_online'] == 2, 'both recovered after SIGKILL')
log('STEP 5 after SIGKILL + restart:', states(st), 'allow', [c['allow'] for c in st['connections']])
sa, sb = Stream(A, 5, 'a5'), Stream(B, 5, 'b5'); sa.start(); sb.start(); sa.join(); sb.join()
assert sa.result == sb.result == 'done'; log('       both serve after restart')

# 6. reconnect handshake interleaved with concurrency changes
def gw_conc(s):
    d = [x for x in http(s[1], s[2], 'GET', '/lmbp/v1/tenant/devices')['devices'] if x.get('online')]
    return d[0].get('max_concurrency') if d else None
last = None
for i in range(8):
    want = 1 + (i % 3)
    k.call(action='connection_disconnect', connection_id=ids['acme'])
    k.call(action='connection_connect', connection_id=ids['acme'])  # reconnect starts
    time.sleep(0.005 * i)  # land the change at different handshake points
    k.call(action='connection_policy_set', connection_id=ids['acme'], policy={'max_concurrency': want})
    last = want
    end = time.time() + 10
    while time.time() < end and gw_conc(A) != want: time.sleep(0.1)
    got = gw_conc(A); log(f'STEP 6.{i} reconnect + max_concurrency={want} -> gateway sees {got}')
    assert got == want
# a task stream still works with the final setting
sa = Stream(A, 3, 'a6'); sa.start(); sa.join(); assert sa.result == 'done'

# 7. remote revoke acme while both stream: acme cancelled, beta continues
sa, sb = Stream(A, 40, 'a7'), Stream(B, 25, 'b7'); sa.start(); sb.start(); sa.wait_chunks(3); sb.wait_chunks(3)
r = k.req({'action': 'connection_revoke', 'connection_id': ids['acme']}, ok=False)
log('STEP 7a local revoke while acme task runs ->', r['ok'], r.get('error'))
dev = [x for x in http(A[1], A[2], 'GET', '/lmbp/v1/tenant/devices')['devices'] if x['status'] == 'active'][0]
http(A[1], A[2], 'POST', f"/lmbp/v1/tenant/devices/{dev['device_id']}/revoke")
wait_ev('a7', 'cancelled', 15); sa.join(); sb.join()
st = wait_status(k, lambda s: states(s).get('acme') == 'revoked', 'acme revoked')
log('STEP 7b remote revoke acme: acme model', ev('a7'), 'client', sa.result, '| beta', sb.result, sb.chunks, '| states', states(st), 'running', st['running_tasks'])
assert sb.result == 'done' and sb.chunks == 25 and st['running_tasks'] == 0

# 8. local revoke (cleanup) + restart: only beta remains
st = k.call(action='connection_revoke', connection_id=ids['acme'])
log('STEP 8 local revoke acme: removed', st.get('removed'), 'warning', st.get('warning'), 'pending', st.get('cleanup_pending'))
k.kill(); k = Kernel(data)
st = wait_status(k, lambda s: s['connections_online'] == 1, 'beta after restart')
log('       after SIGKILL + restart:', states(st), 'key dirs', sorted(os.listdir(os.path.join(data, 'bindings'))))
assert list(states(st)) == ['beta'] and sorted(os.listdir(os.path.join(data, 'bindings'))) == [ids['beta']]
k.call(action='stop'); k.p.stdin.close(); k.p.wait()
log('ACCEPTANCE OK')
