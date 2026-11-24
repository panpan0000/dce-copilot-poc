/* DCE Agent 面板：多会话 + 多轮对话 + 历史 + 阶段状态。
 * 后端契约：whoami → {username}；chat {message, conversation_id} → {answer, session_id}。
 * 会话（conversation_id）由前端生成、后端按「用户 + 会话」哈希成 actor 会话，
 * 所以每个用户只能看到自己的会话。用户名常驻头部，是硬要求。 */
(() => {
  const ROOT_ID = 'dce-agent-root';
  if (document.getElementById(ROOT_ID)) return;

  const STORE_KEY = (user) => `dceAgentPoc:v5:${user || 'anonymous'}`;
  const MAX_CONVERSATIONS = 20;

  const state = {
    username: '',
    conversations: [], // [{id, title, messages:[{role,text,at}], updatedAt}]
    activeId: '',
    backendSession: '',
    busy: false,
    identityBusy: false,
    tab: 'chat',
    timer: null,
    startedAt: 0,
  };

  const el = (tag, cls, html) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (html != null) n.innerHTML = html;
    return n;
  };

  const newConvId = () =>
    (crypto.randomUUID ? crypto.randomUUID().replace(/-/g, '').slice(0, 16) : `c${Date.now().toString(36)}`);

  const root = el('section', 'dce-agent');
  root.id = ROOT_ID;
  root.innerHTML = `
    <div class="dce-agent__card" data-role="card">
      <header class="dce-agent__head">
        <div class="dce-agent__brand">
          <span class="dce-agent__logo" aria-hidden="true">
            <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="#fff" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <path d="M4.5 7A2.5 2.5 0 0 1 7 4.5h10A2.5 2.5 0 0 1 19.5 7v6.5A2.5 2.5 0 0 1 17 16H9.5L5 20v-4a2.5 2.5 0 0 1-.5-1.5z"/>
              <path d="M8.5 10h7"/>
            </svg>
          </span>
          <div>
            <div class="dce-agent__title">DCE Agent</div>
            <div class="dce-agent__sub">kgateway · Substrate 沙箱</div>
          </div>
        </div>
        <div class="dce-agent__head-right">
          <span class="dce-agent__user" data-role="userChip" title="当前 DCE 用户：所有命令都以该身份执行">
            <span class="dce-agent__avatar" data-role="avatar">?</span>
            <span class="dce-agent__user-text">
              <span class="dce-agent__user-label">当前用户</span>
              <strong data-role="identity" class="dce-agent__user-name">读取中…</strong>
            </span>
          </span>
          <button type="button" class="dce-agent__ghost" data-role="expand" title="放大/还原">放大</button>
          <button type="button" class="dce-agent__icon" data-role="close" title="关闭">✕</button>
        </div>
      </header>

      <div class="dce-agent__tabs">
        <button type="button" class="dce-agent__tab is-active" data-tab="chat">对话</button>
        <button type="button" class="dce-agent__tab" data-tab="history">历史 <span data-role="histCount" class="dce-agent__badge">0</span></button>
        <button type="button" class="dce-agent__new" data-role="clearAll" title="清空本地历史" hidden>清空</button>
        <button type="button" class="dce-agent__new" data-role="newConv" title="开启一个新会话">＋ 新会话</button>
      </div>

      <div class="dce-agent__body" data-role="body">
        <div class="dce-agent__thread" data-role="thread"></div>
        <div class="dce-agent__history" data-role="historyList" hidden></div>
      </div>

      <div class="dce-agent__status" data-role="status" role="status">
        <span data-role="statusText">正在连接 Agent Gateway…</span>
        <span class="dce-agent__bar" data-role="bar" hidden><i></i></span>
      </div>

      <form class="dce-agent__compose" data-role="form">
        <textarea data-role="input" rows="2" maxlength="2000"
          placeholder="例如：用 kubectl 看看 kagent 的 pod / 帮我拿最新巡检报告"></textarea>
        <div class="dce-agent__compose-bar">
          <span class="dce-agent__hint">以 <strong data-role="hintWho">…</strong> 身份执行 · Shift+Enter 发送</span>
          <button type="submit" class="dce-agent__send" data-role="send">发送</button>
        </div>
      </form>
    </div>
  `;
  document.documentElement.appendChild(root);

  const $ = (sel) => root.querySelector(sel);
  const card = $('[data-role="card"]');
  const identity = $('[data-role="identity"]');
  const histCount = $('[data-role="histCount"]');
  const threadBox = $('[data-role="thread"]');
  const historyBox = $('[data-role="historyList"]');
  const statusBox = $('[data-role="status"]');
  const statusText = $('[data-role="statusText"]');
  const bar = $('[data-role="bar"]');
  const form = $('[data-role="form"]');
  const input = $('[data-role="input"]');
  const sendBtn = $('[data-role="send"]');
  const body = $('[data-role="body"]');

  const escapeHtml = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

  function renderMarkdown(src) {
    const chunks = escapeHtml(src).split('```');
    let out = '';
    chunks.forEach((chunk, i) => {
      if (i % 2 === 1) {
        out += `<pre class="dce-agent__code">${chunk.replace(/^[a-zA-Z0-9_-]*\n/, '')}</pre>`;
        return;
      }
      let t = chunk
        .replace(/`([^`\n]+)`/g, '<code>$1</code>')
        .replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>')
        .replace(/^###\s+(.*)$/gm, '<h5>$1</h5>')
        .replace(/^##\s+(.*)$/gm, '<h4>$1</h4>')
        .replace(/^#\s+(.*)$/gm, '<h4>$1</h4>')
        .replace(/^\s*[-*]\s+(.*)$/gm, '<li>$1</li>')
        .replace(/^\s*(\d+)\.\s+(.*)$/gm, '<li>$1. $2</li>');
      t = t.replace(/(<li>[\s\S]*?<\/li>)(?!\s*<li>)/g, (m) => `<ul>${m}</ul>`);
      t = t.replace(/\n{2,}/g, '</p><p>').replace(/\n/g, '<br>');
      out += `<p>${t}</p>`;
    });
    return out;
  }

  const timeText = (ts) => new Date(ts).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' });
  const dayText = (ts) => {
    const d = new Date(ts);
    const sameDay = d.toDateString() === new Date().toDateString();
    return sameDay ? timeText(ts) : `${d.getMonth() + 1}/${d.getDate()} ${timeText(ts)}`;
  };

  const activeConv = () => state.conversations.find((c) => c.id === state.activeId) || null;
  const activeMessages = () => (activeConv()?.messages || []);

  function setStatus(text, kind) {
    statusText.textContent = text;
    statusBox.className = 'dce-agent__status' + (kind ? ` is-${kind}` : '');
  }

  function startProgress() {
    state.startedAt = Date.now();
    bar.hidden = false;
    clearInterval(state.timer);
    const tick = () => {
      const s = (Date.now() - state.startedAt) / 1000;
      // 后端没有流式进度，这里只能按耗时给粗略阶段
      const phase = s < 4 ? '正在唤醒沙箱' : s < 15 ? '模型处理中' : s < 40 ? '执行工具 / 生成回复中' : '仍在处理（复杂任务或首次唤醒）';
      setStatus(`进行中 · ${s.toFixed(1)}s · ${phase}`);
    };
    tick();
    state.timer = setInterval(tick, 500);
  }

  function stopProgress(ok, message) {
    clearInterval(state.timer);
    state.timer = null;
    bar.hidden = true;
    const s = ((Date.now() - state.startedAt) / 1000).toFixed(1);
    if (ok) setStatus(`完成 · ${s}s`, 'ok');
    else setStatus(`失败 · ${s}s · ${message}`, 'error');
  }

  function setBusy(v) {
    state.busy = v;
    sendBtn.disabled = v;
    sendBtn.textContent = v ? '处理中…' : '发送';
    input.disabled = v;
  }

  function scrollToBottom() {
    body.scrollTop = body.scrollHeight;
  }

  function renderThread() {
    threadBox.innerHTML = '';
    const messages = activeMessages();
    if (!messages.length) {
      const empty = el('div', 'dce-agent__empty');
      empty.innerHTML = `
        <div class="dce-agent__empty-title">可以这样问</div>
        <button type="button" class="dce-agent__chip" data-q="用 kubectl 看看可见命名空间的 pod。步骤：1) 先 dce-cli.js GET /apis/kpanda.io/v1alpha1/asl/namespaces?page=1&pageSize=50 拿到我可见的命名空间（不要用 kubectl get ns，集群级通常被拒）；2) 对每个命名空间执行 kubectl -n <ns> get pods -o wide；3) 如果 kubectl 报 Forbidden，再用 DCE API 查同一个命名空间的 pods 并说明差异；4) 都没有再说没权限。"">kubectl 查 pod</button>
        <button type="button" class="dce-agent__chip" data-q="帮我看看最新的 SRE 巡检报告（必须用 sre-cli report 取 SRE 产出的报告）">SRE 巡检报告</button>
        <button type="button" class="dce-agent__chip" data-q="执行 shell：uname -a 并贴出原始输出">执行 shell</button>
        <button type="button" class="dce-agent__chip" data-q="联网搜索 Kubernetes 官网，一句话说地址">联网搜索</button>
        <button type="button" class="dce-agent__chip" data-quick="notebooks">我的 Notebook</button>
        <button type="button" class="dce-agent__chip" data-quick="metrics">我的指标</button>
        <button type="button" class="dce-agent__chip" data-quick="rca">故障 RCA</button>
        <div class="dce-agent__empty-note">命令在该用户的 Substrate 沙箱里、以该用户的 K8s 身份执行。</div>`;
      threadBox.appendChild(empty);
      empty.querySelectorAll('.dce-agent__chip').forEach((b) => {
        b.addEventListener('click', () => { input.value = b.dataset.q; input.focus(); });
      });
      empty.querySelectorAll('[data-quick]').forEach((b) => {
        b.addEventListener('click', () => runQuick(b.dataset.quick));
      });
      return;
    }
    messages.forEach((m, idx) => {
      const wrap = el('div', `dce-agent__msg is-${m.role}`);
      wrap.dataset.idx = String(idx);
      if (m.role === 'user') {
        wrap.innerHTML = `
          <div class="dce-agent__msg-head"><span class="dce-agent__who">${escapeHtml(state.username || '我')}</span><span class="dce-agent__time">${timeText(m.at)}</span></div>
          <div class="dce-agent__bubble">${escapeHtml(m.text).replace(/\n/g, '<br>')}</div>`;
      } else if (m.role === 'agent') {
        wrap.innerHTML = `
          <div class="dce-agent__msg-head"><span class="dce-agent__who">Agent</span><span class="dce-agent__time">${timeText(m.at)}</span>
            <button type="button" class="dce-agent__copy" data-copy="${idx}">复制</button></div>
          <div class="dce-agent__bubble markdown">${renderMarkdown(m.text)}</div>`;
      } else {
        wrap.innerHTML = `
          <div class="dce-agent__msg-head"><span class="dce-agent__who">错误</span><span class="dce-agent__time">${timeText(m.at)}</span></div>
          <div class="dce-agent__bubble is-error">${escapeHtml(m.text).replace(/\n/g, '<br>')}</div>`;
      }
      threadBox.appendChild(wrap);
      if (m.role === 'error') {
        wrap.style.cursor = 'pointer';
        wrap.title = '点这里重试';
        wrap.addEventListener('click', () => {
          const prevUser = [...messages].slice(0, idx).reverse().find((x) => x.role === 'user');
          if (prevUser && !state.busy) {
            activeConv().messages.splice(idx, 1);
            input.value = prevUser.text;
            renderAll();
            form.requestSubmit();
          }
        });
      }
    });
    threadBox.querySelectorAll('.dce-agent__copy').forEach((b) => {
      b.addEventListener('click', async () => {
        const m = activeMessages()[Number(b.dataset.copy)];
        try {
          await navigator.clipboard.writeText(m.text);
          b.textContent = '已复制';
          setTimeout(() => { b.textContent = '复制'; }, 1200);
        } catch { b.textContent = '复制失败'; }
      });
    });
    scrollToBottom();
  }

  function convTitle(c) {
    const first = c.messages.find((m) => m.role === 'user');
    return first ? first.text.replace(/\s+/g, ' ').slice(0, 34) : '（空会话）';
  }

  function renderHistory() {
    historyBox.innerHTML = '';
    histCount.textContent = String(state.conversations.length);
    if (!state.conversations.length) {
      historyBox.appendChild(el('div', 'dce-agent__empty-note', '还没有会话。点「＋ 新会话」开始。'));
      return;
    }
    state.conversations
      .slice()
      .sort((a, b) => (b.updatedAt || 0) - (a.updatedAt || 0))
      .forEach((c) => {
        const row = el('button', 'dce-agent__conv' + (c.id === state.activeId ? ' is-active' : ''));
        row.type = 'button';
        row.innerHTML = `
          <span class="dce-agent__conv-main">
            <span class="dce-agent__conv-title">${escapeHtml(convTitle(c))}</span>
            <span class="dce-agent__conv-meta">${dayText(c.updatedAt || Date.now())} · ${c.messages.length} 条</span>
          </span>
          <span class="dce-agent__conv-del" title="删除">✕</span>`;
        row.addEventListener('click', (ev) => {
          if (ev.target.classList.contains('dce-agent__conv-del')) {
            state.conversations = state.conversations.filter((x) => x.id !== c.id);
            if (state.activeId === c.id) {
              state.activeId = state.conversations[0]?.id || '';
              if (!state.activeId) newConversation();
            }
            persist();
            renderAll();
            return;
          }
          state.activeId = c.id;
          state.backendSession = '';
          switchTab('chat');
          renderAll();
        });
        historyBox.appendChild(row);
      });
  }

  function renderAll() {
    renderThread();
    renderHistory();
  }

  function switchTab(tab) {
    state.tab = tab;
    root.querySelectorAll('.dce-agent__tab').forEach((t) => t.classList.toggle('is-active', t.dataset.tab === tab));
    threadBox.hidden = tab !== 'chat';
    historyBox.hidden = tab !== 'history';
    const clearBtn = $('[data-role="clearAll"]');
    clearBtn.hidden = tab !== 'history' || !state.conversations.length;
    clearBtn.textContent = '清空';
    clearBtn.dataset.armed = '';
    if (tab === 'history') renderHistory(); else scrollToBottom();
  }

  root.querySelectorAll('.dce-agent__tab').forEach((t) => t.addEventListener('click', () => switchTab(t.dataset.tab)));

  function newConversation() {
    const conv = { id: newConvId(), title: '', messages: [], updatedAt: Date.now() };
    state.conversations.unshift(conv);
    state.conversations = state.conversations.slice(0, MAX_CONVERSATIONS);
    state.activeId = conv.id;
    state.backendSession = '';
    persist();
    renderAll();
    switchTab('chat');
    input.focus();
  }

  $('[data-role="clearAll"]').addEventListener('click', (ev) => {
    const btn = ev.currentTarget;
    if (state.busy) { setStatus('有请求在处理，稍后再清空', 'error'); return; }
    if (btn.dataset.armed !== '1') {
      btn.dataset.armed = '1';
      btn.textContent = '确认清空？';
      setTimeout(() => { if (btn.dataset.armed === '1') { btn.dataset.armed = ''; btn.textContent = '清空'; } }, 4000);
      return;
    }
    state.conversations = [];
    state.activeId = '';
    state.backendSession = '';
    newConversation();
    setStatus('本地历史已清空', 'ok');
  });

  $('[data-role="newConv"]').addEventListener('click', () => {
    if (state.busy) { setStatus('上一个请求还在处理，稍等或刷新页面', 'error'); return; }
    newConversation();
  });

  async function persist() {
    if (!state.username) return;
    try {
      await chrome.storage.local.set({
        [STORE_KEY(state.username)]: { conversations: state.conversations, activeId: state.activeId },
      });
    } catch {}
  }

  async function purgeOldKeys() {
    try {
      const all = await chrome.storage.local.get(null);
      const stale = Object.keys(all).filter((k) => /^dceAgentPoc:v[34]:/.test(k));
      if (stale.length) await chrome.storage.local.remove(stale);
    } catch {}
  }

  async function restore() {
    if (!state.username) return;
    try {
      const key = STORE_KEY(state.username);
      const hit = await chrome.storage.local.get(key);
      const saved = hit && hit[key];
      if (saved && Array.isArray(saved.conversations) && saved.conversations.length) {
        state.conversations = saved.conversations.slice(0, MAX_CONVERSATIONS);
        state.activeId = saved.activeId && state.conversations.some((c) => c.id === saved.activeId)
          ? saved.activeId
          : state.conversations[0].id;
      }
    } catch {}
    if (!state.conversations.length) newConversation();
  }

  function runtimeMessage(message) {
    return new Promise((resolve, reject) => {
      chrome.runtime.sendMessage(message, (response) => {
        const err = chrome.runtime.lastError;
        if (err) { reject(new Error(err.message)); return; }
        resolve(response);
      });
    });
  }

  async function loadIdentity() {
    if (state.identityBusy) return;
    state.identityBusy = true;
    identity.textContent = '读取中…';
    try {
      const res = await runtimeMessage({ type: 'whoami' });
      if (!res?.ok) throw new Error(res?.error || `网关返回 HTTP ${res?.status || '错误'}`);
      const name = res.body?.username;
      if (typeof name !== 'string' || !name) throw new Error('网关未返回用户名');
      state.username = name;
      identity.textContent = name;
      $('[data-role="avatar"]').textContent = name.slice(0, 1).toUpperCase();
      $('[data-role="hintWho"]').textContent = name;
      setStatus('已连接', 'ok');
      await restore();
      renderAll();
    } catch (e) {
      identity.textContent = '未登录';
      setStatus(`身份读取失败：${e.message}（确认已登录 DCE 页面后点这里重试）`, 'error');
      statusBox.style.cursor = 'pointer';
      statusBox.onclick = () => { statusBox.onclick = null; statusBox.style.cursor = ''; loadIdentity(); };
    } finally {
      state.identityBusy = false;
    }
  }

  const md = (t) => `\n\`\`\`\n${t}\n\`\`\`\n`;

  // 三个快捷按钮不再写死 API 路径：交给 DSH 用 dce CLI 自己发现并调用，
  // 成败都由它按真实返回说明（权限、路由、参数）。
  const QUICK_PROMPTS = {
    notebooks: '用 dce 列出我可见的 AI Lab notebook 实例：先用 dce search 找命令，再执行，把真实 HTTP 状态和返回贴出来；403/404 也原样说明。',
    metrics: '用 DCE Insight 取我命名空间里第一个 Pod 的 CPU 和内存指标，最近 15 分钟。' +
      '执行：dce insight resource get-pod-metrics --cluster kpanda-global-cluster --namespace <我的命名空间> --name <Pod> ' +
      '--start $(( $(date +%s) - 900 )) --end $(date +%s) --step 60 --query-list pod_cpu_usage --query-list pod_memory_usage -o json 。' +
      '（若返回 query ... not support，说明参数名要用 pod_cpu_usage / pod_memory_usage。）把返回里真实的采样值和峰值贴出来，不要编数字。',
    rca: '对我命名空间做一次故障 RCA，必须包含三项真实取证，每项都贴原始输出或关键数字：' +
      '1) Pod 状态：kubectl get pods -o wide（没有 kubectl 权限就用 dce container-management 对应接口）；' +
      '2) Pod 日志：kubectl logs <异常 Pod> --tail=50；' +
      '3) Pod 指标：dce insight resource get-pod-metrics --cluster kpanda-global-cluster --namespace <我的命名空间> --name <异常 Pod> ' +
      '--start $(( $(date +%s) - 3600 )) --end $(date +%s) --step 60 --query-list pod_cpu_usage --query-list pod_memory_usage -o json 。' +
      '最后给出结论，并引用第 3 步里的真实 CPU/内存数字；如果某个接口失败，说明失败原因和 HTTP 状态。',
  };

  function runQuick(kind) {
    if (state.busy) { setStatus('上一个请求还在处理中', 'error'); return; }
    if (!state.username) { setStatus('还没读到 DCE 用户，先点状态条重试', 'error'); return; }
    input.value = QUICK_PROMPTS[kind] || '';
    form.requestSubmit();
  }

  // 流式发送：先铺一条空的 Agent 气泡，边收边画
  function streamChat({ text, conversationId }) {
    return new Promise((resolve) => {
      const conv = activeConv();
      const bubble = { role: 'agent', text: '', at: Date.now() };
      conv.messages.push(bubble);
      conv.updatedAt = Date.now();
      renderThread();
      const paint = () => {
        const last = threadBox.lastElementChild?.querySelector('.dce-agent__bubble.markdown');
        if (last) last.innerHTML = renderMarkdown(bubble.text || '…');
        scrollToBottom();
      };
      paint();

      const port = chrome.runtime.connect({ name: 'dsh-stream' });
      let settled = false;
      // MV3 的后台 30 秒没活动会被浏览器回收；模型思考期间没有消息，必须自己保活。
      const heartbeat = setInterval(() => { try { port.postMessage({ type: 'ping' }); } catch {} }, 12000);
      const finish = (sessionId) => {
        if (settled) return;
        settled = true;
        clearInterval(heartbeat);
        if (sessionId) state.backendSession = sessionId;
        if (!bubble.text) bubble.text = '（没有返回内容）';
        renderThread();
        persist();
        resolve({ ok: true });
      };
      // 断线兜底：改走一次性接口（后台不会被长连接回收），把完整答案补回来
      const fallback = async (why) => {
        if (settled) return;
        settled = true;
        clearInterval(heartbeat);
        setStatus(`流式连接断了（${why}），正在用非流式重试…`);
        try {
          const res = await runtimeMessage({
            type: 'chat',
            message: text,
            conversationId: conv.id,
            sessionId: state.backendSession || undefined,
          });
          if (!res?.ok) throw new Error(res?.error || `HTTP ${res?.status}`);
          const b = res.body || {};
          const answer = typeof b.answer === 'string' ? b.answer
            : b.result?.report ? String(b.result.report)
            : typeof b === 'string' ? b : JSON.stringify(b, null, 2);
          conv.messages[conv.messages.length - 1] = { role: 'agent', text: (bubble.text || '') + (bubble.text ? '\n\n---\n' : '') + (answer || '（没有返回内容）'), at: Date.now() };
          conv.updatedAt = Date.now();
          renderThread();
          persist();
          resolve({ ok: true, fallback: true });
        } catch (e) {
          conv.messages[conv.messages.length - 1] = {
            role: 'error', text: `${why}；非流式重试也失败：${e.message}\n（点这条消息重试）`, at: Date.now(),
          };
          renderThread();
          persist();
          resolve({ ok: false, error: e.message });
        }
      };

      const fail = (msg) => { void fallback(msg); };
      port.onMessage.addListener((m) => {
        if (m.type === 'delta') {
          bubble.text += m.text;
          paint();
        } else if (m.type === 'done') {
          finish(m.sessionId);
        } else if (m.type === 'error') {
          fail(m.message);
        }
      });
      port.onDisconnect.addListener(() => { if (!settled) fail('连接中断（刷新页面重试）'); });
      port.postMessage({ type: 'chat-stream', message: text, conversationId });
    });
  }

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (state.busy) return;
    const text = input.value.trim();
    if (!text) { input.focus(); return; }
    if (!state.username) { setStatus('还没读到 DCE 用户，先点状态条重试', 'error'); return; }
    if (!activeConv()) newConversation();

    const conv = activeConv();
    conv.messages.push({ role: 'user', text, at: Date.now() });
    conv.title = conv.title || convTitle(conv);
    conv.updatedAt = Date.now();
    input.value = '';
    setBusy(true);
    startProgress();
    try {
      const res = await streamChat({ text, conversationId: conv.id });
      if (res.ok) stopProgress(true);
      else stopProgress(false, res.error);
    } catch (e) {
      stopProgress(false, e.message);
    } finally {
      setBusy(false);
      renderAll();
      persist();
      input.focus();
    }
  });

  input.addEventListener('keydown', (ev) => {
    if (ev.isComposing || ev.keyCode === 229) return;
    if (ev.key === 'Enter' && ev.shiftKey) { ev.preventDefault(); form.requestSubmit(); }
  });

  $('[data-role="close"]').addEventListener('click', () => { root.remove(); });
  $('[data-role="expand"]').addEventListener('click', (ev) => {
    const on = card.classList.toggle('is-expanded');
    ev.currentTarget.textContent = on ? '还原' : '放大';
  });

  purgeOldKeys().then(loadIdentity);
})();
