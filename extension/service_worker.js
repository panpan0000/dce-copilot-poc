// The two values below are deliberately constants.  The content script cannot
// choose a destination, path, or backend on behalf of the page.
// 由构建脚本生成的 config.js 提供；缺失时回落到默认值（本地开发用）
const CFG = (typeof globalThis !== 'undefined' && globalThis.DCE_AGENT_CONFIG) || {};
let DCE_ORIGIN = CFG.dceOrigin || 'http://dce.example.com';
let GATEWAY_BASE = CFG.gatewayBase || 'http://agent.example.com';
let configReady;
// 配置优先级：chrome.storage（设置页） > config.js（构建期） > 默认值
function ensureConfig() {
  if (!configReady) {
    configReady = chrome.storage.local.get(['dceOrigin', 'gatewayBase']).then((c) => {
      if (c.dceOrigin) { DCE_ORIGIN = c.dceOrigin.replace(/\/$/, ''); CFG.dceOrigin = DCE_ORIGIN; }
      if (c.gatewayBase) { GATEWAY_BASE = c.gatewayBase.replace(/\/$/, ''); CFG.gatewayBase = GATEWAY_BASE; }
    }).catch(() => {});
  }
  return configReady;
}

const dceUserUrl = () => `${DCE_ORIGIN}/apis/ghippo.io/v1alpha1/current-user`;

// DCE API 端点（已按 docs.daocloud.io/openapi 的 kpanda / insight / baize 规格核对）。
const DCE_API = Object.freeze({
  cluster: 'kpanda-global-cluster',
  workspaces: '/apis/baize.io/v1alpha1/workspaces',                       // baize(白泽)=AILab
  notebooks: (ws) => [
    `/apis/baize.io/v1alpha1/workspaces/${ws}/notebooks`,
    `/apis/baize.io/v1alpha1/workspaces/${ws}/clusters/${DCE_API.cluster}/namespaces/${ws}/notebooks`,
  ],
  pods: (ns) => `/apis/kpanda.io/v1alpha1/clusters/${DCE_API.cluster}/namespaces/${ns}/pods?page=1&pageSize=50`,
  podLog: (ns, pod, lines = 50) =>
    `/apis/kpanda.io/v1alpha1/clusters/${DCE_API.cluster}/namespaces/${ns}/pods/${pod}/log?tailLines=${lines}`,
  podMetrics: (ns, pod, seconds = 900) => {
    const end = Math.floor(Date.now() / 1000);
    return `/apis/insight.io/v1alpha1/clusters/${DCE_API.cluster}/namespaces/${ns}/pods/${pod}/metrics`
      + `?start=${end - seconds}&end=${end}&step=60&queryList=pod_cpu_usage`;
  },
});

const GATEWAY_PATHS = Object.freeze({
  // 2026-11-19: 指向 substrate 骨架的兼容路径（老的 /poc 路由指向 0 副本的常驻 Pod）
  whoami: '/sandbox/poc/whoami',
  chat: '/sandbox/poc/chat',
  chatStream: '/sandbox/poc/chat/stream',
});

// A DCE login redirect can finish navigation before the page has written its
// token to localStorage. Retry only within this request; the token is never
// cached between requests.
const TOKEN_READ_ATTEMPTS = 6;
const TOKEN_READ_DELAY_MS = 300;

