"use strict";
const $ = (id) => document.getElementById(id);
let current = null;
let audioOpts = { apps: [], devices: [] };

// For the log: which page sent a request and what the user last did there.
const tabId = Math.random().toString(36).slice(2, 8);
let lastInput = { what: "none", at: 0 };
addEventListener("pointerdown", () => (lastInput = { what: "pointer", at: Date.now() }), true);
addEventListener("keydown", (e) => (lastInput = { what: "key:" + e.key, at: Date.now() }), true);
function via() {
  const ago = Date.now() - lastInput.at;
  return lastInput.what + (lastInput.at ? " " + Math.round(ago / 1000) + "s ago" : "") + (document.hidden ? " hidden" : "");
}

async function post(path, body) {
  const r = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Blite": "1", "X-Blite-Tab": tabId, "X-Blite-Via": via() },
    body: JSON.stringify(body || {}),
  });
  const j = await r.json().catch(() => ({ error: "HTTP " + r.status }));
  if (!r.ok) throw new Error(j.error || "HTTP " + r.status);
  return j;
}

function showError(msg) {
  const e = $("error");
  e.textContent = msg || "";
  e.hidden = !msg;
}

function busy(btn, on) { btn.disabled = on; }

async function refresh() {
  const r = await fetch("/api/state");
  if (!r.ok) { $("status").textContent = "disconnected from Blite"; return; }
  render(await r.json());
}

function render(st) {
  current = st;
  const s = st.session;
  $("version").textContent = st.version ? "v" + st.version : "";
  $("updateNudge").hidden = !(s && s.updateRecommended);
  renderUpdate(st.update, !!s);
  $("autostartRow").hidden = st.autostart === undefined;
  renderLogs(st.logs);
  $("autostart").checked = !!st.autostart;
  $("lobby").hidden = !!s;
  $("call").hidden = !s;
  $("status").textContent = s ? "in a session" : "not in a session";
  if (st.error) showError(st.error);
  if (document.activeElement !== $("myName")) $("myName").value = st.name || "";
  $("myTag").textContent = st.tag ? "#" + st.tag : "";
  renderReconnecting(s);
  if (!s) return;
  $("room").textContent = s.room;
  const others = (s.members || []).filter((m) => m.state === "connected");
  $("safetyRow").hidden = others.length === 0;
  $("safety").textContent = s.safety || "";
  for (const el of document.querySelectorAll("input[name=src]")) {
    el.checked = el.value === (s.source || "none");
  }
  $("shareHint").hidden = !!s.source && s.source !== "none";
  $("encoder").textContent = s.encoder ? "Encoding with " + s.encoder : "";
  $("srcNote").hidden = !s.sourceNote;
  $("srcNote").textContent = s.sourceNote || "";
  $("resNote").hidden = !s.resolutionNote;
  $("resNote").textContent = s.resolutionNote || "";
  renderAudio(s);
  renderRelay(s);
  if (document.activeElement !== $("resolution")) $("resolution").value = s.resolution || "720p";
  renderLayout(st.layout || s.layout);
  const tb = $("members").querySelector("tbody");
  tb.replaceChildren();
  const ms = s.members || [];
  $("alone").hidden = ms.length > 0;
  $("members").hidden = ms.length === 0;
  const bad = ms.filter((m) => m.state === "untrusted").map((m) => m.name);
  $("untrusted").hidden = bad.length === 0;
  $("untrusted").textContent = bad.length
    ? "Refused to connect to " + bad.join(", ") + ": their encryption keys didn't match their identity. The connection may be intercepted."
    : "";
  for (const m of ms) {
    const tr = document.createElement("tr");
    const cells = [nameWithTag(m), null, m.layer || "", m.frames ? String(m.frames) : "", m.pair || ""];
    cells.forEach((v, i) => {
      const td = document.createElement("td");
      if (i === 1) {
        const dot = document.createElement("span");
        dot.className = "dot " + m.state;
        td.append(dot, m.state);
      } else if (i === 0) {
        td.append(v);
        if (m.owner) {
          const b = document.createElement("span");
          b.className = "badge";
          b.textContent = "owner";
          td.append(b);
        }
        if (m.keyChanged) {
          const w = document.createElement("div");
          w.className = "warn";
          w.textContent = "new device or key since you last saw this name";
          td.append(w);
        }
      } else td.textContent = v;
      if (i === 0 && m.client) {
        const c = document.createElement("div");
        c.className = "muted small";
        c.textContent = m.client;
        td.append(c);
      }
      if (i === 0 && m.recv) {
        const r = document.createElement("div");
        r.className = "muted small";
        r.textContent = "you get " + (m.recv === 2160 ? "4K" : m.recv + "p");
        td.append(r);
      }
      if (i === 0 && m.notice) {
        const n = document.createElement("div");
        n.className = "warn";
        n.textContent = m.notice;
        td.append(n);
      }
      tr.append(td);
    });
    tr.append(peerControls(m));
    tr.append(ownerControls(m, s.isOwner));
    tb.append(tr);
  }
  $("ownerCol").hidden = !s.isOwner;
  const owner = ms.find((m) => m.owner);
  $("ownerLine").textContent = s.isOwner
    ? "You own this room: you can make someone else the owner, or remove people."
    : owner ? owner.name + (owner.tag ? "#" + owner.tag : "") + " owns this room." : "";
  $("idKey").textContent = s.idKey || "";
}

