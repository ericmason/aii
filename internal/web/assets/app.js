const $ = (s) => document.querySelector(s);
const resultsEl = $("#results");
const emptyEl = $("#empty");
const previewEl = $("#preview");
const previewHead = $("#preview-head");
const previewBody = $("#preview-body");

function esc(s) { return (s || "").replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c])); }

function highlight(s) { return esc(s).replaceAll("«", "<mark>").replaceAll("»", "</mark>"); }

function fmtDate(unix) {
  if (!unix) return "";
  const d = new Date(unix * 1000);
  const now = new Date();
  const diff = (now - d) / 1000;
  if (diff < 60) return "just now";
  if (diff < 3600) return `${Math.floor(diff/60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff/3600)}h ago`;
  if (diff < 86400 * 14) return `${Math.floor(diff/86400)}d ago`;
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric", year: d.getFullYear() !== now.getFullYear() ? "numeric" : undefined });
}

function fmtDateAbs(unix) {
  if (!unix) return "";
  return new Date(unix * 1000).toLocaleString();
}

function agentLabel(a) {
  if (a === "claude_code") return "Claude Code";
  if (a === "codex") return "Codex";
  if (a === "cursor") return "Cursor";
  return a;
}

function roleLabel(role) {
  return ({ user: "user", assistant: "assistant", thinking: "reasoning", tool: "tool" }[role]) || role;
}

// --- stats ------------------------------------------------------------

async function loadStats() {
  try {
    const r = await fetch("/api/stats");
    const data = await r.json();
    if (!data || !data.length) {
      $("#stats").innerHTML = `<span class="chip">no data yet — run <code>aii index</code></span>`;
      return;
    }
    const total = data.reduce((a, s) => ({ sessions: a.sessions + s.Sessions, messages: a.messages + s.Messages }), { sessions: 0, messages: 0 });
    const chips = [
      `<span class="chip"><strong>${total.sessions.toLocaleString()}</strong> sessions</span>`,
      `<span class="chip"><strong>${total.messages.toLocaleString()}</strong> messages</span>`,
      ...data.map(s => `<span class="chip">${agentLabel(s.Agent)} · ${s.Sessions.toLocaleString()}</span>`),
    ];
    $("#stats").innerHTML = chips.join("");
  } catch (e) { $("#stats").textContent = "stats unavailable"; }
}
loadStats();

// --- search -----------------------------------------------------------

let ctl = null;
let debounceTimer = null;

const progressEl = $("#progress");
const searchFieldEl = $("#search-field");

// showBusy(true) turns on the header progress bar and input spinner
// immediately. We call it on every keystroke so the UI never feels frozen
// while we debounce or the first request warms caches.
function showBusy(on) {
  if (on) {
    progressEl.classList.add("active");
    searchFieldEl.classList.add("loading");
    resultsEl.classList.add("stale");
  } else {
    progressEl.classList.remove("active");
    searchFieldEl.classList.remove("loading");
    resultsEl.classList.remove("stale");
  }
}

function renderResults(hits) {
  if (!hits) hits = [];
  if (!hits.length) {
    resultsEl.innerHTML = `<p class="empty">no matches</p>`;
    emptyEl.hidden = true;
    return;
  }
  const count = hits.length;
  const countLabel = `<p class="result-count">${count} session${count === 1 ? "" : "s"} · sorted by relevance</p>`;
  resultsEl.innerHTML = countLabel + hits.map((h, i) => {
    const hidden = (h.match_count || 0) - (h.excerpts ? h.excerpts.length : 0);
    const moreLabel = hidden > 0 ? `<span class="more">+${hidden} more</span>` : "";
    const excerpts = (h.excerpts || []).map(e => `
      <li data-role="${esc(e.role)}">
        <span class="bar"></span>
        <span class="role">${esc(roleLabel(e.role))}</span>
        <span class="text">${highlight(e.snippet)}</span>
      </li>`).join("");
    const summary = h.summary ? `<div class="r-summary">${esc(h.summary)}</div>` : "";
    const workspace = h.workspace ? `<div class="r-workspace">${esc(h.workspace)}</div>` : "";
    const firstOrd = (h.excerpts && h.excerpts[0] && h.excerpts[0].ordinal) || 0;
    const title = esc(h.title || h.session_uid);
    const uid = esc(h.session_uid.slice(0, 8));
    return `
      <article class="result" data-uid="${esc(h.session_uid)}" data-ordinal="${firstOrd}">
        <div class="rank">${i + 1}</div>
        <div>
          <header class="r-head">
            <span class="agent ${esc(h.agent)}">${esc(agentLabel(h.agent))}</span>
            <span title="${fmtDateAbs(h.started_at)}">${fmtDate(h.started_at)}</span>
            <span class="sep">·</span>
            <span class="uid">${uid}</span>
            ${moreLabel}
          </header>
          <h2 class="r-title">${title}</h2>
          ${workspace}
          ${summary}
          <ul class="excerpts">${excerpts}</ul>
        </div>
      </article>`;
  }).join("");
  emptyEl.hidden = true;
  for (const el of resultsEl.querySelectorAll(".result")) {
    el.addEventListener("click", () => openSession(el.dataset.uid, Number(el.dataset.ordinal || 0)));
  }
}

