#!/usr/bin/env python3
"""Multi-SaaS repro against the native kernel library (no Flutter needed).

  go build -buildmode=c-shared -o /tmp/liblsp.so ./cmd/avora_native   # macOS: liblsp.dylib
  LMBP_MOCK_GATEWAYS=2 scripts/mock-start.sh go 8080
  scripts/multisaas-repro.py /tmp/liblsp.so [.mock] [8080]
  scripts/mock-stop.sh

Pairs one fresh data dir with both mock gateways (acme on port, beta on
port+1), checks status shows 2/2, chats through both, then disconnects and
revokes acme and restarts the kernel: only beta must remain.
"""
import ctypes, json, os, sys, time, tempfile, urllib.parse, urllib.request
lib_path = sys.argv[1] if len(sys.argv) > 1 else '/tmp/liblsp.so'
mock_dir = sys.argv[2] if len(sys.argv) > 2 else os.environ.get('LMBP_MOCK_DIR', '.mock')
port = int(sys.argv[3]) if len(sys.argv) > 3 else 8080
env = dict(l.strip().split('=',1) for l in open(os.path.join(mock_dir, 'env')) if '=' in l)
lib = ctypes.CDLL(lib_path)
for f in (lib.LSP_Start, lib.LSP_Call): f.restype = ctypes.c_void_p; f.argtypes = [ctypes.c_char_p]
lib.LSP_Free.argtypes = [ctypes.c_void_p]
def go(fn, obj):
    p = fn(json.dumps(obj).encode()); s = ctypes.string_at(p).decode(); lib.LSP_Free(p)
    r = json.loads(s)
    if not r['ok']: raise SystemExit(f"{obj.get('action')}: {r['error']}")
    return r.get('data')
def call(**kw): return go(lib.LSP_Call, kw)
def http(gw, key, method, path, body=None):
    req = urllib.request.Request(gw+path, method=method, data=json.dumps(body).encode() if body is not None else None,
        headers={'Authorization':'Bearer '+key,'X-End-User':'alice','Content-Type':'application/json'})
    with urllib.request.urlopen(req, timeout=30) as r: return json.loads(r.read() or b'{}')
data = tempfile.mkdtemp()
st = go(lib.LSP_Start, {'data_dir': data})
call(action='create_adapter', adapter={'id':'local','type':'ollama_http','name':'Local','base_url':'http://127.0.0.1:'+os.environ.get('LMBP_MOCK_MODEL_PORT','11500'),'models':['qwen3-8b'],'enabled':True})
saas = [('acme', f'http://127.0.0.1:{port}', env['LSP_TENANT_KEY_ACME']), ('beta', f'http://127.0.0.1:{port+1}', env['LSP_TENANT_KEY_BETA'])]
for tenant, gw, key in saas:
    link = 'lmbp://pair?v=2&gateway=' + urllib.parse.quote(gw, safe='') + '&tenant=' + tenant
    c = call(action='pair_start', link=link)
    print(tenant, 'consent', c['consent']['saas']['name'], 'available', c['available_models'])
    a = call(action='pair_approve', pair_id=c['pair_id'], allow=['local/*'])
    http(gw, key, 'POST', '/lmbp/v1/tenant/devices/approvals', {'user_code': a['user_code']})
    for _ in range(100):
        s = call(action='pair_status', pair_id=c['pair_id'])
        if s['state'] != 'pending': break
        time.sleep(0.2)
    assert s['state'] == 'paired', s
    print(tenant, 'paired as', s['connection_id'])
for _ in range(150):
    st = call(action='status')
    if st['connections_online'] == 2: break
    time.sleep(0.2)
print('status: 已连接 %d/%d' % (st['connections_online'], st['connections_total']))
assert st['connections_online'] == 2 and st['connections_total'] == 2, st['connections']
for tenant, gw, key in saas:
    ms = http(gw, key, 'GET', '/v1/models')
    mid = [m['id'] for m in ms['data'] if m['id'].endswith('/local/qwen3-8b')][0]
    r = http(gw, key, 'POST', '/v1/chat/completions', {'model': mid, 'messages':[{'role':'user','content':'hello from '+tenant}]})
    print(tenant, 'chat ->', r['choices'][0]['message']['content'][:60].replace('\n',' '))
u = call(action='usage_stats', group_by=['saas'])
print('usage by saas:', [(b['saas'], b.get('gateway'), b['requests']) for b in u['buckets']])
assert len(u['buckets']) == 2
for c in st['connections']: print(' ', c['id'], c['saas_name'], c['issuer'], c['tenant'], c['state'], c['allow'])
# per-connection disconnect + revoke, then restart keeps the remaining binding
a_id = st['connections'][0]['id']
st = call(action='connection_disconnect', connection_id=a_id)
print('after disconnect acme:', [(c['tenant'], c['state']) for c in st['connections']])
st = call(action='connection_revoke', connection_id=a_id)
print('after revoke acme:', [(c['tenant'], c['state']) for c in st['connections']])
call(action='stop'); st = go(lib.LSP_Start, {'data_dir': data})
for _ in range(100):
    st = call(action='status')
    if st['connections_online'] == 1: break
    time.sleep(0.2)
print('after restart: %d/%d' % (st['connections_online'], st['connections_total']), st['tenant'])
assert st['connections_total'] == 1 and st['tenant'] == 'beta' and st['connections_online'] == 1
call(action='stop'); print('REPRO OK')