// nameWithTag renders name#tag, the tag muted: the tag comes from the
// member's identity key, so it tells two people with the same name apart.
function nameWithTag(m) {
  const f = document.createDocumentFragment();
  f.append(m.name);
  if (m.tag) {
    const t = document.createElement("span");
    t.className = "tag";
    t.textContent = "#" + m.tag;
    f.append(t);
  }
  return f;
}

// ownerControls (room owner only): hand the room over, or remove someone,
// for now (disconnect) or for good (ban).
function ownerControls(m, isOwner) {
  const td = document.createElement("td");
  td.hidden = !isOwner;
  if (!isOwner) return td;
  const btn = (label, title, f) => {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = label;
    b.title = title;
    b.onclick = () => { busy(b, true); f().catch(showError).finally(() => busy(b, false)); };
    td.append(b, " ");
  };
  if (m.state === "connected") {
    btn("Make owner", "Give " + m.name + " ownership of the room", () =>
      confirm("Make " + m.name + " the owner? You can't take it back yourself.")
        ? post("/api/transfer", { id: m.id }) : Promise.resolve());
  }
  btn("Disconnect", "Remove " + m.name + " from the call; they can rejoin with the link", () =>
    confirm("Disconnect " + m.name + "? They can rejoin with the link.")
      ? post("/api/kick", { id: m.id, ban: false }) : Promise.resolve());
  btn("Ban", "Remove " + m.name + " and keep them out of this room", () =>
    confirm("Ban " + m.name + "? They won't be able to rejoin this room.")
      ? post("/api/kick", { id: m.id, ban: true }) : Promise.resolve());
  return td;
}

// renderReconnecting shows a banner while our link to the server, or to a
// member, is being restored; the call keeps playing underneath.
let reconnTimer = null;
function renderReconnecting(s) {
  const peers = s ? (s.members || []).filter((m) => m.state === "reconnecting").map((m) => m.name) : [];
  const server = !!(s && s.reconnecting);
  const show = () => {
    const parts = [];
    if (server) {
      const left = Math.max(0, Math.round((Date.parse(s.giveUpAt) - Date.now()) / 1000));
      parts.push("Reconnecting to the server... giving up in " + left + " s. The call keeps going meanwhile.");
    }
    if (peers.length) parts.push("Reconnecting to " + peers.join(", ") + "...");
    $("reconnText").textContent = parts.join(" ");
  };
  $("reconnBanner").hidden = !server && peers.length === 0;
  clearInterval(reconnTimer);
  reconnTimer = null;
  if (server) reconnTimer = setInterval(show, 1000);
  show();
}

