// E2E for mock/web/chat.html: for each gateway implementation, start it with
// the chat UI, pair the real Go connector (localsubproxy connector, Ollama
// adapter pointed at the fake model's /api/chat) through the page, chat with
// streaming, multi-turn, image upload, stop, CONSENT_DENIED and DEVICE_OFFLINE.
//
//   node chat-ui.mjs [go node python]
// env: LSP2_BIN (dir with localsubproxy + lspmock), CHROME (browser path),
//      SHOTS (screenshot dir), PYTHON (python with aiohttp/cryptography)
import { spawn } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { chromium } from 'playwright-core';

const repo = path.resolve(new URL('../../..', import.meta.url).pathname);
const bin = process.env.LSP2_BIN || path.join(repo, 'bin');
const shots = process.env.SHOTS || path.join(os.tmpdir(), 'lsp2-chat-shots');
const chrome = process.env.CHROME || '/usr/bin/google-chrome';
const python = process.env.PYTHON || path.join(repo, 'mock/python/.venv/bin/python');
const impls = process.argv.slice(2).length ? process.argv.slice(2) : ['go', 'node', 'python'];
fs.mkdirSync(shots, { recursive: true });
const key = crypto.randomBytes(24).toString('hex');            // tenant API key, never printed
const env = { ...process.env, LSP_TENANT_KEY_ACME: key, LOCALSUBPROXY_DATA_KEY: crypto.randomBytes(32).toString('hex') };
const procs = [];
const run = (cmd, args, opts = {}) => {
  const p = spawn(cmd, args, { env, stdio: ['ignore', 'pipe', 'pipe'], ...opts });
  p.out = ''; p.stdout.on('data', (d) => (p.out += d)); p.stderr.on('data', (d) => (p.out += d));
  procs.push(p); return p;
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms, what) {
  const end = Date.now() + ms;
  for (;;) { const v = await fn().catch(() => null); if (v) return v; if (Date.now() > end) throw new Error('timeout: ' + what); await sleep(200); }
}
const cleanup = () => { for (const p of procs) try { p.kill('SIGKILL'); } catch { /* gone */ } };
process.on('exit', cleanup);

const profiles = path.join(repo, 'mock/tenant-profiles.example.json');
function startGateway(impl, port) {
  const listen = `127.0.0.1:${port}`, iss = `http://${listen}`;
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), `lsp2-${impl}-`));
  switch (impl) {
    case 'go': return run(path.join(bin, 'localsubproxy'), ['server', '-listen', listen, '-db', path.join(tmp, 'g.db'), '-tenants', 'acme:LSP_TENANT_KEY_ACME',
      '-tenant-profiles', profiles, '-issuer', iss, '-chat-ui']);
    case 'node': return run('node', [path.join(repo, 'mock/node/gateway.mjs'), '--listen', listen, '--issuer', iss, '--tenant-profiles', profiles]);
    case 'python': return run(python, [path.join(repo, 'mock/python/gateway.py'), '--listen', listen, '--issuer', iss, '--tenant-profiles', profiles]);
  }
  throw new Error('unknown impl ' + impl);
}
function startConnector(dir, link, modelURL) {
  const a = ['connector', '-yes', '-identity', path.join(dir, 'id.json'), '-adapter-id', 'local', '-ollama-url', modelURL, '-models', 'qwen3-8b',
    '-capabilities', 'vision', '-adapter-file', path.join(dir, 'adapters.enc'), '-audit-db', path.join(dir, 'audit.db')];
  if (link) a.push('-pair-link', link);
  return run(path.join(bin, 'localsubproxy'), a, { cwd: dir });
}

const modelPort = 11590;
const fm = run(path.join(bin, 'lspmock'), ['fakemodel', '-listen', `127.0.0.1:${modelPort}`, '-models', 'qwen3-8b']);
await until(async () => (await fetch(`http://127.0.0.1:${modelPort}/api/tags`)).ok, 10000, 'fake model');

