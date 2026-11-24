// A2A 薄壳：把沙箱里的 DSH 包成 A2A（kagent / substrate 需要 8080 上说 A2A）。
// 只做协议翻译，不碰模型、不碰工具：DSH 自己跑 session，这里转发 prompt 并等 turn/end。
const http = require('http');
const fs = require('fs');
const crypto = require('crypto');

const DSH = process.env.DSH_ENDPOINT || 'http://127.0.0.1:18790';
const PORT = Number(process.env.A2A_PORT || 80);
// 单轮上限：必须小于网关的 300s，否则先被网关掐断
const TURN_TIMEOUT_MS = Number(process.env.TURN_TIMEOUT_MS || 240000);

const sessionByContext = new Map(); // contextId -> dsh sessionId
let sreToken = null; // 只在内存里，绝不落到 prompt 或磁盘
const DCE_ORIGIN = process.env.DCE_ORIGIN || 'http://10.31.223.20:31284';
const SRE_GATEWAY = process.env.SRE_GATEWAY || 'http://sre-gateway.watsons-agent-poc.svc.cluster.local:3088';

// 只暴露 SRE 的两个固定接口，不做任意转发
http.createServer(async (req, res) => {
  // /dce/* → 用内存里的用户 JWT 调 DCE OpenAPI（沙箱里只有代理，没有 JWT 文件）
  if (req.url.startsWith('/dce/')) {
    const target = req.url.slice(4); // 保留 /apis/... 或 /kapis/...
    if (!/^\/(apis|kapis)\//.test(target)) { res.writeHead(400); return res.end('only /apis or /kapis'); }
    if (!sreToken) { res.writeHead(401); return res.end('no user token for this session'); }
    try {
      const chunks = []; for await (const c of req) chunks.push(c);
      const r = await fetch(DCE_ORIGIN + target, {
        method: req.method,
        headers: {
          authorization: 'Bearer ' + sreToken,
          ...(req.headers['content-type'] ? { 'content-type': req.headers['content-type'] } : {}),
          accept: 'application/json',
        },
        body: ['GET', 'HEAD'].includes(req.method) ? undefined : Buffer.concat(chunks),
        signal: AbortSignal.timeout(30000),
      });
      const text = await r.text();
      res.writeHead(r.status, { 'content-type': 'application/json' });
      res.end(text);
    } catch (e) {
      res.writeHead(502, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ error: String(e.message) }));
    }
    return;
  }

  const only = req.url === '/sre/report' || req.url === '/sre/inspect';
  if (!only) { res.writeHead(404); return res.end('not found'); }
  if (!sreToken) { res.writeHead(401); return res.end('no sre token for this session'); }
  const target = req.url === '/sre/report' ? '/sre/poc/reports/latest' : '/sre/poc/inspect';
  const method = req.url === '/sre/report' ? 'GET' : 'POST';
  try {
    const r = await fetch(SRE_GATEWAY + target, {
      method,
      headers: { authorization: 'Bearer ' + sreToken, 'content-type': 'application/json' },
      body: method === 'POST' ? '{}' : undefined,
      signal: AbortSignal.timeout(120000),
    });
    const body = await r.text();
    res.writeHead(r.status, { 'content-type': 'application/json' });
    res.end(body);
  } catch (e) {
    res.writeHead(502, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ error: String(e.message) }));
  }
}).listen(8099, '127.0.0.1', () => log('sre proxy on 127.0.0.1:8099'));

function log(...a) { console.log(new Date().toISOString(), ...a); }

async function rpc(method, payload) {
  const r = await fetch(DSH + '/api/' + method, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ type: 'client-request', rpcId: crypto.randomUUID(), method, payload }),
    signal: AbortSignal.timeout(30000),
  });
  const j = await r.json();
  if (!r.ok || !j.result?.ok) throw new Error('dsh rpc failed: ' + method);
  return j.result.value;
}

async function dshSession(contextId) {
  const known = sessionByContext.get(contextId);
  if (known) return known;
  const v = await rpc('session.create', { cwd: '/home/agent/work' });
  sessionByContext.set(contextId, v.sessionId);
  log('session.create', contextId, '->', v.sessionId);
  return v.sessionId;
}

