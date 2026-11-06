"use strict";

const $ = (id) => document.getElementById(id);

const EXPORT_LIMIT = 10000;

const state = {
  sessionId: null,
  summary: null,
  q: "",
  type: "all",
  origin: "",
  offset: 0,
  limit: 100,
  matched: 0,
  // selected holds every entry index chosen for export, including rows on
  // other pages. anchor is the Shift+click origin. focusIndex is the row
  // whose detail panel is open.
  selected: new Set(),
  anchor: null,
  focusIndex: null,
  matchIndexes: [],
  matchPos: new Map(),
};

const TYPE_LABELS = {
  "ls-item": "Local Storage",
  "ls-meta": "LS metadata",
  "ss-item": "Session Storage",
  "ss-namespace": "SS namespace",
  "ss-meta": "SS metadata",
  version: "Version",
  "idb-data": "IndexedDB",
  "idb-index": "IDB index",
  "idb-meta": "IDB metadata",
  other: "Record",
};

// ---------- view helpers ----------

function show(view) {
  for (const id of ["emptyState", "loadingState", "errorState", "browser"]) {
    $(id).classList.toggle("hidden", id !== view);
  }
}

function showError(message) {
  $("errorText").textContent = message;
  show("errorState");
}

function fmtBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

async function api(url, opts) {
  const res = await fetch(url, opts);
  let body;
  try {
    body = await res.json();
  } catch {
    throw new Error(`Server error (${res.status})`);
  }
  if (!res.ok) throw new Error(body.error || `Server error (${res.status})`);
  return body;
}

// ---------- opening databases ----------

async function openWithFiles(fileList) {
  if (!fileList.length) return;
  show("loadingState");
  $("loadingText").textContent = `Uploading and parsing ${fileList.length} file(s)…`;
  const form = new FormData();
  for (const f of fileList) form.append("files", f, f.name);
  try {
    const summary = await api("/api/upload", { method: "POST", body: form });
    onOpened(summary);
  } catch (err) {
    showError(err.message);
  }
}

async function openPath(path) {
  show("loadingState");
  $("loadingText").textContent = `Opening ${path}…`;
  try {
    const summary = await api("/api/open", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path }),
    });
    onOpened(summary);
  } catch (err) {
    showError(err.message);
  }
}

function onOpened(summary) {
  state.sessionId = summary.id;
  state.summary = summary;
  state.q = "";
  state.type = "all";
  state.origin = "";
  state.offset = 0;
  state.selected = new Set();
  state.anchor = null;
  state.focusIndex = null;
  state.matchIndexes = [];
  state.matchPos = new Map();
  hideNote();
  updateSelectionBar();
  $("searchInput").value = "";
  $("typeFilter").value = "all";
  closeDetail();
  renderSummary();
  show("browser");
  loadRows();
}

// ---------- summary sidebar ----------

function renderSummary() {
  const s = state.summary;
  $("summaryCard").classList.remove("hidden");
  $("sumSource").textContent = s.source;
  $("sumMode").textContent = s.mode === "db" ? "database scan" : "raw table scan";
  const files = s.files || [];
  $("sumFiles").textContent = files.length <= 4 ? files.join(", ") : `${files.length} files parsed`;
  $("sumTotal").textContent = s.total.toLocaleString();
  $("sumTruncated").classList.toggle("hidden", !s.truncated);

  const badge = $("dbBadge");
  badge.textContent = `${s.source} — ${s.total.toLocaleString()} records`;
  badge.classList.remove("hidden");

  const chips = $("typeChips");
  chips.innerHTML = "";
  const entries = Object.entries(s.types || {}).sort((a, b) => b[1] - a[1]);
  for (const [type, count] of entries) {
    const chip = document.createElement("button");
    chip.className = "chip" + (state.type === type ? " active" : "");
    chip.innerHTML = `${TYPE_LABELS[type] || type}<span class="n">${count.toLocaleString()}</span>`;
    chip.onclick = () => {
      state.type = state.type === type ? "all" : type;
      $("typeFilter").value = state.type;
      state.offset = 0;
      renderSummary();
      loadRows();
    };
    chips.appendChild(chip);
  }

  const origins = s.origins || [];
  $("originsWrap").classList.toggle("hidden", origins.length === 0);
  const list = $("originList");
  list.innerHTML = "";
  for (const { origin, count } of origins) {
    const item = document.createElement("div");
    item.className = "origin-item" + (state.origin === origin ? " active" : "");
    item.innerHTML = `<span class="o" title="${escapeHTML(origin)}">${escapeHTML(origin)}</span><span class="n">${count}</span>`;
    item.onclick = () => {
      state.origin = state.origin === origin ? "" : origin;
      state.offset = 0;
      renderSummary();
      loadRows();
    };
    list.appendChild(item);
  }
}