const browser = await chromium.launch({ executablePath: chrome, args: ['--no-sandbox'] });
const results = {};
let port = 18970;
for (const impl of impls) {
  const t0 = Date.now(), checks = [];
  const ok = (name) => { checks.push(name); console.log(`  [${impl}] ok  ${name}`); };
  let gw, con;
  const page = await browser.newPage({ viewport: { width: 1360, height: 900 }, locale: 'zh-CN' });
  const consoleErrors = [];
  page.on('pageerror', (e) => consoleErrors.push(String(e)));
  // HTTP error statuses are expected (the test provokes 401/403/503); only JS exceptions and CSP violations fail.
  page.on('console', (m) => { if (m.type() === 'error' && !m.text().startsWith('Failed to load resource')) consoleErrors.push(m.text()); });
  try {
    port += 1;
    const base = `http://127.0.0.1:${port}`;
    gw = startGateway(impl, port);
    await until(async () => (await fetch(base + '/healthz')).ok, 15000, impl + ' gateway');
    const r = await fetch(base + '/chat');
    if (!r.ok || !(r.headers.get('content-security-policy') || '').includes("script-src 'self'")) throw new Error('chat page/CSP missing');
    if ((await r.text()).includes(key)) throw new Error('key leaked into HTML');
    ok('GET /chat serves the page with CSP, no key in HTML');

    // login
    await page.goto(base + '/chat');
    await page.fill('#key', 'x'.repeat(40)); await page.click('#loginForm button[type=submit]');
    await page.waitForFunction(() => document.getElementById('loginErr').textContent.length > 0);
    ok('wrong key rejected: ' + (await page.textContent('#loginErr')));
    await page.fill('#key', key); await page.click('#loginForm button[type=submit]');
    await page.waitForSelector('#app:not(.hidden)');
    await page.waitForFunction(() => document.getElementById('pairLink').textContent.startsWith('lsp2://pair'));
    const link = await page.textContent('#pairLink');
    if (!(await page.$('#qr img'))) throw new Error('no QR code');
    ok('logged in; pair link + QR: ' + link);

    // pair the real connector through the page
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), `lsp2-dev-${impl}-`));
    con = startConnector(dir, link, `http://127.0.0.1:${modelPort}`);
    const code = await until(async () => (con.out.match(/pairing code: ([A-Z]{4}-[A-Z]{4})/) || [])[1], 15000, 'pairing code');
    const jkt = (con.out.match(/jkt=([A-Za-z0-9_-]{43})/) || [])[1];
    await page.fill('#code', code); if (jkt) await page.fill('#jkt', jkt);
    await page.click('#approve');
    await page.waitForFunction(() => document.getElementById('pairMsg').textContent.includes('已批准'));
    await page.waitForSelector('#devices .badge.on', { timeout: 20000 });
    await page.waitForFunction(() => [...document.querySelectorAll('#model option')].some((o) => o.value), null, { timeout: 15000 });
    await page.screenshot({ path: path.join(shots, `chat-${impl}-paired.png`) });
    ok(`paired ${code} (jkt ${jkt ? 'checked' : 'n/a'}); device online with models`);

    const send = async (text) => {
      const n = await page.$$eval('.turn.assistant', (x) => x.length);
      await page.fill('#input', text); await page.press('#input', 'Enter');
      await page.waitForFunction((n) => { const t = document.querySelectorAll('.turn.assistant')[n]; return t && t.dataset.state; }, n, { timeout: 30000 });
      return page.evaluate((n) => { const t = document.querySelectorAll('.turn.assistant')[n];
        return { state: t.dataset.state, text: t.querySelector('.bubble').textContent, meta: t.querySelector('.meta').textContent, err: t.querySelector('.err')?.textContent || '' }; }, n);
    };
    // streaming: watch the bubble grow while the request is in flight
    const growth = page.evaluate(() => new Promise((res) => {
      const seen = new Set();
      const iv = setInterval(() => { const b = document.querySelector('.turn.assistant:last-child .bubble'); if (b) seen.add(b.textContent.length); if (seen.size >= 4) { clearInterval(iv); res(seen.size); } }, 20);
      setTimeout(() => { clearInterval(iv); res(seen.size); }, 20000);
    }));
    let a = await send('你好');
    const sizes = await growth;
    if (a.state !== 'done' || !a.text.includes('你说的是：你好') || !/tokens 输入 \d+ \/ 输出 \d+/.test(a.meta) || !a.meta.includes('首字')) throw new Error('turn 1: ' + JSON.stringify(a));
    if (sizes < 4) throw new Error('reply did not stream incrementally');
    ok(`streamed reply (${sizes} intermediate lengths): "${a.text.slice(0, 40)}…" | ${a.meta}`);
    a = await send('再来一轮');
    if (!a.text.includes('第 2 轮')) throw new Error('multi-turn history not sent: ' + a.text);
    ok('multi-turn: model saw 2 user turns');

    // image upload (> 16 KiB → lsp2-blob + blob.chunk on the wire)
    const png = await page.evaluate(() => { const c = document.createElement('canvas'); c.width = c.height = 200; const x = c.getContext('2d');
      for (let i = 0; i < 200; i += 2) for (let j = 0; j < 200; j += 2) { x.fillStyle = `rgb(${Math.random() * 255 | 0},${i},${Math.random() * 255 | 0})`; x.fillRect(i, j, 2, 2); }
      return c.toDataURL('image/png').split(',')[1]; });
    const buf = Buffer.from(png, 'base64');
    await page.setInputFiles('#file', { name: 'noise.png', mimeType: 'image/png', buffer: buf });
    await page.waitForSelector('#attInfo img');
    if (buf.length <= 16384) throw new Error('test image too small for the blob path');
    a = await send('这张图是什么？');
    if (!a.text.includes(`我看到了一张图片，${buf.length} 字节`)) throw new Error('image: ' + a.text);
    ok(`image upload ${buf.length} bytes reached the local model intact`);

    // stop / cancel
    const n = await page.$$eval('.turn.assistant', (x) => x.length);
    await page.fill('#input', '请给一个长回答'); await page.press('#input', 'Enter');
    await page.waitForFunction((n) => (document.querySelectorAll('.turn.assistant')[n]?.querySelector('.bubble').textContent.length || 0) > 10, n);
    await page.click('#stop');
    await page.waitForFunction((n) => document.querySelectorAll('.turn.assistant')[n]?.dataset.state === 'stopped', n);
    const stopped = await page.evaluate((n) => document.querySelectorAll('.turn.assistant')[n].querySelector('.bubble').textContent.length, n);
    if (stopped >= 500) throw new Error('stop did not stop');
    ok(`stop button cancelled the stream after ${stopped} chars`);
    await page.screenshot({ path: path.join(shots, `chat-${impl}.png`) });

    // CONSENT_DENIED: a second (fake) device asks to pair, SaaS user denies, then tries to approve
    const pub = crypto.generateKeyPairSync('ed25519').publicKey.export({ format: 'jwk' }).x;
    const au = await (await fetch(base + '/v2/device/authorize', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ tenant: 'acme', public_key: pub, device_name: 'intruder' }) })).json();
    await page.fill('#code', au.user_code); await page.click('#deny');
    await page.waitForFunction(() => document.getElementById('pairMsg').textContent.includes('已拒绝'));
    await page.fill('#code', au.user_code); await page.click('#approve');
    await page.waitForFunction(() => document.getElementById('pairMsg').textContent.includes('配对未获同意'));
    await page.locator('#pairMsg').scrollIntoViewIfNeeded();
    ok('CONSENT_DENIED shown: ' + (await page.textContent('#pairMsg')));

    // DEVICE_OFFLINE: stop the connector, wait for the gateway to drop it, send again
    con.kill('SIGKILL');
    await page.waitForSelector('#devices .badge.off', { timeout: 45000 });
    a = await send('还在吗？');
    if (a.state !== 'error' || !/DEVICE_OFFLINE|OUTCOME_UNKNOWN|MODEL_NOT_FOUND/.test(a.err)) throw new Error('offline: ' + JSON.stringify(a));
    ok('offline device error shown: ' + a.err);
    await page.screenshot({ path: path.join(shots, `chat-${impl}-errors.png`) });
    if (consoleErrors.length) throw new Error('browser console errors: ' + consoleErrors.join(' | '));
    results[impl] = { ok: true, checks: checks.length, seconds: Math.round((Date.now() - t0) / 1000) };
  } catch (e) {
    await page.screenshot({ path: path.join(shots, `chat-${impl}-FAILED.png`) }).catch(() => {});
    const sel = await page.evaluate(() => document.getElementById('model')?.outerHTML).catch(() => '');
    console.log(`  [${impl}] FAIL ${e.message}\n  model select: ${sel}\n--- gateway log ---\n${gw?.out.slice(-2000)}\n--- connector log ---\n${con?.out.slice(-2000)}`);
    results[impl] = { ok: false, error: e.message };
  } finally {
    con?.kill('SIGKILL'); gw?.kill('SIGKILL'); await page.close();
  }
}
await browser.close();
fm.kill('SIGKILL');
console.log(JSON.stringify(results));
process.exit(Object.values(results).every((r) => r.ok) ? 0 : 1);