// renderRelay shows the relay queue (active relay first) and our button to
// volunteer, withdraw from the queue, or stop relaying.
function renderRelay(s) {
  const q = s.relay || [];
  const me = q.findIndex((e) => e.me);
  $("relayMode").textContent = q.length === 0
    ? "Everyone sends their video to everyone (full mesh). A member with a fast upload can volunteer as the relay: the others then send their video only to the relay, and it forwards it."
    : me === 0
      ? "You are the relay: everyone's video goes through you."
      : "Video goes through " + q[0].name + ".";
  $("relayQueue").replaceChildren(...q.map((e, i) => {
    const li = document.createElement("li");
    li.textContent = (e.me ? "You" : e.name) + (i === 0 ? " (relaying)" : " (waiting)");
    return li;
  }));
  $("relayQueue").hidden = q.length === 0;
  $("relayBtn").textContent = me === 0 ? "Stop relaying" : me > 0 ? "Leave the relay queue" : q.length ? "Volunteer as next relay" : "Volunteer as relay";
  $("relayBtn").onclick = () => post("/api/relay", { on: me < 0 }).catch(showError);
  $("relayRefused").hidden = !s.relayRefused;
}

// peerControls returns the cells for our viewer settings of one peer: the best
// video quality we ask them for, and their playback volume.
function peerControls(m) {
  const frag = document.createDocumentFragment();
  const set = (patch) =>
    post("/api/peer", { id: m.id, quality: m.quality, volume: m.volume, muted: !!m.muted, ...patch }).catch(showError);
  const q = document.createElement("select");
  for (const [v, label] of [["auto", "Best"], ["low", "Low"], ["off", "Off (audio only)"]]) {
    q.append(new Option(label, v, false, v === m.quality));
  }
  q.onchange = () => set({ quality: q.value });
  const vol = document.createElement("input");
  vol.type = "range";
  vol.min = "0";
  vol.max = "200";
  vol.value = String(Math.round((m.volume ?? 1) * 100));
  vol.title = vol.value + "%";
  vol.onchange = () => set({ volume: Number(vol.value) / 100 });
  const mute = document.createElement("label");
  const cb = document.createElement("input");
  cb.type = "checkbox";
  cb.checked = !!m.muted;
  cb.onchange = () => set({ muted: cb.checked });
  mute.append(cb, "mute");
  for (const els of [[q], [vol, mute]]) {
    const td = document.createElement("td");
    td.append(...els);
    frag.append(td);
  }
  return frag;
}

// renderUpdate shows Molt update state: available build, problems, and the
// outcome of the last update. Installing restarts Blite, so not during a call.
function renderUpdate(u, inSession) {
  $("updateBox").hidden = !u;
  if (!u) return;
  $("updResult").hidden = !u.result;
  $("updResult").querySelector("span").textContent = u.result || "";
  $("updAvail").hidden = !u.available;
  $("updAvail").querySelector("strong").textContent = u.available
    ? (u.mandatory ? "Required update: Blite " : "Update available: Blite ") + u.available
    : "";
  $("updNotes").textContent = u.notes || "";
  $("updInstall").hidden = !u.canApply;
  $("updInstall").disabled = inSession;
  $("updInstall").title = inSession ? "Leave the session first" : "";
  $("updProblem").hidden = !u.problem;
  $("updProblem").textContent = u.problem || "";
  let status = "Channel: " + u.channel + ".";
  if (u.checking) status += " Checking...";
  else if (u.checked && !u.available) status += " Up to date (checked " + u.checked + " UTC).";
  if (u.available && !u.canApply) status += " This copy can't update itself; install the new release package.";
  $("updStatus").textContent = status;
}

