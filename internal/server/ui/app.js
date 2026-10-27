// Forgeline web UI. No framework and no build step. Everything that comes from the server
// (pipeline names, step names, log output) is untrusted, so it is only ever inserted as text.

const TOKEN_KEY = "forgeline.token";
const app = document.getElementById("app");
const signoutBtn = document.getElementById("signout");

const GLYPH = {
  queued: "◷", pending: "·", running: "▶", succeeded: "✓", cached: "✓",
  failed: "✗", canceled: "⊘", skipped: "↷",
};
const FINAL = new Set(["succeeded", "failed", "canceled"]);
const MAX_LOG_CHARS = 2_000_000;

let cleanup = () => {}; // stops timers and streams of the current view

// ---- helpers ---------------------------------------------------------------------------

function h(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else if (k === "class") node.className = v;
    else node.setAttribute(k, v === true ? "" : v);
  }
  node.append(...children.flat().filter((c) => c != null));
  return node;
}

const SVG_NS = "http://www.w3.org/2000/svg";
function s(tag, attrs, ...children) {
  const node = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs || {})) node.setAttribute(k, v);
  node.append(...children.flat().filter((c) => c != null));
  return node;
}

function badge(status) {
  return h("span", { class: `badge ${status}` },
    h("span", { class: "glyph", "aria-hidden": "true" }, GLYPH[status] || "?"), status);
}

function fmtTime(iso) {
  return iso ? new Date(iso).toLocaleString() : "—";
}

function fmtDuration(startIso, endIso) {
  if (!startIso) return "—";
  const ms = (endIso ? new Date(endIso) : new Date()) - new Date(startIso);
  const sec = Math.max(0, Math.round(ms / 1000));
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  return min < 60 ? `${min}m ${sec % 60}s` : `${Math.floor(min / 60)}h ${min % 60}m`;
}