async function ask(contextId, text, onText) {
  const sid = await dshSession(contextId);
  const before = await rpc('session.history', { sessionId: sid, maxMessages: 2 });
  const baseline = Math.max(-1, ...before.events.map((x) => x.event?.seq ?? -1));
  await rpc('session.prompt', { sessionId: sid, mode: 'queue', content: [{ type: 'text', text }] });

  const deadline = Date.now() + TURN_TIMEOUT_MS;
  while (Date.now() < deadline) {
    const data = await rpc('session.history', { sessionId: sid, maxMessages: 2 });
    const events = (data.events || []).map((x) => x.event || {});
    if (onText) {
      // 半成品文本：最后一条 assistant 消息的 text 块
      // 只看本轮（seq > baseline）的最后一条 assistant 消息，避免把上一轮的答案当增量
      let partial = '';
      let seq = -1;
      for (let i = events.length - 1; i >= 0; i--) {
        const ev = events[i];
        if (ev.type === 'assistant/message' && ev.seq > baseline) {
          partial = ((ev.data?.message?.content) || [])
            .filter((x) => x.type === 'text').map((x) => x.text).join('');
          seq = ev.seq;
          break;
        }
      }
      if (partial) onText(partial, seq);
    }
    const end = events.find((e) => e.type === 'turn/end' && e.seq > baseline);
    if (end) {
      const msg = events.filter((e) => e.type === 'assistant/message' && e.seq > baseline).at(-1);
      const answer = ((msg?.data?.message?.content) || [])
        .filter((x) => x.type === 'text').map((x) => x.text).join('');
      if (!answer) {
        // 没有文本时把最后几个事件抖出来，方便定位（模型报错/网络被拦/凭据缺失）。
        const tail = events.slice(-4).map((e) => e.type + ':' + JSON.stringify(e.data || {}).slice(0, 200)).join(' | ');
        return { answer: '（本轮没有文本输出）事件尾巴: ' + tail, sid, events };
      }
      return { answer, sid, events };
    }
    await new Promise((r) => setTimeout(r, 300));
  }
  const steps = Math.max(0, (events || []).filter((e) => e.type === 'tool/call').length);
  const err = new Error(`本轮超过 ${Math.round(TURN_TIMEOUT_MS / 1000)} 秒已中止（已完成 ${steps} 步工具调用）`);
  err.userFacing = true;
  throw err;
}

function send(res, code, body) {
  const raw = JSON.stringify(body);
  res.writeHead(code, { 'content-type': 'application/json' });
  res.end(raw);
}

function a2aResult(id, contextId, text) {
  return {
    jsonrpc: '2.0', id,
    result: {
      kind: 'task',
      id: crypto.randomUUID(),
      contextId,
      status: { state: 'completed', timestamp: new Date().toISOString() },
      artifacts: [{ artifactId: crypto.randomUUID(), parts: [{ kind: 'text', text }] }],
      history: [],
    },
  };
}

