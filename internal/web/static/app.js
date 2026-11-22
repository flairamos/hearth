/* hearth web GUI — vanilla JS, no dependencies */
const $ = (id) => document.getElementById(id);
let sessionId = null, busy = false, agentMode = false, es = null;
let pendingImgs = 0;
let live = null; // streaming message element

function esc(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}
function md(s) {
  // minimal markdown: code fences, headings, bold, inline code, lists
  let h = esc(s);
  h = h.replace(/```(\w*)\n([\s\S]*?)```/g, (_, l, c) => `<pre><code>${c}</code></pre>`);
  h = h.replace(/^### (.*)$/gm, "<h3>$1</h3>").replace(/^## (.*)$/gm, "<h2>$1</h2>").replace(/^# (.*)$/gm, "<h1>$1</h1>");
  h = h.replace(/`([^`\n]+)`/g, "<code>$1</code>");
  h = h.replace(/\*\*([^*\n]+)\*\*/g, "<b>$1</b>");
  h = h.replace(/^[-*] (.*)$/gm, "• $1");
  return h;
}

function addMsg(role, text) {
  const div = document.createElement("div");
  div.className = "msg " + role;
  const label = role === "user" ? "you" : role === "assistant" ? "assistant" : role;
  div.innerHTML = `<div class="role">${label}</div><div class="body">${role === "assistant" ? md(text) : esc(text)}</div>`;
  $("chat").appendChild(div);
  scrollBottom();
  return div;
}
function scrollBottom() { $("chat").scrollTop = $("chat").scrollHeight; }

async function loadSessions() {
  const list = await (await fetch("/api/sessions")).json();
  const ul = $("session-list");
  ul.innerHTML = "";
  for (const s of list) {
    const li = document.createElement("li");
    li.textContent = `${s.title} (${s.msgCount})`;
    li.dataset.id = s.id;
    if (s.id === sessionId) li.className = "active";
    li.onclick = () => loadSession(s.id);
    ul.appendChild(li);
  }
}

async function renderFromState() {
  const st = await (await fetch("/api/state")).json();
  sessionId = st.sessionID;
  renderMessages(st.messages);
  scrollBottom();
}

async function loadSession(id) {
  const st = await (await fetch("/api/session/load?id=" + encodeURIComponent(id), { method: "POST" })).json();
  sessionId = st.sessionID;
  renderMessages(st.messages);
  loadSessions();
  updateInfo();
}

function renderMessages(msgs) {
  $("chat").innerHTML = "";
  for (const m of msgs) {
    if (m.role === "tool") { addTool(m.toolName, m.content, m.toolError); continue; }
    if (m.role === "system") continue;
    addMsg(m.role, m.content);
  }
  scrollBottom();
}

function addTool(name, text, isErr) {
  const div = document.createElement("div");
  div.className = "tool" + (isErr ? " err" : "");
  div.textContent = `${isErr ? "✗" : "✓"} ${name} — ${text.slice(0, 300)}`;
  $("chat").appendChild(div);
  scrollBottom();
}

function updateInfo() {
  $("side-info").textContent = `model: ${modelName || "-"}\nmode: ${agentMode ? "agent" : "chat"}`;
  $("side-info").style.whiteSpace = "pre-wrap";
}

function startEvents() {
  if (es) es.close();
  es = new EventSource("/api/events");
  es.onmessage = (e) => {
    const ev = JSON.parse(e.data);
    switch (ev.type) {
      case "user":
        addMsg("user", ev.text + (ev.images ? `  📎 图片 ×${ev.images}` : ""));
        live = addMsg("assistant", "");
        live.querySelector(".body").classList.add("cursor-live");
        pendingImgs = 0;
        updateInfo();
        break;
      case "thinking":
      case "delta": {
        if (!live) live = addMsg("assistant", "");
        const body = live.querySelector(".body");
        // two stable children: thinking accumulates separately so content
        // deltas never wipe it (and vice versa)
        let th = body.querySelector(".thinking");
        let ct = body.querySelector(".content");
        if (!th) {
          th = document.createElement("div");
          th.className = "thinking";
          body.appendChild(th);
        }
        if (!ct) {
          ct = document.createElement("div");
          ct.className = "content";
          body.appendChild(ct);
        }
        if (ev.type === "thinking") {
          live._think = (live._think || "") + ev.text;
          th.textContent = live._think;
        } else {
          live._text = (live._text || "") + ev.text;
          ct.innerHTML = md(live._text);
        }
        body.classList.add("cursor-live");
        scrollBottom();
        break;
      }
      case "tool_start": {
        let brief = ev.args || "";
        try {
          const a = JSON.parse(ev.args);
          brief = a.command || a.path || a.pattern || a.query || a.url || a.content || ev.args;
        } catch (_) { /* raw */ }
        addTool(ev.tool + " ▶ " + brief, "", false);
        break;
      }
      case "tool_result":
        addTool(ev.tool, ev.text, ev.error);
        break;
      case "approval":
        $("approval").classList.remove("hidden");
        $("ap-tool").textContent = ev.tool + " — " + ev.text;
        $("ap-detail").textContent = ev.detail || "";
        $("approval").dataset.id = ev.id;
        break;
      case "error":
        addMsg("error", "⚠ " + ev.text);
        break;
      case "notice":
        addMsg("notice", ev.text);
        break;
      case "stats":
        $("metrics").textContent =
          `ctx ${(ev.ctxUsed / 1000).toFixed(1)}k/${ev.ctxMax} ${ev.pct}%` +
          (ev.tps ? ` · ⚡ ${ev.tps.toFixed(1)} tok/s · ${ev.tokens} tok` : "");
        break;
      case "model":
        modelName = ev.text;
        updateInfo();
        break;
      case "provider":
        providerName = ev.text;
        updateInfo();
        break;
      case "reset":
        $("chat").innerHTML = "";
        live = null;
        sessionId = null;
        loadSessions();
        break;
      case "done":
        if (live) live.querySelector(".body").classList.remove("cursor-live");
        live = null;
        busy = false;
        setBusyUI(false);
        loadSessions();
        // the assembled session message is the source of truth — re-render
        // (covers page reloads mid-turn and interrupted partials)
        try { renderFromState(); } catch (_) {}
        updateInfo();
        break;
    }
  };
}

async function send() {
  if (busy) return;
  const text = $("input").value.trim();
  if (!text) return;
  const res = await fetch("/api/send", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ text, agent: agentMode }),
  });
  if (res.status === 409) { addMsg("error", "⚠ busy — 当前回合未结束"); return; }
  $("input").value = "";
  busy = true;
  setBusyUI(true);
}

function setBusyUI(b) {
  $("btn-send").classList.toggle("hidden", b);
  $("btn-stop").classList.toggle("hidden", !b);
}

async function newSession() {
  await fetch("/api/session/new", { method: "POST" });
  sessionId = null;
  $("chat").innerHTML = "";
  live = null;
  loadSessions();
  const st = await (await fetch("/api/state")).json();
  renderMessages(st.messages);
  updateInfo();
}

async function interrupt() {
  await fetch("/api/interrupt", { method: "POST" });
}

let modelName = null;
let providerName = "local";
async function updateInfo() {
  const st = await (await fetch("/api/state")).json();
  modelName = st.model;
  providerName = st.provider;
  agentMode = st.agentMode;
  $("agent-mode").checked = st.agentMode;
  $("side-info").textContent = `model: ${st.model || "-"}\nprovider: ${st.provider}\nmode: ${st.agentMode ? "agent" : "chat"}`;
  $("side-info").style.whiteSpace = "pre-wrap";
}

$("btn-img").onclick = () => $("img-file").click();
$("img-file").onchange = async (e) => {
  const f = e.target.files[0];
  if (!f) return;
  const fd = new FormData();
  fd.append("file", f);
  const res = await fetch("/api/img", { method: "POST", body: fd });
  if (res.ok) {
    const out = await res.json();
    pendingImgs = out.count;
    $("status").textContent = `📎 图片已附加 ×${out.count}`;
  } else {
    $("status").textContent = "⚠ 图片附加失败(仅 png/jpeg/webp/gif,≤8MB)";
  }
  e.target.value = "";
};
$("btn-retry").onclick = async () => {
  if (busy) return;
  await fetch("/api/retry", { method: "POST" });
};
$("btn-export").onclick = () => {
  window.open("/api/export", "_blank");
};

$("agent-mode").onchange = async (e) => {
  await fetch("/api/agent-mode", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ on: e.target.checked }),
  });
  agentMode = e.target.checked;
  updateInfo();
};
$("btn-send").onclick = send;
$("btn-stop").onclick = interrupt;
$("btn-new").onclick = newSession;
// ---- popup picker: @ files + / commands, ↑↓ navigate, Enter completes/runs ----
let pickMode = null; // "file" | "cmd"
let pickItems = [];
let pickIndex = 0;

function hidePicker() {
  pickMode = null;
  pickItems = [];
  $("picker").classList.remove("open");
  $("picker").innerHTML = "";
}

function renderPick() {
  const pk = $("picker");
  pk.innerHTML = "";
  pickItems.forEach((it, i) => {
    const div = document.createElement("div");
    div.className = "pick" + (i === pickIndex ? " sel" : "");
    div.innerHTML = `<span>${esc(it.name)}</span>` +
      (it.desc ? `<span class="dim">${esc(it.desc)}</span>` : "");
    div.onclick = () => { pickIndex = i; completePick(); };
    pk.appendChild(div);
  });
  pk.classList.add("open");
  const sel = pk.querySelector(".sel");
  if (sel) sel.scrollIntoView({ block: "nearest" });
}

function completePick() {
  const it = pickItems[pickIndex];
  if (!it) return;
  if (pickMode === "file") {
    $("input").value = $("input").value.replace(/@([^@\s]*)$/, "@" + it.name + " ");
  } else {
    // commands without arguments run immediately; others complete for editing
    $("input").value = "/" + it.name + " ";
    if (!it.usage) {
      hidePicker();
      send();
      return;
    }
  }
  hidePicker();
  $("input").focus();
}

async function showFilePicker(query) {
  const res = await fetch("/api/files?query=" + encodeURIComponent(query));
  const files = await res.json();
  pickItems = (files || []).map((f) => ({ name: f, desc: "" }));
  pickMode = "file";
  pickIndex = 0;
  renderPick();
}

async function showCmdPicker(query) {
  const res = await fetch("/api/commands");
  const cmds = await res.json();
  pickItems = cmds.filter((c) => c.name.startsWith(query));
  pickMode = "cmd";
  pickIndex = 0;
  renderPick();
}

$("input").addEventListener("keydown", (e) => {
  if (pickMode) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      pickIndex = (pickIndex + 1) % pickItems.length;
      renderPick();
      return;
    }
    if (e.key === "ArrowUp") {
      e.preventDefault();
      pickIndex = (pickIndex - 1 + pickItems.length) % pickItems.length;
      renderPick();
      return;
    }
    if (e.key === "Escape") {
      e.preventDefault();
      hidePicker();
      return;
    }
    if (e.key === "Enter" || e.key === "Tab") {
      e.preventDefault();
      if (pickItems.length === 0) { hidePicker(); send(); return; } // unmatched command → let the server respond
      completePick();
      return;
    }
  }
  if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); }
});
$("input").addEventListener("input", () => {
  const v = $("input").value;
  if (v.startsWith("/") && !v.includes(" ")) {
    showCmdPicker(v.slice(1).toLowerCase());
    return;
  }
  const m = v.match(/@([^@\s]*)$/);
  if (m && !busy) showFilePicker(m[1]);
  else hidePicker();
});
document.querySelectorAll(".ap-actions button").forEach((b) => {
  b.onclick = async () => {
    const id = $("approval").dataset.id;
    const dec = b.id === "ap-once" ? "once" : b.id === "ap-session" ? "session" : "deny";
    await fetch("/api/approve", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id, decision: dec }),
    });
    $("approval").classList.add("hidden");
  };
});

async function boot() {
  const st = await (await fetch("/api/state")).json();
  sessionId = st.sessionID;
  renderMessages(st.messages);
  startEvents();
  loadSessions();
  updateInfo();
}
boot();