// ANSI colour codes are noise in a plain text pane.
const ANSI = /\u001b\[[0-9;?]*[ -/]*[@-~]/g;

class AuthError extends Error {}

async function api(path, options = {}) {
  const token = sessionStorage.getItem(TOKEN_KEY);
  const resp = await fetch(path, {
    ...options,
    headers: { ...(options.headers || {}), Authorization: `Bearer ${token}` },
  });
  if (resp.status === 401) throw new AuthError("The token was not accepted.");
  return resp;
}

async function apiJSON(path, options) {
  const resp = await api(path, options);
  const body = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(body.error || `Server returned ${resp.status}`);
  return body;
}

// ---- router ----------------------------------------------------------------------------

function route() {
  cleanup();
  cleanup = () => {};
  const token = sessionStorage.getItem(TOKEN_KEY);
  signoutBtn.hidden = !token;
  if (!token) return showLogin();
  const m = location.hash.match(/^#\/runs\/([A-Za-z0-9_-]+)$/);
  (m ? showRun(m[1]) : showList()).catch(handleError);
  app.focus({ preventScroll: true });
}

function handleError(err) {
  if (err instanceof AuthError) {
    sessionStorage.removeItem(TOKEN_KEY);
    return showLogin(err.message);
  }
  app.replaceChildren(h("h1", {}, "Something went wrong"), h("p", { class: "error", role: "alert" }, err.message));
}

signoutBtn.addEventListener("click", () => {
  sessionStorage.removeItem(TOKEN_KEY);
  // A link such as /#token=... (printed by a local `forgeline server` that generated its own
// token) signs in once. The fragment is never sent to the server, and it is removed from the
// address bar and history entry straight away.
const fromLink = location.hash.match(/^#token=([A-Za-z0-9_-]+)$/);
if (fromLink) {
  sessionStorage.setItem(TOKEN_KEY, fromLink[1]);
  history.replaceState(null, "", location.pathname + location.search);
}

route();
});
window.addEventListener("hashchange", route);

// ---- login -----------------------------------------------------------------------------

function showLogin(message) {
  document.title = "Sign in · Forgeline";
  const input = h("input", { id: "token", type: "password", autocomplete: "off", required: true });
  const errBox = h("div", { role: "alert" });
  if (message) errBox.append(h("p", { class: "error" }, message));
  const form = h("form", { class: "login" },
    h("h1", {}, "Sign in"),
    h("p", { class: "muted" }, "Enter the server's bearer token (FORGELINE_TOKEN). It is kept for this browser tab only."),
    errBox,
    h("label", { for: "token" }, "Token"),
    input,
    h("button", { class: "primary", type: "submit" }, "Sign in"));
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    sessionStorage.setItem(TOKEN_KEY, input.value.trim());
    try {
      await apiJSON("/v1/runs?limit=1");
      // A link such as /#token=... (printed by a local `forgeline server` that generated its own
// token) signs in once. The fragment is never sent to the server, and it is removed from the
// address bar and history entry straight away.
const fromLink = location.hash.match(/^#token=([A-Za-z0-9_-]+)$/);
if (fromLink) {
  sessionStorage.setItem(TOKEN_KEY, fromLink[1]);
  history.replaceState(null, "", location.pathname + location.search);
}

route();
    } catch (err) {
      sessionStorage.removeItem(TOKEN_KEY);
      errBox.replaceChildren(h("p", { class: "error" }, err instanceof AuthError ? err.message : `Could not reach the server: ${err.message}`));
    }
  });
  app.replaceChildren(form);
  input.focus();
}

// ---- run list --------------------------------------------------------------------------

async function showList() {
  document.title = "Runs · Forgeline";
  const tbody = h("tbody");
  const more = h("button", { type: "button", hidden: true }, "Load older runs");
  const empty = h("p", { class: "muted", hidden: true }, "No runs yet. Send one with: forgeline submit --server … pipeline.yaml");
  app.replaceChildren(
    h("h1", {}, "Runs"),
    h("div", { class: "table-wrap" },
      h("table", {},
        h("thead", {}, h("tr", {}, ...["Run", "Pipeline", "Status", "Trigger", "Started", "Duration"].map((t) => h("th", { scope: "col" }, t)))),
        tbody)),
    empty, h("p", {}, more));

  let nextBefore = "";
  const rows = new Map();
  const addRow = (run) => {
    const tr = h("tr", {},
      h("td", { class: "id" }, h("a", { href: `#/runs/${run.id}` }, run.id.slice(0, 8))),
      h("td", {}, run.pipeline),
      h("td", {}, badge(run.status)),
      h("td", {}, run.trigger),
      h("td", {}, fmtTime(run.started_at || run.created_at)),
      h("td", {}, fmtDuration(run.started_at, run.finished_at)));
    return tr;
  };
  const render = (runs, append) => {
    for (const run of runs) {
      const tr = addRow(run);
      const old = rows.get(run.id);
      if (old) old.replaceWith(tr);
      else if (append) tbody.append(tr);
      else tbody.prepend(tr);
      rows.set(run.id, tr);
    }
    empty.hidden = rows.size > 0;
  };

  const first = await apiJSON("/v1/runs?limit=50");
  render(first.runs, true);
  nextBefore = first.next_before || "";
  more.hidden = !nextBefore;
  more.addEventListener("click", async () => {
    more.disabled = true;
    try {
      const page = await apiJSON(`/v1/runs?limit=50&before=${encodeURIComponent(nextBefore)}`);
      render(page.runs, true);
      nextBefore = page.next_before || "";
      more.hidden = !nextBefore;
    } catch (err) {
      handleError(err);
    }
    more.disabled = false;
  });

  // Refresh the newest page so statuses and new runs show up. Older pages stay as loaded.
  const timer = setInterval(async () => {
    try {
      render((await apiJSON("/v1/runs?limit=50")).runs.reverse(), false);
    } catch (err) {
      if (err instanceof AuthError) handleError(err);
    }
  }, 3000);
  cleanup = () => clearInterval(timer);
}

// ---- run page --------------------------------------------------------------------------

async function showRun(id) {
  document.title = `Run ${id.slice(0, 8)} · Forgeline`;
  const head = h("div");
  const dagBox = h("div", { class: "dag-wrap" });
  const stepsBox = h("ul", { class: "steps" });
  const logTitle = h("h2", {}, "Log");
  const logState = h("span", { class: "muted", "aria-live": "polite" });
  const logPre = h("pre", { class: "log", tabindex: "0", "aria-label": "Step log" });
  app.replaceChildren(
    head,
    h("h2", {}, "Steps"), dagBox,
    h("h2", {}, "Choose a step"), stepsBox,
    h("div", { class: "log-head" }, logTitle, logState),
    logPre);

  let run = await apiJSON(`/v1/runs/${encodeURIComponent(id)}`);
  let selected = null;
  let stream = null; // { stepId, abort }

  const pickDefault = () => {
    const steps = run.steps || [];
    return (steps.find((x) => x.status === "failed") || steps.find((x) => x.status === "running") || steps[0] || {}).id || null;
  };

  const select = (stepId) => {
    if (stepId === selected) return;
    selected = stepId;
    if (stream) stream.abort.abort();
    logPre.replaceChildren();
    logTitle.textContent = stepId ? `Log: ${stepId}` : "Log";
    logState.textContent = "";
    if (stepId) stream = followLog(run.id, stepId, logPre, logState);
    renderSteps();
  };

  const renderSteps = () => {
    dagBox.replaceChildren(renderDag(run.steps || [], selected, select));
    stepsBox.replaceChildren(...(run.steps || []).map((st) =>
      h("li", {}, h("button", { type: "button", "aria-pressed": String(st.id === selected), onclick: () => select(st.id) },
        badge(st.status), st.id))));
  };

  const renderHead = () => {
    const cancel = h("button", { type: "button" }, "Cancel run");
    cancel.hidden = FINAL.has(run.status);
    cancel.addEventListener("click", async () => {
      cancel.disabled = true;
      try { run = await apiJSON(`/v1/runs/${encodeURIComponent(run.id)}/cancel`, { method: "POST" }); renderAll(); }
      catch (err) { handleError(err); }
    });
    head.replaceChildren(
      h("p", {}, h("a", { href: "#/" }, "← All runs")),
      h("div", { class: "run-head" }, h("h1", {}, run.pipeline), badge(run.status), cancel),
      run.reason ? h("p", { class: "error", role: "status" }, run.reason) : null,
      h("div", { class: "meta" },
        h("span", {}, `Run ${run.id}`),
        h("span", {}, `Trigger: ${run.trigger}`),
        run.source && run.source.sha ? h("span", {}, `Commit ${run.source.sha.slice(0, 8)}`) : null,
        h("span", {}, `Started: ${fmtTime(run.started_at)}`),
        h("span", {}, `Duration: ${fmtDuration(run.started_at, run.finished_at)}`)));
  };

  const renderAll = () => { renderHead(); renderSteps(); };

  select(pickDefault());
  renderAll();

  const timer = setInterval(async () => {
    try {
      run = await apiJSON(`/v1/runs/${encodeURIComponent(id)}`);
      renderAll();
      if (FINAL.has(run.status)) clearInterval(timer);
    } catch (err) {
      clearInterval(timer);
      handleError(err);
    }
  }, 1000);
  cleanup = () => { clearInterval(timer); if (stream) stream.abort.abort(); };
}

// ---- DAG -------------------------------------------------------------------------------

const NODE_W = 176, NODE_H = 56, GAP_X = 64, GAP_Y = 20, PAD = 16;

function renderDag(steps, selected, onSelect) {
  if (!steps.length) return h("p", { class: "muted" }, "No steps.");
  const byId = new Map(steps.map((st) => [st.id, st]));
  const depth = new Map();
  const visit = (st, seen = new Set()) => {
    if (depth.has(st.id)) return depth.get(st.id);
    if (seen.has(st.id)) return 0; // a cycle cannot reach here: the server rejects them
    seen.add(st.id);
    let d = 0;
    for (const n of st.needs || []) if (byId.has(n)) d = Math.max(d, visit(byId.get(n), seen) + 1);
    depth.set(st.id, d);
    return d;
  };
  steps.forEach((st) => visit(st));

  const columns = [];
  for (const st of steps) (columns[depth.get(st.id)] ||= []).push(st);
  const pos = new Map();
  columns.forEach((col, x) => col.forEach((st, y) =>
    pos.set(st.id, { x: PAD + x * (NODE_W + GAP_X), y: PAD + y * (NODE_H + GAP_Y) })));
  const width = PAD * 2 + columns.length * NODE_W + (columns.length - 1) * GAP_X;
  const height = PAD * 2 + Math.max(...columns.map((c) => c.length)) * NODE_H + (Math.max(...columns.map((c) => c.length)) - 1) * GAP_Y;

  const svg = s("svg", { class: "dag", viewBox: `0 0 ${width} ${height}`, width, height, role: "img",
    "aria-label": `Pipeline graph with ${steps.length} steps. Use the step buttons below to choose a step.` },
    s("defs", {}, s("marker", { id: "arrow", viewBox: "0 0 10 10", refX: "9", refY: "5", markerWidth: "7", markerHeight: "7", orient: "auto-start-reverse" },
      s("path", { d: "M0 0 L10 5 L0 10 z" }))));

  for (const st of steps) {
    for (const n of st.needs || []) {
      const a = pos.get(n), b = pos.get(st.id);
      if (!a || !b) continue;
      const x1 = a.x + NODE_W, y1 = a.y + NODE_H / 2, x2 = b.x, y2 = b.y + NODE_H / 2;
      const mid = (x1 + x2) / 2;
      svg.append(s("path", { class: "edge", d: `M${x1} ${y1} C${mid} ${y1} ${mid} ${y2} ${x2 - 2} ${y2}`, "marker-end": "url(#arrow)" }));
    }
  }
  for (const st of steps) {
    const { x, y } = pos.get(st.id);
    const label = st.id.length > 22 ? st.id.slice(0, 21) + "…" : st.id;
    const g = s("g", { class: `node ${st.status}${st.id === selected ? " selected" : ""}`, transform: `translate(${x} ${y})` },
      s("title", {}, `${st.id}: ${st.status}`),
      s("rect", { width: NODE_W, height: NODE_H, rx: 8 }),
      s("text", { x: 12, y: 24 }, label),
      s("text", { class: "sub", x: 12, y: 43 }, `${GLYPH[st.status] || "?"} ${st.status}`));
    g.addEventListener("click", () => onSelect(st.id));
    svg.append(g);
  }
  return svg;
}

// ---- live log --------------------------------------------------------------------------

// followLog streams a step's log into pre with fetch, because EventSource cannot send the
// Authorization header. It resumes with Last-Event-ID if the connection drops.
function followLog(runId, stepId, pre, stateEl) {
  const abort = new AbortController();
  let lastSeq = 0, chars = 0, truncated = false;

  const append = (text) => {
    if (truncated) return;
    const nearBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
    chars += text.length;
    if (chars > MAX_LOG_CHARS) {
      truncated = true;
      pre.append("\n[view truncated: open /v1/runs/…/logs to read the whole log]\n");
    } else {
      pre.append(text.replace(ANSI, ""));
    }
    if (nearBottom) pre.scrollTop = pre.scrollHeight;
  };

  const run = async () => {
    for (let attempt = 0; attempt < 5 && !abort.signal.aborted; attempt++) {
      try {
        const headers = { Accept: "text/event-stream" };
        if (lastSeq) headers["Last-Event-ID"] = String(lastSeq);
        const resp = await api(`/v1/runs/${encodeURIComponent(runId)}/steps/${encodeURIComponent(stepId)}/logs`, { headers, signal: abort.signal });
        if (!resp.ok) throw new Error(`log request failed (${resp.status})`);
        stateEl.textContent = "following…";
        if (await readEvents(resp.body, abort.signal)) return;
      } catch (err) {
        if (abort.signal.aborted) return;
        if (err instanceof AuthError) return handleError(err);
        stateEl.textContent = `connection lost, retrying… (${err.message})`;
      }
      await new Promise((r) => setTimeout(r, 1000));
    }
    if (!abort.signal.aborted) stateEl.textContent = "log stream stopped; reload to retry";
  };

  // Returns true when the server sent the end event.
  async function readEvents(body, signal) {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    while (!signal.aborted) {
      const { done, value } = await reader.read();
      if (done) return false;
      buf += decoder.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const raw = buf.slice(0, i);
        buf = buf.slice(i + 2);
        let event = "message", data = "";
        for (const line of raw.split("\n")) {
          if (line.startsWith("event: ")) event = line.slice(7);
          else if (line.startsWith("data: ")) data = line.slice(6);
        }
        if (!data) continue;
        const payload = JSON.parse(data);
        if (event === "end") {
          stateEl.textContent = `step ${payload.status}`;
          return true;
        }
        lastSeq = payload.seq;
        append(payload.text);
      }
    }
    return false;
  }

  run();
  return { stepId, abort };
}

// A link such as /#token=... (printed by a local `forgeline server` that generated its own
// token) signs in once. The fragment is never sent to the server, and it is removed from the
// address bar and history entry straight away.
const fromLink = location.hash.match(/^#token=([A-Za-z0-9_-]+)$/);
if (fromLink) {
  sessionStorage.setItem(TOKEN_KEY, fromLink[1]);
  history.replaceState(null, "", location.pathname + location.search);
}

route();