function renderSessions(items, total) {
  emptyEl.hidden = true;
  if (!items || !items.length) {
    resultsEl.innerHTML = `<p class="empty">no sessions in this window</p>`;
    $("#browse-hint").hidden = true;
    return;
  }
  const showing = items.length === total ? `${items.length} sessions` : `${items.length} of ${total.toLocaleString()} sessions`;
  const hint = $("#browse-hint");
  hint.textContent = "browsing — type above to search";
  hint.hidden = false;
  const countLabel = `<p class="result-count">${showing} · newest first</p>`;
  resultsEl.innerHTML = countLabel + items.map((s) => {
    const summary = s.summary ? `<div class="r-summary">${esc(s.summary)}</div>` : "";
    const workspace = s.workspace ? `<div class="r-workspace">${esc(s.workspace)}</div>` : "";
    const title = esc(s.title || s.uid);
    const uid = esc((s.uid || "").slice(0, 8));
    const msgs = `<span class="more">${s.message_count} msg${s.message_count === 1 ? "" : "s"}</span>`;
    return `
      <article class="result session-row" data-uid="${esc(s.uid)}">
        <div class="rank">·</div>
        <div>
          <header class="r-head">
            <span class="agent ${esc(s.agent)}">${esc(agentLabel(s.agent))}</span>
            <span title="${fmtDateAbs(s.started_at)}">${fmtDate(s.started_at)}</span>
            <span class="sep">·</span>
            <span class="uid">${uid}</span>
            ${msgs}
          </header>
          <h2 class="r-title">${title}</h2>
          ${workspace}
          ${summary}
        </div>
      </article>`;
  }).join("");
  for (const el of resultsEl.querySelectorAll(".result")) {
    el.addEventListener("click", () => openSession(el.dataset.uid, 0));
  }
}

async function runBrowse() {
  if (ctl) ctl.abort();
  ctl = new AbortController();
  const params = new URLSearchParams({
    agent: $("#agent").value,
    since: $("#since").value,
    limit: "100",
  });
  showBusy(true);
  try {
    const r = await fetch("/api/sessions?" + params, { signal: ctl.signal });
    const data = await r.json();
    renderSessions(data.items || [], data.total || 0);
    showBusy(false);
  } catch (e) {
    if (e.name !== "AbortError") {
      console.error(e);
      showBusy(false);
    }
  }
}

async function runSearch() {
  const q = $("#q").value.trim();
  if (!q) {
    // Empty query → browse mode (recent sessions filtered by since/agent).
    return runBrowse();
  }
  $("#browse-hint").hidden = true;
  if (ctl) ctl.abort();
  ctl = new AbortController();
  const params = new URLSearchParams({
    q,
    agent: $("#agent").value,
    since: $("#since").value,
    limit: "50",
  });
  showBusy(true);
  try {
    const r = await fetch("/api/search?" + params, { signal: ctl.signal });
    const hits = await r.json();
    renderResults(hits);
    showBusy(false);
  } catch (e) {
    if (e.name !== "AbortError") {
      console.error(e);
      showBusy(false);
    }
    // On AbortError we leave the spinner on — a fresh search is starting.
  }
}

$("#search-form").addEventListener("submit", (e) => { e.preventDefault(); runSearch(); });
$("#q").addEventListener("input", () => {
  clearTimeout(debounceTimer);
  // Immediate visual feedback even before debounce fires or the first
  // request starts — otherwise the first cold search feels unresponsive.
  if ($("#q").value.trim()) showBusy(true);
  else showBusy(false);
  debounceTimer = setTimeout(runSearch, 180);
});
$("#agent").addEventListener("change", runSearch);
$("#since").addEventListener("change", runSearch);
$("#q").addEventListener("keydown", (e) => {
  if (e.key === "Escape") {
    $("#q").value = "";
    runBrowse();
  }
});

// Default landing view = browse, not "start typing".
emptyEl.hidden = true;
runBrowse();

// --- session preview --------------------------------------------------

async function openSession(uid, scrollOrdinal) {
  try {
    const r = await fetch("/api/session/" + encodeURIComponent(uid));
    if (!r.ok) return;
    const s = await r.json();
    const summaryHTML = s.summary
      ? `<details class="preview-summary"><summary>summary</summary><pre>${esc(s.summary)}</pre></details>`
      : "";
    previewHead.innerHTML = `
      <div class="meta">
        <span class="agent ${esc(s.agent)}">${esc(agentLabel(s.agent))}</span>
        <span title="${fmtDateAbs(s.started_at)}">${fmtDate(s.started_at)}</span>
        ${s.ended_at && s.ended_at !== s.started_at ? `<span class="sep">→</span><span title="${fmtDateAbs(s.ended_at)}">${fmtDate(s.ended_at)}</span>` : ""}
        <span class="sep">·</span>
        <span class="uid">${esc(s.uid)}</span>
      </div>
      <h1 class="title">${esc(s.title || s.uid)}</h1>
      ${s.workspace ? `<div class="r-workspace">${esc(s.workspace)}</div>` : ""}
      ${summaryHTML}`;
    previewBody.innerHTML = (s.messages || []).map(m => `
      <section class="msg ${esc(m.role)}" data-ordinal="${m.ordinal}">
        <header>
          <span>${esc(roleLabel(m.role))}</span>
          <span class="when">${fmtDate(m.ts)}</span>
        </header>
        <pre>${esc(m.content)}</pre>
      </section>`).join("");
    previewEl.hidden = false;
    document.body.style.overflow = "hidden";
    if (scrollOrdinal) {
      const tgt = previewBody.querySelector(`[data-ordinal="${scrollOrdinal}"]`);
      if (tgt) setTimeout(() => tgt.scrollIntoView({ block: "center" }), 40);
    }
  } catch (e) { console.error(e); }
}

function closePreview() {
  previewEl.hidden = true;
  document.body.style.overflow = "";
}
$("#close-preview").addEventListener("click", closePreview);
previewEl.addEventListener("click", (e) => { if (e.target === previewEl) closePreview(); });
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closePreview(); });