function wait(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function getPageOrigin(sender) {
  const pageUrl = sender?.tab?.url || sender?.url;
  if (typeof pageUrl !== 'string') {
    throw new Error('请求缺少页面来源');
  }

  let origin;
  try {
    origin = new URL(pageUrl).origin;
  } catch {
    throw new Error('页面来源无效');
  }

  // 允许两个来源：配置的 DCE 地址（options/config.js），或已配置网关所在的页面。
  // 真正的安全边界在网关与 adapter（都校验 JWT），这里只防"任意站点借用扩展"。
  // 没配置过（还是模板占位值）时，直接采用当前页面来源，避免装完必须先去填选项
  if (!DCE_ORIGIN || DCE_ORIGIN.includes('example.com')) {
    DCE_ORIGIN = origin;
    CFG.dceOrigin = origin;
  }
  const allowed = new Set([DCE_ORIGIN, CFG.extensionOrigin].filter(Boolean));
  if (!allowed.has(origin)) {
    throw new Error(`页面来源不是允许的 DCE 地址（当前 ${origin}，已配置 ${[...allowed].join(', ') || '无'}，请在扩展选项里填写）`);
  }

  // Chrome provides sender.origin for content-script messages in current
  // versions.  When present, check it as a second signal; do not trust it
  // instead of tab.url because the URL check is the primary binding.
  if (sender?.origin && sender.origin !== DCE_ORIGIN) {
    throw new Error('消息 origin 不匹配');
  }

  if (!Number.isInteger(sender?.tab?.id) || sender.tab.id < 0) {
    throw new Error('请求没有有效的 DCE 标签页');
  }

  return sender.tab.id;
}

async function readDceToken(tabId) {
  let lastError;
  for (let attempt = 0; attempt < TOKEN_READ_ATTEMPTS; attempt += 1) {
    try {
      const results = await chrome.scripting.executeScript({
        target: { tabId },
        world: 'MAIN',
        func: () => window.localStorage.getItem('ANAKIN_TOKEN'),
      });

      const token = results?.[0]?.result;
      if (typeof token === 'string' && token.length > 0) {
        // Do not store, log, display, or put this value in a URL. It exists
        // only for the duration of the one request below.
        return token;
      }
      lastError = new Error('当前 DCE 页面没有登录 token');
    } catch (error) {
      lastError = error instanceof Error ? error : new Error('读取 DCE token 失败');
    }

    if (attempt < TOKEN_READ_ATTEMPTS - 1) {
      await wait(TOKEN_READ_DELAY_MS);
    }
  }

  throw lastError || new Error('当前 DCE 页面没有登录 token');
}

async function currentUser(token) {
  let response;
  try {
    response = await fetch(dceUserUrl(), {
      method: 'GET',
      headers: { Authorization: `Bearer ${token}`, Accept: 'application/json' },
      credentials: 'omit',
      cache: 'no-store',
      referrerPolicy: 'no-referrer',
      signal: AbortSignal.timeout(10000),
    });
  } catch {
    throw new Error('无法从 DCE 读取当前用户');
  }
  if (!response.ok) throw new Error(`DCE 身份验证失败（HTTP ${response.status}）`);
  const user = (await response.json())?.username;
  if (typeof user !== 'string' || !/^[A-Za-z0-9_.@-]{1,128}$/.test(user)) {
    throw new Error('DCE 返回的用户名无法用于 POC 路由');
  }
  return user;
}

async function callGateway(tabId, path, init, timeoutMs) {
  const token = await readDceToken(tabId);
  // This header only selects a route. The selected agent must independently
  // verify the same DCE JWT belongs to the user whose Pod it serves.
  const username = await currentUser(token);
  const headers = new Headers(init?.headers || {});
  headers.set('Authorization', `Bearer ${token}`);
  headers.set('Accept', 'application/json');
  headers.set('X-POC-User', username);

  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const response = await fetch(`${GATEWAY_BASE}${path}`, {
      ...init,
      headers,
      credentials: 'omit',
      cache: 'no-store',
      referrerPolicy: 'no-referrer',
      signal: controller.signal,
    });

    const contentType = response.headers.get('content-type') || '';
    const body = contentType.includes('application/json')
      ? await response.json()
      : await response.text();

    return { ok: response.ok, status: response.status, body };
  } catch (error) {
    if (error?.name === 'AbortError') {
      throw new Error('Agent Gateway 请求超时');
    }
    throw new Error('Agent Gateway 不可达');
  } finally {
    clearTimeout(timeout);
  }
}

// 以当前用户 JWT 调 DCE。path 必须以 /apis/ 或 /kapis/ 开头，避免被页面利用。
async function dceCall(token, path) {
  if (!/^\/(apis|kapis)\//.test(path)) throw new Error('非法 DCE 路径');
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 20000);
  try {
    const r = await fetch(DCE_ORIGIN + path, {
      headers: { Authorization: `Bearer ${token}`, Accept: 'application/json' },
      credentials: 'omit', cache: 'no-store', signal: controller.signal,
    });
    const text = await r.text();
    let body = null;
    try { body = JSON.parse(text); } catch { body = text.slice(0, 400); }
    return { ok: r.ok, status: r.status, body };
  } finally { clearTimeout(timer); }
}