async function setSource(kind) {
  showError("");
  try { render((await post("/api/source", { kind })).state); }
  catch (e) { showError(e.message); refresh(); }
}

// loadSources lists monitors and windows as more video source choices.
async function loadSources() {
  const r = await fetch("/api/sources");
  if (!r.ok) return;
  const j = await r.json();
  $("sourcesErr").textContent = j.error ? "Can't list windows: " + j.error : "";
  const box = $("targets");
  box.replaceChildren();
  for (const t of j.targets || []) {
    const lab = document.createElement("label");
    const rb = document.createElement("input");
    rb.type = "radio";
    rb.name = "src";
    rb.value = t.source;
    rb.checked = !!(current && current.session && current.session.source === t.source);
    rb.onchange = () => setSource(t.source);
    lab.append(rb, (t.kind === "monitor" ? "Monitor: " : "Window: ") + t.name);
    box.append(lab);
  }
}

async function loadAudio() {
  const r = await fetch("/api/audio");
  if (!r.ok) return;
  audioOpts = await r.json();
  $("audioErr").textContent = audioOpts.error ? "Audio capture unavailable: " + audioOpts.error : "";
  if (current && current.session) renderAudio(current.session);
}

function renderAudio(s) {
  const a = s.audio || { mode: "none" };
  for (const el of document.querySelectorAll("input[name=amode]")) el.checked = el.value === a.mode;
  const chosen = new Set(a.apps || []);
  const list = $("appList");
  list.replaceChildren();
  const apps = audioOpts.apps || [];
  if (apps.length === 0) {
    const p = document.createElement("p");
    p.className = "muted small";
    p.textContent = "No apps are playing audio right now.";
    list.append(p);
  }
  for (const app of apps) {
    const lab = document.createElement("label");
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.value = app.key;
    cb.checked = chosen.has(app.key);
    cb.onchange = () => setAudio("apps");
    lab.append(cb, app.name + (app.pid ? " (" + app.pid + ")" : ""));
    list.append(lab);
  }
  const sel = $("output");
  const prev = s.output || "";
  sel.replaceChildren(new Option("System default", ""));
  for (const d of audioOpts.devices || []) sel.append(new Option(d.name, d.id));
  sel.value = prev;
}

async function setAudio(mode) {
  const apps = [...document.querySelectorAll("#appList input:checked")].map((c) => c.value);
  try {
    render((await post("/api/audio", { mode, apps })).state);
  } catch (e) { showError(e.message); refresh(); }
}

function renderLogs(l) {
  $("logsBox").hidden = !l;
  if (!l) return;
  $("logsDir").textContent = l.file || l.dir || "none";
  $("logsGuardRow").hidden = !l.guard;
  $("logsGuard").textContent = l.guard || "";
  $("logsErr").hidden = !l.error;
  $("logsErr").textContent = l.error ? "Can't write log files: " + l.error : "";
  $("logsReport").textContent = l.report || "";
}

function renderLayout(l) {
  l = l || { mode: "windows" };
  for (const el of document.querySelectorAll("input[name=layout]")) el.checked = el.value === l.mode;
  const custom = l.rows > 0 || l.cols > 0;
  $("gridOpts").hidden = l.mode !== "grid";
  const editing = ["gridSize", "gridRows", "gridCols"].some((id) => document.activeElement === $(id));
  if (editing) return;
  $("gridSize").value = custom ? "custom" : "auto";
  $("gridCustom").hidden = !custom;
  $("gridRows").value = l.rows || "";
  $("gridCols").value = l.cols || "";
}

async function setLayout() {
  const mode = document.querySelector("input[name=layout]:checked")?.value || "windows";
  const custom = $("gridSize").value === "custom";
  $("gridCustom").hidden = !custom;
  const n = (id) => (custom ? Math.max(0, Math.min(8, parseInt($(id).value, 10) || 0)) : 0);
  try { render((await post("/api/layout", { mode, rows: n("gridRows"), cols: n("gridCols") })).state); }
  catch (e) { showError(e.message); }
}

