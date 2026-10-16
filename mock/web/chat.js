// LMBP mock SaaS chat page (shared by the Go, Node and Python gateways).
// No build step. All user/model text is rendered with textContent only.
'use strict';
(function () {
  const $ = (id) => document.getElementById(id);
  const SS = window.sessionStorage;
  let auth = null;            // {tenant, user, key}
  let history = [];           // OpenAI messages of the current conversation
  let models = [];            // /v1/models data
  let attachment = null;      // {dataURL, name, size, type}
  let busy = null;            // AbortController of the running request
  let pairLink = '', issuer = '';
  const initialEmpty = $('empty') ? $('empty').cloneNode(true) : null;

  const ERR = {
    UNAUTHENTICATED: '认证失败：租户 API Key 无效或已失效',
    FORBIDDEN: '无权访问该资源',
    BAD_REQUEST: '请求格式或参数错误',
    DEVICE_OFFLINE: '设备离线：请确认电脑上的 Connector 正在运行',
    DEVICE_BUSY: '设备繁忙：已达并发上限，请稍后重试',
    MODEL_NOT_FOUND: '模型不存在：你的在线设备上没有这个模型',
    UPSTREAM_ERROR: '本机模型出错',
    TIMEOUT: '任务超时',
    CANCELLED: '任务已取消',
    RATE_LIMITED: '请求过于频繁，请稍后再试',
    INTERNAL: '网关内部错误',
    OUTCOME_UNKNOWN: '设备中途断开，结果未知（可能已执行）',
    FLOW_CONTROL: '流控违规，连接已断开',
    SLOW_CONSUMER: '读取过慢，任务被网关终止',
    CAPABILITY_UNSUPPORTED: '模型或设备不支持该能力（如图片、tools）',
    PAYLOAD_TOO_LARGE: '附件过大（单个 ≤ 20 MiB，单任务 ≤ 32 MiB）',
    IDEMPOTENCY_KEY_REUSED: 'Idempotency-Key 被用于不同的请求体',
    E2EE_DECRYPT_FAILED: '设备无法解密请求',
    CONSENT_DENIED: '配对未获同意：该配对码已被拒绝，需在设备上重新发起配对',
    VERSION_UNSUPPORTED: '协议版本不支持',
    BAD_SIGNATURE: '任务签名无效',
  };
  const describe = (code, msg) => (ERR[code] || '错误') + (msg ? '（' + msg + '）' : '');

  function toast(text, bad) {
    const d = document.createElement('div');
    d.textContent = text; if (bad) d.className = 'bad';
    $('toast').appendChild(d); setTimeout(() => d.remove(), 5000);
  }

  // ---- API ----
  class ApiError extends Error { constructor(status, code, msg) { super(msg); this.status = status; this.code = code; } }
  async function parseError(res) {
    let code = 'HTTP_' + res.status, msg = res.statusText;
    try {
      const b = await res.json();
      if (b.error && typeof b.error === 'object') { code = b.error.code || code; msg = b.error.message || msg; }
      else if (b.code) { code = b.code; msg = b.detail || b.title || msg; }
      else if (typeof b.error === 'string') { code = b.error; }
    } catch { /* not JSON */ }
    return new ApiError(res.status, code, msg);
  }
  function headers(extra) {
    return Object.assign({ Authorization: 'Bearer ' + auth.key, 'X-End-User': auth.user }, extra || {});
  }
  async function api(method, path, body) {
    const res = await fetch(path, { method, headers: headers(body ? { 'Content-Type': 'application/json' } : {}), body: body ? JSON.stringify(body) : undefined });
    if (!res.ok) { const e = await parseError(res); if (e.status === 401) onUnauthorized(); throw e; }
    return res.json();
  }
  function onUnauthorized() { toast(ERR.UNAUTHENTICATED + '，请重新登录', true); logout(); }

  // ---- login ----
  function showApp(on) {
    $('login').classList.toggle('hidden', on); $('app').classList.toggle('hidden', !on); $('logout').classList.toggle('hidden', !on);
    $('who').textContent = on ? `租户 ${auth.tenant} · 用户 ${auth.user}` : '';
  }
  async function login(a) {
    auth = a;
    try { await api('GET', '/lmbp/v1/tenant/devices'); }
    catch (e) { auth = null; throw e; }
    SS.setItem('lmbp.auth', JSON.stringify(a));
    showApp(true); await loadPairing(); await refresh();
  }
  function logout() { auth = null; SS.removeItem('lmbp.auth'); showApp(false); }
  $('loginForm').addEventListener('submit', async (ev) => {
    ev.preventDefault(); $('loginErr').textContent = '';
    try { await login({ tenant: $('tenant').value.trim(), user: $('user').value.trim(), key: $('key').value }); $('key').value = ''; }
    catch (e) { $('loginErr').textContent = e.status === 401 ? 'API Key 无效' : describe(e.code, e.message); }
  });
  $('logout').onclick = logout;

  // ---- pairing ----
  async function loadPairing() {
    try {
      const res = await fetch('/.well-known/lmbp?tenant=' + encodeURIComponent(auth.tenant));
      if (!res.ok) throw await parseError(res);
      issuer = (await res.json()).issuer;
    } catch (e) { issuer = location.origin; toast('读取网关元数据失败：' + describe(e.code, e.message), true); }
    pairLink = 'lmbp://pair?v=1&gateway=' + encodeURIComponent(issuer) + '&tenant=' + encodeURIComponent(auth.tenant) + '&remote=' + encodeURIComponent(location.origin);
    $('pairLink').textContent = pairLink;
    $('qr').textContent = '';
    if (window.qrcode) {
      const q = window.qrcode(0, 'M'); q.addData(pairLink); q.make();
      const img = new Image(); img.src = q.createDataURL(4, 2); img.alt = '配对链接二维码'; $('qr').appendChild(img);
    }
  }
  const cmd = () => `localsubproxy connector -pair-link '${pairLink}' -adapter-id local -ollama-url http://127.0.0.1:11434 -models <模型名> -capabilities vision`;
  const copy = (t, what) => navigator.clipboard?.writeText(t).then(() => toast('已复制' + what), () => toast('复制失败', true));
  $('copyLink').onclick = () => copy(pairLink, '配对链接');
  $('copyCmd').onclick = () => copy(cmd(), ' Connector 命令');
  function pairMsg(text, bad) { const d = $('pairMsg'); d.className = bad ? 'msg-err' : 'msg-ok'; d.textContent = text; }
  async function decide(deny) {
    const user_code = $('code').value.trim();
    if (!user_code) return pairMsg('请输入设备上显示的配对码', true);
    const body = { user_code }; const jkt = $('jkt').value.trim(); if (jkt) body.jkt = jkt; if (deny) body.deny = true;
    $('approve').disabled = $('deny').disabled = true;
    try {
      const r = await api('POST', '/lmbp/v1/tenant/devices/approvals', body);
      pairMsg(r.status === 'approved' ? `已批准：${r.device_name || '设备'}（${r.device_id}），等待设备上线…` : '已拒绝该配对请求，设备将收到 CONSENT_DENIED');
      $('code').value = ''; $('jkt').value = ''; history_replaceParams();
      setTimeout(refresh, 1500);
    } catch (e) {
      const extra = { 404: '（配对码无效、已过期或已使用）', 409: '（jkt 与该配对请求的设备密钥不一致）', 429: '（失败次数过多，请一分钟后再试）' }[e.status] || '';
      pairMsg(describe(e.code, e.message) + extra, true);
    } finally { $('approve').disabled = $('deny').disabled = false; }
  }
  $('approve').onclick = () => decide(false);
  $('deny').onclick = () => decide(true);
  function history_replaceParams() { if (location.search) window.history.replaceState(null, '', location.pathname); }

  // ---- devices & models ----
  async function refresh() {
    if (!auth) return;
    try {
      const [d, m] = await Promise.all([api('GET', '/lmbp/v1/tenant/devices'), api('GET', '/v1/models')]);
      renderDevices(d.devices || []); renderModels(m.data || []);
    } catch (e) { if (e.status !== 401) $('pairState').textContent = '加载失败：' + describe(e.code, e.message); }
  }
  function el(tag, cls, text) { const e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function renderDevices(devs) {
    const active = devs.filter((d) => d.status === 'active'), online = active.filter((d) => d.online);
    $('devCount').textContent = String(active.length);
    const ps = $('pairState'); ps.textContent = '';
    if (!active.length) ps.append(el('span', 'badge off', '未配对'), document.createTextNode(' 还没有已配对的设备，请先在下方配对。'));
    else ps.append(el('span', 'badge ' + (online.length ? 'on' : 'off'), online.length ? '已连接' : '离线'),
      document.createTextNode(` 已配对 ${active.length} 台，在线 ${online.length} 台`));
    const box = $('devices'); box.textContent = '';
    if (!devs.length) box.append(el('div', 'meta', '暂无设备'));
    for (const d of devs) {
      const row = el('div', 'dev');
      const h = el('div', 'dev-head');
      h.append(el('span', 'nm', d.name || '(未命名设备)'),
        el('span', 'badge ' + (d.status !== 'active' ? 'rv' : d.online ? 'on' : 'off'), d.status !== 'active' ? '已吊销' : d.online ? '在线' : '离线'));
      row.append(h, el('div', 'mono meta', d.device_id));
      const ms = el('div', 'models');
      for (const m of d.models || []) {
        const caps = (d.model_capabilities || {})[m] || [];
        const dn = (d.model_names || {})[m];
        const b = el('span', 'badge', (dn || m) + (caps.length ? ' · ' + caps.join('/') : '')); b.title = m;
        ms.append(b);
      }
      row.append(ms);
      if (d.status === 'active') {
        const b = el('button', 'danger', '吊销'); b.style.marginTop = '8px';
        b.onclick = async () => {
          if (!confirm(`吊销设备 ${d.name}？吊销后设备需要重新配对。`)) return;
          try { await api('POST', '/lmbp/v1/tenant/devices/' + encodeURIComponent(d.device_id) + '/revoke'); toast('已吊销'); refresh(); }
          catch (e) { toast(describe(e.code, e.message), true); }
        };
        row.append(b);
      }
      box.append(row);
    }
  }
  // modelLabels shows the device-advertised scheme name (display_name) instead of
  // the long "device/adapter/model" id; the device is added only when several
  // devices serve models, and the id when labels would still collide.
  function modelLabels(list) {
    const devs = new Set(list.map((m) => m.id.split('/')[0]));
    const base = (m) => {
      const n = m.display_name || m.id.split('/').slice(1).join('/') || m.id;
      return devs.size > 1 && m.owned_by ? n + '  @' + m.owned_by : n;
    };
    const count = new Map();
    for (const m of list) count.set(base(m), (count.get(base(m)) || 0) + 1);
    return new Map(list.map((m) => [m.id, count.get(base(m)) > 1 ? base(m) + '  (' + m.id + ')' : base(m)]));
  }
  function renderModels(list) {
    models = list;
    const sel = $('model'), prev = sel.value; sel.textContent = '';
    if (!list.length) { const o = el('option', '', '（没有在线模型）'); o.value = ''; sel.append(o); }
    const labels = modelLabels(list);
    for (const m of list) { const o = el('option', '', labels.get(m.id)); o.value = m.id; o.title = m.id; sel.append(o); }
    if (list.some((m) => m.id === prev)) sel.value = prev;
    else if (prev && busy === null && history.length) {
      // Keep the conversation's model selectable so the user sees DEVICE_OFFLINE instead of a silent switch.
      const o = el('option', '', prev + '（已离线）'); o.value = prev; sel.prepend(o); sel.value = prev;
    }
    showCaps();
  }
  function showCaps() {
    const m = models.find((x) => x.id === $('model').value);
    $('modelCaps').textContent = m ? '能力：' + ((m.capabilities || []).join(', ') || '文本') : '';
  }
  $('model').onchange = showCaps;
  $('refresh').onclick = refresh;
  setInterval(refresh, 4000);

  // ---- attachments ----
  $('pick').onclick = () => $('file').click();
  $('file').onchange = () => {
    const f = $('file').files[0]; $('file').value = '';
    if (!f) return;
    if (f.size > 20 << 20) return toast(ERR.PAYLOAD_TOO_LARGE, true);
    const r = new FileReader();
    r.onload = () => { attachment = { dataURL: r.result, name: f.name, size: f.size }; renderAtt(); };
    r.readAsDataURL(f);
  };
  function renderAtt() {
    const a = $('attInfo'); a.textContent = '';
    if (!attachment) return;
    const img = new Image(); img.src = attachment.dataURL; img.alt = attachment.name;
    const x = el('button', 'ghost', '移除'); x.onclick = () => { attachment = null; renderAtt(); };
    a.append(img, document.createTextNode(` ${attachment.name}（${(attachment.size / 1024).toFixed(1)} KiB）`), x);
  }

  // ---- starter prompts ----
  function bindStarters(root) {
    if (!root) return;
    for (const card of root.querySelectorAll('.starter-card')) {
      card.onclick = () => {
        const p = card.getAttribute('data-prompt') || '';
        if (!p) return;
        $('input').value = p;
        $('input').focus();
        if ($('model').value) send();
      };
    }
  }
  bindStarters($('empty'));

  // ---- chat ----
  function turn(role, text, imgURL) {
    $('empty')?.remove();
    const t = el('div', 'turn ' + role);
    const head = el('div', 'turn-header');
    const avatar = el('span', 'turn-avatar', role === 'user' ? (auth?.user || 'U').slice(0, 1).toUpperCase() : 'AI');
    const who = el('div', 'who', role === 'user' ? auth.user : ($('model').value || '本机模型'));
    head.append(avatar, who);
    const b = el('div', 'bubble', text);
    if (imgURL) { const i = new Image(); i.src = imgURL; i.alt = '附件'; b.append(i); }
    const meta = el('div', 'meta');
    t.append(head, b, meta); $('chat').append(t); scroll();
    return { t, b, meta };
  }
  const scroll = () => { const c = $('chat'); c.scrollTop = c.scrollHeight; };
  function showErr(t, code, msg) {
    const e = el('div', 'err'); e.append(el('b', '', code), document.createTextNode(' ' + describe(code, msg)));
    t.append(e); scroll();
  }
  function setBusy(ctrl) {
    busy = ctrl; $('send').classList.toggle('hidden', !!ctrl); $('stop').classList.toggle('hidden', !ctrl); $('input').disabled = !!ctrl;
  }
  $('newChat').onclick = () => {
    if (busy) busy.abort();
    history = [];
    $('chat').textContent = '';
    if (initialEmpty) {
      const node = initialEmpty.cloneNode(true);
      bindStarters(node);
      $('chat').append(node);
    } else {
      const e = el('div', 'empty', '新对话已开始。'); e.id = 'empty'; $('chat').append(e);
    }
  };
  $('stop').onclick = () => busy && busy.abort();
  $('input').addEventListener('keydown', (e) => { if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); } });
  $('send').onclick = send;

  async function send() {
    const text = $('input').value.trim(), model = $('model').value;
    if (busy || (!text && !attachment)) return;
    if (!model) return toast('没有可用的模型：请先配对设备并确认它在线', true);
    const content = attachment ? [{ type: 'text', text: text || '请描述这张图片。' }, { type: 'image_url', image_url: { url: attachment.dataURL } }] : text;
    const userMsg = { role: 'user', content };
    turn('user', text || '请描述这张图片。', attachment && attachment.dataURL);
    $('input').value = ''; attachment = null; renderAtt();
    const msgs = history.concat([userMsg]);
    const a = turn('assistant', ''); a.b.classList.add('cursor');
    const ctrl = new AbortController(); setBusy(ctrl);
    const t0 = performance.now(); let tFirst = 0, out = '', usage = null, finish = '', taskID = '', failed = null;
    try {
      const res = await fetch('/v1/chat/completions', {
        method: 'POST', signal: ctrl.signal,
        headers: headers({ 'Content-Type': 'application/json' }), // no Idempotency-Key: 停止 must cancel at once (keyed tasks get a 30 s re-attach grace)
        body: JSON.stringify({ model, stream: true, messages: msgs }),
      });
      taskID = res.headers.get('X-Task-Id') || '';
      if (!res.ok) { const e = await parseError(res); failed = e; if (e.status === 401) onUnauthorized(); }
      else {
        const rd = res.body.getReader(), dec = new TextDecoder(); let buf = '';
        for (;;) {
          const { value, done } = await rd.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          let i;
          while ((i = buf.indexOf('\n\n')) >= 0) {
            const ev = buf.slice(0, i); buf = buf.slice(i + 2);
            const data = ev.split('\n').filter((l) => l.startsWith('data:')).map((l) => l.slice(5).trimStart()).join('\n');
            if (!data || data === '[DONE]') continue;
            let j; try { j = JSON.parse(data); } catch { continue; }
            if (j.error) { failed = new ApiError(0, j.error.code, j.error.message); continue; }
            const ch = (j.choices || [])[0] || {};
            const piece = ch.delta && ch.delta.content;
            if (piece) { if (!tFirst) tFirst = performance.now(); out += piece; a.b.textContent = out; scroll(); }
            if (ch.delta && ch.delta.tool_calls) { out += '\n[tool_call] ' + JSON.stringify(ch.delta.tool_calls); a.b.textContent = out; }
            if (ch.finish_reason) finish = ch.finish_reason;
            if (j.usage) usage = j.usage;
          }
        }
      }
    } catch (e) {
      if (e.name === 'AbortError') failed = { code: 'CANCELLED', message: '已手动停止', stopped: true };
      else failed = new ApiError(0, 'NETWORK', '网络错误：' + e.message);
    }
    a.b.classList.remove('cursor');
    const total = performance.now() - t0;
    const parts = [];
    if (tFirst) parts.push(`首字 ${Math.round(tFirst - t0)} ms`);
    parts.push(`总耗时 ${Math.round(total)} ms`);
    if (usage) {
      parts.push(`tokens 输入 ${usage.prompt_tokens ?? '-'} / 输出 ${usage.completion_tokens ?? '-'}`);
      if (usage.completion_tokens && tFirst && total > tFirst - t0) parts.push(`${(usage.completion_tokens / ((total - (tFirst - t0)) / 1000 || 1)).toFixed(1)} tok/s`);
    }
    if (finish) parts.push('finish: ' + finish);
    if (taskID) parts.push('task ' + taskID);
    a.meta.textContent = parts.join(' · ');
    a.t.dataset.state = failed ? (failed.stopped ? 'stopped' : 'error') : 'done';
    if (failed) {
      if (!out) a.b.textContent = failed.stopped ? '（已停止）' : '（无输出）';
      showErr(a.t, failed.code, failed.message);
    }
    // Keep the turn in history if anything was produced (a stopped partial answer counts).
    if (out) history = msgs.concat([{ role: 'assistant', content: out }]);
    else if (!failed) history = msgs.concat([{ role: 'assistant', content: '' }]);
    setBusy(null); $('input').focus();
  }

  // ---- boot ----
  const qs = new URLSearchParams(location.search);
  if (qs.get('user_code')) $('code').value = qs.get('user_code');
  if (qs.get('jkt')) $('jkt').value = qs.get('jkt');
  if (qs.get('tenant')) $('tenant').value = qs.get('tenant');
  const saved = SS.getItem('lmbp.auth');
  if (saved) login(JSON.parse(saved)).catch(() => logout());
})();