// 依次探测候选路径，返回第一个非 404 的结果（含命中的路径）
async function probePaths(token, paths) {
  const tried = [];
  for (const p of paths) {
    try {
      const r = await dceCall(token, p);
      tried.push({ path: p, status: r.status });
      if (r.status !== 404) return { hit: p, ...r, tried };
    } catch (e) { tried.push({ path: p, error: String(e.message).slice(0, 80) }); }
  }
  return { hit: null, status: 404, body: null, tried };
}

async function handleQuickAction(message, sender) {
  const tabId = getPageOrigin(sender);
  const token = await readDceToken(tabId);
  const ns = await currentUser(token);            // 本次环境里命名空间与用户名同名

  if (message.type === 'notebooks') {
    const ws = await dceCall(token, DCE_API.workspaces);
    const items = ws.body?.items || [];
    const first = items[0] || {};
    const wsId = first.workspaceId ?? first.id;
    const alias = first.alias || first.name || '';
    if (!wsId) return { ok: ws.ok, status: ws.status, body: { workspaces: items, notebooks: [], note: '当前账号没有可见的 AILab 工作空间' } };
    let nb = { ok: false, status: 0, body: null };
    const tried = [];
    for (const path of DCE_API.notebooks(wsId)) {
      nb = await dceCall(token, path);
      tried.push({ path, status: nb.status });
      if (nb.status !== 404) break;
    }
    return {
      ok: nb.ok, status: nb.status,
      body: {
        workspace: { id: wsId, alias },
        workspaces: items.length,
        notebooks: nb.body?.items || nb.body?.data || [],
        tried,
        note: nb.ok ? '' : `notebook 接口返回 HTTP ${nb.status}：当前环境可能未开通 AILab notebook API（工作空间本身可见）`,
      },
    };
  }

  if (message.type === 'metrics') {
    const pods = await dceCall(token, DCE_API.pods(ns));
    const items = pods.body?.items || [];
    const pod = items.find((i) => i.metadata?.name)?.metadata?.name;
    if (!pod) return { ok: false, status: pods.status, body: { namespace: ns, pods: items.length, note: '该命名空间下没有可见的 Pod' } };
    const m = await dceCall(token, DCE_API.podMetrics(ns, pod));
    const series = m.body?.metrics?.[0]?.data?.matrix || [];
    const values = (series[0]?.values || []).slice(-12).map((v) => ({ t: Number(v.timestamp), v: Number(Number(v.value).toFixed(4)) }));
    return {
      ok: m.ok, status: m.status,
      body: { namespace: ns, pod, query: 'pod_cpu_usage', series: series.length, values, note: m.ok ? '' : `指标接口 HTTP ${m.status}` },
    };
  }

  // rca：pod 状态 + 日志 + 指标
  const pods = await dceCall(token, DCE_API.pods(ns));
  const items = pods.body?.items || [];
  const bad = items.filter((p) => {
    const cs = p.status?.containerStatuses || [];
    return p.status?.phase !== 'Running' || cs.some((c) => !c.ready);
  }).map((p) => ({
    name: p.metadata?.name,
    phase: p.status?.phase,
    reason: (p.status?.containerStatuses || [])[0]?.state?.waiting?.reason || '',
    restarts: (p.status?.containerStatuses || [])[0]?.restartCount || 0,
  }));
  const target = (bad[0] || items[0])?.metadata?.name;
  const log = target ? await dceCall(token, DCE_API.podLog(ns, target)) : { ok: false, status: 0, body: '没有可见 Pod' };
  const metric = target ? await dceCall(token, DCE_API.podMetrics(ns, target)) : { ok: false, status: 0, body: null };
  const series = metric.body?.metrics?.[0]?.data?.matrix || [];
  const lastVals = (series[0]?.values || []).slice(-6).map((v) => Number(Number(v.value).toFixed(4)));
  return {
    ok: pods.ok, status: 200,
    body: {
      cluster: DCE_API.cluster, namespace: ns, total: items.length,
      unhealthy: bad, examined: target || null,
      logs: {
        ok: log.ok, status: log.status,
        text: typeof log.body === 'string' ? log.body.slice(-1500) : JSON.stringify(log.body).slice(0, 400),
      },
      metrics: { ok: metric.ok, status: metric.status, query: 'pod_cpu_usage', last6: lastVals },
    },
  };
}