function wire() {
  $("createBtn").onclick = async () => {
    showError("");
    busy($("createBtn"), true);
    try { render((await post("/api/create", { password: $("createPw").value })).state); }
    catch (e) { showError(e.message); }
    finally { busy($("createBtn"), false); }
  };
  $("joinBtn").onclick = async () => {
    showError("");
    busy($("joinBtn"), true);
    try { render((await post("/api/join", { link: $("joinLink").value })).state); }
    catch (e) { showError(e.message); }
    finally { busy($("joinBtn"), false); }
  };
  $("nameBtn").onclick = async () => {
    showError("");
    busy($("nameBtn"), true);
    try { await post("/api/name", { name: $("myName").value }); $("myName").blur(); }
    catch (e) { showError(e.message); }
    finally { busy($("nameBtn"), false); }
  };
  $("myName").onkeydown = (e) => { if (e.key === "Enter") $("nameBtn").click(); };
  $("leaveBtn").onclick = async () => {
    try { render((await post("/api/leave")).state); } catch (e) { showError(e.message); }
  };
  $("copyBtn").onclick = async () => {
    const link = current && current.session && current.session.link;
    if (!link) return;
    try { await navigator.clipboard.writeText(link); $("copyBtn").textContent = "Copied"; }
    catch { prompt("Copy this link:", link); }
    setTimeout(() => ($("copyBtn").textContent = "Copy share link"), 1500);
  };
  for (const el of document.querySelectorAll("input[name=src]")) el.onchange = () => setSource(el.value);
  $("refreshSources").onclick = loadSources;
  for (const el of document.querySelectorAll("input[name=layout]")) el.onchange = setLayout;
  $("gridSize").onchange = () => {
    if ($("gridSize").value === "custom") {
      if (!$("gridRows").value) $("gridRows").value = 2;
      if (!$("gridCols").value) $("gridCols").value = 2;
    }
    setLayout();
  };
  $("gridRows").onchange = setLayout;
  $("gridCols").onchange = setLayout;
  $("resolution").onchange = async () => {
    try { render((await post("/api/resolution", { res: $("resolution").value })).state); } catch (e) { showError(e.message); }
  };
  $("logsSend").onclick = async () => {
    busy($("logsSend"), true);
    try { render((await post("/api/logs/send")).state); } catch (e) { showError(e.message); refresh(); }
    finally { busy($("logsSend"), false); }
  };
  $("logsOpen").onclick = async () => {
    try { await post("/api/logs/open"); } catch (e) { showError(e.message); }
  };
  $("autostart").onchange = async () => {
    try { render((await post("/api/autostart", { on: $("autostart").checked })).state); } catch (e) { showError(e.message); }
  };
  $("updCheck").onclick = async () => {
    try { render((await post("/api/update/check")).state); } catch (e) { showError(e.message); }
  };
  $("updInstall").onclick = async () => {
    busy($("updInstall"), true);
    try {
      await post("/api/update/install");
      $("updStatus").textContent = "Installing. Blite will restart in a moment.";
    } catch (e) { showError(e.message); busy($("updInstall"), false); refresh(); }
  };
  $("updDismiss").onclick = async () => {
    try { render((await post("/api/update/dismiss")).state); } catch (e) { showError(e.message); }
  };
  loadSources();
  for (const el of document.querySelectorAll("input[name=amode]")) {
    el.onchange = () => setAudio(el.value);
  }
  $("refreshApps").onclick = loadAudio;
  $("output").onchange = async () => {
    try { render((await post("/api/output", { device: $("output").value })).state); }
    catch (e) { showError(e.message); }
  };
  const es = new EventSource("/api/events");
  es.addEventListener("change", refresh);
  es.onerror = () => { $("status").textContent = "reconnecting to Blite..."; };
}

wire();
refresh().then(loadAudio);