function escapeHTML(s) {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// ---------- row listing ----------

async function loadRows() {
  if (!state.sessionId) return;
  const params = new URLSearchParams({
    q: state.q,
    type: state.type,
    origin: state.origin,
    offset: String(state.offset),
    limit: String(state.limit),
    ids: "1",
  });
  let res;
  try {
    res = await api(`/api/session/${state.sessionId}/entries?${params}`);
  } catch (err) {
    showError(err.message);
    return;
  }
  state.matched = res.matched;
  state.matchIndexes = Array.isArray(res.indexes) ? res.indexes : res.rows.map((row) => row.index);
  state.matchPos = new Map(state.matchIndexes.map((id, i) => [id, i]));

  const tbody = $("rows");
  tbody.innerHTML = "";
  for (const row of res.rows) {
    const tr = document.createElement("tr");
    tr.dataset.index = String(row.index);
    if (row.deleted) tr.classList.add("is-deleted");
    if (state.selected.has(row.index)) tr.classList.add("selected");
    if (row.index === state.focusIndex) tr.classList.add("focused");
    tr.innerHTML = `
      <td class="col-type"><span class="badge t-${row.type}">${TYPE_LABELS[row.type] || row.type}</span>${row.deleted ? ' <span class="badge deleted">del</span>' : ""}</td>
      <td title="${escapeHTML(row.keyDisplay)}">${escapeHTML(row.keyDisplay)}<span class="enc-tag">${row.keyEnc}</span></td>
      <td title="${escapeHTML(row.valuePreview)}">${escapeHTML(row.valuePreview) || '<span class="enc-tag">(empty)</span>'}<span class="enc-tag">${row.valueEnc}</span></td>
      <td class="size">${fmtBytes(row.valueSize)}</td>`;
    tr.addEventListener("mousedown", (e) => {
      // Keep Shift+click from highlighting the row text as a selection.
      if (e.shiftKey) e.preventDefault();
    });
    tr.addEventListener("click", (e) => onRowClick(row.index, e));
    tbody.appendChild(tr);
  }

  $("noMatches").classList.toggle("hidden", res.rows.length > 0);

  const from = res.matched === 0 ? 0 : state.offset + 1;
  const to = state.offset + res.rows.length;
  $("matchCount").textContent =
    res.matched === res.total
      ? `${res.total.toLocaleString()} records`
      : `${res.matched.toLocaleString()} of ${res.total.toLocaleString()} records`;
  $("pageInfo").textContent = `${from.toLocaleString()}–${to.toLocaleString()}`;
  $("prevBtn").disabled = state.offset === 0;
  $("nextBtn").disabled = to >= res.matched;
}

// ---------- selection ----------

function rangeBetween(from, to) {
  const a = state.matchPos.get(from);
  const b = state.matchPos.get(to);
  if (a === undefined || b === undefined) return [to];
  const lo = Math.min(a, b);
  const hi = Math.max(a, b);
  return state.matchIndexes.slice(lo, hi + 1);
}

function onRowClick(index, e) {
  const toggle = e.ctrlKey || e.metaKey;
  const canRange = e.shiftKey && state.anchor !== null && state.matchPos.has(state.anchor);
  if (canRange) {
    const range = rangeBetween(state.anchor, index);
    if (toggle) {
      for (const i of range) state.selected.add(i);
    } else {
      state.selected = new Set(range);
    }
  } else if (toggle) {
    if (state.selected.has(index)) state.selected.delete(index);
    else state.selected.add(index);
    state.anchor = index;
  } else {
    state.selected = new Set([index]);
    state.anchor = index;
  }
  state.focusIndex = index;
  paintSelection();
  showDetail(index);
}

function paintSelection() {
  document.querySelectorAll("#rows tr").forEach((tr) => {
    const index = Number(tr.dataset.index);
    tr.classList.toggle("selected", state.selected.has(index));
    tr.classList.toggle("focused", index === state.focusIndex);
  });
  updateSelectionBar();
}

function updateSelectionBar() {
  const n = state.selected.size;
  $("selCount").textContent = n === 0 ? "None selected" : `${n.toLocaleString()} selected`;
  $("exportBtn").disabled = n === 0 || n > EXPORT_LIMIT;
  $("clearSelBtn").disabled = n === 0;
  const kind = $("exportNote").dataset.kind;
  if (n > EXPORT_LIMIT) {
    showNote(`A report can include at most ${EXPORT_LIMIT.toLocaleString()} records.`, "bad");
  } else if (kind === "limit" || kind === "ok" || kind === "bad") {
    hideNote();
  }
}

function orderedSelection() {
  return [...state.selected].sort((a, b) => {
    const pa = state.matchPos.has(a) ? state.matchPos.get(a) : state.matchIndexes.length + a;
    const pb = state.matchPos.has(b) ? state.matchPos.get(b) : state.matchIndexes.length + b;
    return pa - pb;
  });
}

function showNote(text, kind) {
  const el = $("exportNote");
  el.dataset.kind = kind === "bad" ? (text.startsWith("A report can include") ? "limit" : "bad") : kind || "";
  el.textContent = text;
  el.classList.remove("hidden", "bad");
  if (kind === "bad") el.classList.add("bad");
}

function hideNote() {
  const el = $("exportNote");
  el.dataset.kind = "";
  el.textContent = "";
  el.classList.add("hidden");
  el.classList.remove("bad");
}

function filenameFromDisposition(header) {
  if (!header) return "leveldb-report.html";
  const quoted = /filename="([^"]+)"/.exec(header);
  if (quoted) return quoted[1];
  const plain = /filename=([^;]+)/.exec(header);
  return plain ? plain[1].trim() : "leveldb-report.html";
}