function validateSessionId(value) {
  if (value === undefined || value === null || value === '') return undefined;
  if (typeof value !== 'string' || !/^[A-Za-z0-9_-]{1,128}$/.test(value)) {
    throw new Error('session_id 格式无效');
  }
  return value;
}

async function handleMessage(message, sender) {
  const tabId = getPageOrigin(sender);

  if (message?.type === 'whoami') {
    return callGateway(tabId, GATEWAY_PATHS.whoami, {
      method: 'GET',
    }, 15000);
  }

  if (['notebooks', 'metrics', 'rca'].includes(message?.type)) {
    return handleQuickAction(message, sender);
  }

  if (message?.type === 'chat') {
    if (typeof message.message !== 'string' || message.message.trim().length === 0) {
      throw new Error('消息不能为空');
    }
    if (message.message.length > 2000) {
      throw new Error('消息长度不能超过 2000 个字符');
    }

    const sessionId = validateSessionId(message.sessionId);
    // conversationId 决定后端会话（"新会话"会换一个），只允许安全字符。
    const conversationId = validateSessionId(message.conversationId);
    const payload = {
      message: message.message,
      ...(sessionId ? { session_id: sessionId } : {}),
      ...(conversationId ? { conversation_id: conversationId } : {}),
    };

    return callGateway(tabId, GATEWAY_PATHS.chat, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }, 240000);
  }

  throw new Error('未知消息类型');
}

// 流式：内容脚本用长连接端口，这里读 SSE 再把增量推回去
chrome.runtime.onConnect.addListener((port) => {
  if (port.name !== 'dsh-stream') return;
  port.onMessage.addListener(async (msg) => {
    await ensureConfig();
    // 心跳：内容脚本每 12s 发一次，既保活后台，也确认端口还通
    if (msg?.type === 'ping') {
      try { port.postMessage({ type: 'pong' }); } catch {}
      return;
    }
    if (msg?.type !== 'chat-stream') return;
    const tabId = senderTabId(port);
    try {
      const token = await readDceToken(tabId);
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), 240000);
      const resp = await fetch(GATEWAY_BASE + GATEWAY_PATHS.chatStream, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
        body: JSON.stringify({ message: msg.message, conversation_id: msg.conversationId }),
        signal: controller.signal,
      });
      if (!resp.ok || !resp.body) throw new Error(`网关返回 HTTP ${resp.status}`);
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      let finished = false;
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const block = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          for (const line of block.split('\n')) {
            if (!line.startsWith('data:')) continue;
            let ev;
            try { ev = JSON.parse(line.slice(5).trim()); } catch { continue; }
            if (ev.delta) port.postMessage({ type: 'delta', text: ev.delta });
            if (ev.error) { port.postMessage({ type: 'error', message: typeof ev.error === 'string' ? ev.error : JSON.stringify(ev.error) }); finished = true; }
            if (ev.done && !finished) { port.postMessage({ type: 'done', sessionId: ev.session_id }); finished = true; }
          }
        }
        if (finished) break;
      }
      clearTimeout(timer);
      if (!finished) port.postMessage({ type: 'done' });
    } catch (e) {
      port.postMessage({ type: 'error', message: e?.message || String(e) });
    }
  });
});

function senderTabId(port) {
  return port?.sender?.tab?.id;
}

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  ensureConfig();
  handleMessage(message, sender)
    .then(sendResponse)
    .catch((error) => sendResponse({
      ok: false,
      error: error instanceof Error ? error.message : '请求失败',
    }));

  // Keep the message channel open for the async gateway request.
  return true;
});