const server = http.createServer(async (req, res) => {
  if (req.method === 'GET' && req.url.startsWith('/.well-known/agent-card.json')) {
    return send(res, 200, {
      name: 'dsh_sandbox', description: 'DSH in Substrate sandbox', version: '0.1.0',
      defaultInputModes: ['text'], defaultOutputModes: ['text'],
      capabilities: { streaming: false }, skills: [],
      supportedInterfaces: [{ url: '/', protocolBinding: 'JSONRPC' }],
    });
  }
  if (req.method === 'GET' && req.url === '/healthz') { res.writeHead(200); return res.end('ok\n'); }
  if (req.method !== 'POST') { res.writeHead(405); return res.end(); }

  let raw = '';
  for await (const c of req) { raw += c; if (raw.length > 1 << 20) { res.writeHead(413); return res.end(); } }

  let body;
  try { body = JSON.parse(raw); } catch { return send(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'parse error' } }); }

  const { id, method, params } = body || {};
  if (method !== 'message/send' && method !== 'message/stream') {
    return send(res, 200, { jsonrpc: '2.0', id, error: { code: -32601, message: 'method not supported: ' + method } });
  }
  const contextId = params?.contextId || params?.message?.contextId || 'default';
  const parts = params?.message?.parts || [];
  const srePart = parts.find((p) => p.kind === 'data' && p.data && p.data.sreToken);
  if (srePart) {
    sreToken = srePart.data.sreToken;
    try { fs.mkdirSync('/run/dsh', { recursive: true, mode: 0o700 }); fs.writeFileSync('/run/dsh/token', sreToken, { mode: 0o600 }); } catch (e) { log('token file write failed', e.message); }
    log('sre token received for', contextId);
  }
  const kcPart = parts.find((p) => p.kind === 'data' && p.data && p.data.kubeconfig);
  if (kcPart) {
    try {
      fs.mkdirSync('/home/agent/.kube', { recursive: true, mode: 0o700 });
      fs.writeFileSync('/home/agent/.kube/config', kcPart.data.kubeconfig, { mode: 0o600 });
      log('kubeconfig installed for', contextId);
    } catch (e) { log('kubeconfig write failed', e.message); }
  }
  const text = parts.filter((p) => p.kind === 'text').map((p) => p.text).join('\n');
  if (!text) return send(res, 200, { jsonrpc: '2.0', id, error: { code: -32602, message: 'empty message' } });

  log('turn start', contextId, JSON.stringify(text).slice(0, 120));

  if (method === 'message/stream') {
    res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache', connection: 'keep-alive' });
    let sent = '';
    let sentSeq = -1;
    const send = (obj) => res.write('data: ' + JSON.stringify(obj) + '\n\n');
    send({ jsonrpc: '2.0', id, result: { kind: 'task', contextId, status: { state: 'working' }, artifacts: [] } });
    try {
      const { answer } = await ask(contextId, text, (partial, seq) => {
        if (seq !== sentSeq) { sentSeq = seq; sent = ''; }   // 换了一步 = 新的一段文本
        if (partial.length > sent.length && partial.startsWith(sent)) {
          const delta = partial.slice(sent.length);
          sent = partial;
          send({ jsonrpc: '2.0', id, result: { kind: 'task', contextId, status: { state: 'working' },
            artifacts: [{ parts: [{ kind: 'text', text: delta }] }] } });
        }
      });
      if (answer.length > sent.length) {
        send({ jsonrpc: '2.0', id, result: { kind: 'task', contextId, status: { state: 'working' },
          artifacts: [{ parts: [{ kind: 'text', text: answer.slice(sent.length) }] }] } });
      }
      send({ jsonrpc: '2.0', id, result: { kind: 'task', contextId, status: { state: 'completed', timestamp: new Date().toISOString() }, artifacts: [] } });
    } catch (e) {
      send({ jsonrpc: '2.0', id, error: { code: -32603, message: e.message } });
    }
    return res.end();
  }
  try {
    const { answer, sid } = await ask(contextId, text);
    log('turn done', contextId, 'dsh=', sid, JSON.stringify(answer).slice(0, 120));
    return send(res, 200, a2aResult(id, contextId, answer));
  } catch (e) {
    // 超时/异常时把最后几个事件带回去，否则在 actor 里完全看不到 DSH 在干什么
    let tail = '';
    try {
      const sid = sessionByContext.get(contextId);
      if (sid) {
        const h = await rpc('session.history', { sessionId: sid, maxMessages: 2 });
        tail = (h.events || []).slice(-6).map((x) => (x.event?.type || '?') + ':' + JSON.stringify(x.event?.data || {}).slice(0, 160)).join(' | ');
      }
    } catch {}
    log('turn error', contextId, e.message, tail);
    return send(res, 200, { jsonrpc: '2.0', id, error: { code: -32603, message: e.message + ' || ' + tail } });
  }
});

server.listen(PORT, '0.0.0.0', () => log('a2a shim listening on', PORT, '-> dsh', DSH));