async function saveReportFile(blob, name) {
  if (typeof window.showSaveFilePicker === "function") {
    try {
      const handle = await window.showSaveFilePicker({
        suggestedName: name,
        types: [{ description: "HTML report", accept: { "text/html": [".html"] } }],
      });
      const writable = await handle.createWritable();
      await writable.write(blob);
      await writable.close();
      return "saved";
    } catch (err) {
      if (err && err.name === "AbortError") return "cancelled";
      // The picker can be unavailable inside the embedded window. Fall through.
    }
  }
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 4000);
  return "downloaded";
}

async function exportReport() {
  const indexes = orderedSelection();
  if (!indexes.length || indexes.length > EXPORT_LIMIT) return;
  $("exportBtn").disabled = true;
  $("exportBtn").textContent = "Exporting…";
  showNote("Building report…", "");
  try {
    const res = await fetch(`/api/session/${state.sessionId}/report`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ indexes }),
    });
    if (!res.ok) {
      let msg = `Server error (${res.status})`;
      try {
        msg = (await res.json()).error || msg;
      } catch { /* keep the status text */ }
      throw new Error(msg);
    }
    const blob = await res.blob();
    const name = filenameFromDisposition(res.headers.get("Content-Disposition"));
    const how = await saveReportFile(blob, name);
    if (how === "cancelled") {
      hideNote();
      return;
    }
    const verb = how === "saved" ? "Saved" : "Downloaded";
    showNote(`${verb} ${name} (${indexes.length.toLocaleString()} record${indexes.length === 1 ? "" : "s"})`, "ok");
  } catch (err) {
    showNote(err.message, "bad");
  } finally {
    $("exportBtn").textContent = "Export report";
    $("exportBtn").disabled = state.selected.size === 0 || state.selected.size > EXPORT_LIMIT;
  }
}

// ---------- detail panel ----------

async function showDetail(index) {
  let d;
  try {
    d = await api(`/api/session/${state.sessionId}/entry?i=${index}`);
  } catch (err) {
    showError(err.message);
    return;
  }

  $("detail").classList.remove("hidden");
  const typeBadge = $("detailType");
  typeBadge.textContent = d.typeLabel;
  typeBadge.className = `badge t-${d.type}`;
  $("detailDeleted").classList.toggle("hidden", !d.deleted);
  $("detailOrigin").textContent = d.origin || "—";
  $("detailStorageKey").textContent = d.storageKey || "—";
  $("detailSize").textContent = fmtBytes(d.value.size) + (d.truncated ? " (display truncated)" : "");

  buildTabs("keyTabs", "keyView", d.key);
  buildTabs("valueTabs", "valueView", d.value);
}

function buildTabs(tabsId, viewId, views) {
  const tabs = [];
  tabs.push({ label: `Decoded (${views.bestEnc})`, text: views.best });
  if (views.json) tabs.push({ label: "JSON", text: views.json });
  tabs.push({ label: "UTF-8", text: views.utf8 });
  tabs.push({ label: "UTF-16LE", text: views.utf16le });
  tabs.push({ label: "Latin-1", text: views.latin1 });
  tabs.push({ label: "Hex", text: views.hexDump });

  const wrap = $(tabsId);
  wrap.innerHTML = "";
  const viewer = $(viewId);
  const activate = (i) => {
    wrap.querySelectorAll(".tab").forEach((el, j) => el.classList.toggle("active", i === j));
    viewer.textContent = tabs[i].text === "" ? "(empty)" : tabs[i].text;
  };
  tabs.forEach((t, i) => {
    const btn = document.createElement("button");
    btn.className = "tab";
    btn.textContent = t.label;
    btn.onclick = () => activate(i);
    wrap.appendChild(btn);
  });
  activate(0);
}

function closeDetail() {
  $("detail").classList.add("hidden");
  state.focusIndex = null;
  paintSelection();
}

// ---------- samples ----------

async function loadSamples() {
  try {
    const res = await api("/api/samples");
    const samples = res.samples || [];
    if (!samples.length) return;
    $("samples").classList.remove("hidden");
    const list = $("sampleList");
    list.innerHTML = "";
    for (const s of samples) {
      const btn = document.createElement("button");
      btn.className = "sample-btn";
      btn.textContent = s.name;
      btn.onclick = () => openPath(s.path);
      list.appendChild(btn);
    }
  } catch {
    /* samples are optional */
  }
}

// ---------- wiring ----------

$("folderInput").addEventListener("change", (e) => openWithFiles([...e.target.files]));
$("filesInput").addEventListener("change", (e) => openWithFiles([...e.target.files]));
$("pathOpenBtn").addEventListener("click", () => {
  const p = $("pathInput").value.trim();
  if (p) openPath(p);
});
$("pathInput").addEventListener("keydown", (e) => {
  if (e.key === "Enter") $("pathOpenBtn").click();
});
$("errorDismiss").addEventListener("click", () => {
  show(state.sessionId ? "browser" : "emptyState");
});
$("detailClose").addEventListener("click", closeDetail);
$("clearSelBtn").addEventListener("click", () => {
  state.selected.clear();
  state.anchor = null;
  paintSelection();
});
$("exportBtn").addEventListener("click", exportReport);

let searchTimer;
$("searchInput").addEventListener("input", (e) => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    state.q = e.target.value;
    state.offset = 0;
    loadRows();
  }, 200);
});
$("typeFilter").addEventListener("change", (e) => {
  state.type = e.target.value;
  state.offset = 0;
  renderSummary();
  loadRows();
});
$("prevBtn").addEventListener("click", () => {
  state.offset = Math.max(0, state.offset - state.limit);
  loadRows();
});
$("nextBtn").addEventListener("click", () => {
  if (state.offset + state.limit < state.matched) {
    state.offset += state.limit;
    loadRows();
  }
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeDetail();
  if ((e.ctrlKey || e.metaKey) && (e.key === "a" || e.key === "A")) {
    const tag = document.activeElement && document.activeElement.tagName;
    if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;
    if (!state.sessionId || !state.matchIndexes.length) return;
    e.preventDefault();
    state.selected = new Set(state.matchIndexes);
    if (state.anchor === null) state.anchor = state.matchIndexes[0];
    paintSelection();
  }
});

loadSamples();
