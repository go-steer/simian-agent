// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Simian's web UI: a view over the controller's /api/, and — behind
// Identity-Aware Proxy, for the people allowed — a form to inject and clear
// faults through the controller's executor. It polls every few seconds and
// refreshes at once when the live event stream says something happened. No
// framework and no build step, on purpose.
"use strict";

const $ = (id) => document.getElementById(id);
const state = { arena: decodeURIComponent(location.hash.slice(1)), autonomous: [], active: [], faultUIDs: new Set(), me: {}, catalog: [], config: null, adminFilled: false };

const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const time = (t) => (t && !t.startsWith("0001") ? new Date(t).toLocaleTimeString() : "–");
const q = (path) => path + (state.arena ? (path.includes("?") ? "&" : "?") + "namespace=" + encodeURIComponent(state.arena) : "");

async function get(path) {
  const r = await fetch(path, { cache: "no-store" });
  if (!r.ok) throw new Error(path + ": " + r.status);
  return r.json();
}

// post sends a write. X-Simian-UI is what the server requires of every
// write, so a page on another site cannot make one with the user's cookie.
async function post(path, body) {
  const r = await fetch(path, {
    method: "POST", cache: "no-store",
    headers: { "Content-Type": "application/json", "X-Simian-UI": "1" },
    body: body === undefined ? "" : JSON.stringify(body),
  });
  const text = await r.text();
  if (!r.ok) throw new Error(text.trim() || r.status);
  return text ? JSON.parse(text) : {};
}

function target(t) {
  const x = (t && t[0]) || {};
  return (x.namespace ? x.namespace + "/" : "") + (x.name || Object.values(x.labels || {})[0] || "?");
}

function remaining(deadline) {
  const s = Math.round((new Date(deadline) - Date.now()) / 1000);
  if (s <= 0) return "ending";
  return (s >= 60 ? Math.floor(s / 60) + "m " : "") + (s % 60) + "s";
}

function verdict(v) {
  if (v === true) return '<span class="badge ok">yes</span>';
  if (v === false) return '<span class="badge bad">no</span>';
  return '<span class="muted">–</span>';
}

function renderActive(list) {
  state.active = list;
  $("active-count").textContent = list.length ? "(" + list.length + ")" : "";
  $("active").innerHTML = list.length
    ? list.map((f) => `
      <div class="card">
        <div class="top">
          <span class="kind">${esc(f.manifest.resource_kind)}</span>
          <span>→ ${esc(target(f.manifest.targets))}</span>
          <span class="badge">${esc(f.manifest.engine)}</span>
          <span class="badge info">${esc(f.manifest.source || "")}</span>
          <span class="remaining" data-deadline="${esc(f.deadline)}">${remaining(f.deadline)}</span>
          ${state.me.can_write ? `<button class="clear" data-uid="${esc(f.fault_uid)}" title="Take it out now">Clear</button>` : ""}
        </div>
        <div class="muted">${esc(f.fault_uid)} · applied ${time(f.applied_at)} · until ${time(f.deadline)}</div>
      </div>`).join("")
    : '<div class="empty">No faults running.</div>';
}

function renderCycles(cycles) {
  if (!state.autonomous.length) {
    $("plan").innerHTML = state.me.can_admin
      ? '<div class="empty">Autonomous mode is off. Turn it on from ⚙ Configuration.</div>'
      : '<div class="empty">Autonomous mode is off.</div>';
  } else {
    const latest = cycles.find((c) => c.hypothesis);
    $("plan").innerHTML = latest
      ? `<div class="plan">
          <div class="muted">Latest plan · ${esc(latest.namespace)} · ${time(latest.started_at)}</div>
          <p class="hypothesis">${esc(latest.hypothesis)}</p>
          <ol>${(latest.steps || []).map((s) => `
            <li><span class="kind">${esc(s.kind)}</span> → ${esc(s.target)} for ${esc(s.duration)}
              ${s.rationale ? `<div class="why">${esc(s.rationale)}</div>` : ""}
              ${s.duration_rationale ? `<div class="why">Why that long: ${esc(s.duration_rationale)}</div>` : ""}
            </li>`).join("")}</ol>
        </div>`
      : `<div class="empty">Running in ${esc(state.autonomous.join(", "))}; no plan yet.</div>`;
  }
  $("cycles").innerHTML = cycles.length
    ? cycles.map((c) => {
        const cls = c.outcome === "completed" ? "ok" : c.outcome === "skipped" ? "warn" : c.outcome === "open" ? "info" : "";
        const label = c.outcome === "open" ? "running" : c.outcome;
        let what;
        if (c.outcome === "completed") {
          what = (c.steps || []).map((s) => esc(s.kind) + " → " + esc(s.target)).join(", ") +
            ` <span class="muted">(${(c.applied || []).length} applied${(c.refused || []).length ? ", " + c.refused.length + " refused" : ""})</span>`;
        } else if (c.outcome === "skipped") {
          what = esc(c.reason) + (c.detail ? ` <span class="muted">— ${esc(c.detail)}</span>` : "");
        } else if (c.outcome === "open") {
          what = '<span class="muted">checking the arena, planning or applying</span>';
        } else {
          what = '<span class="muted">' + esc(c.outcome) + "</span>";
        }
        return `<div class="cycle"><span class="muted">${time(c.started_at)}</span><span class="badge ${cls}">${esc(label)}</span><span>${state.arena ? "" : esc(c.namespace) + " · "}${what}</span></div>`;
      }).join("")
    : '<div class="empty">No cycles yet.</div>';
  if (!$("events").children.length) $("events").innerHTML = '<li class="empty">Waiting for events…</li>';
}

function renderFaults(rows) {
  state.faultUIDs = new Set(rows.map((r) => r.fault_uid));
  $("faults").innerHTML = rows.length
    ? `<table><thead><tr><th>Started</th><th>Kind</th><th>Target</th><th>Source</th><th>Outcome</th><th>Ended</th><th>Injected</th><th>Effect seen</th><th>Recovered</th></tr></thead><tbody>
      ${rows.map((r) => {
        const outcomeCls = r.outcome === "refused" || r.outcome === "driver-failed" ? "warn" : r.outcome === "open" ? "info" : "";
        return `<tr title="${esc(r.fault_uid)}${r.error ? " — " + esc(r.error) : ""}">
          <td class="num">${time(r.applied_at || r.received_at)}</td>
          <td class="kind">${esc(r.kind)}</td>
          <td>${esc(target(r.targets))}</td>
          <td>${esc(r.source)}${r.requested_by ? `<div class="muted">${esc(r.requested_by)}</div>` : ""}${r.cleared_by ? `<div class="muted">cleared by ${esc(r.cleared_by)}</div>` : ""}</td>
          <td><span class="badge ${outcomeCls}">${esc(r.outcome)}</span> <span class="muted">${esc(r.reason)}</span></td>
          <td class="num">${time(r.ended_at)}</td>
          <td>${verdict(r.injected)}</td>
          <td>${verdict(r.efficacy)}</td>
          <td>${verdict(r.recovered)}${r.recovered === false && r.unready ? `<div class="muted">${esc(r.unready.join("; "))}</div>` : ""}</td>
        </tr>`;
      }).join("")}</tbody></table>`
    : '<div class="empty">No faults yet.</div>';
}

function renderWorkloads(list) {
  if (!state.arena) {
    $("workloads").innerHTML = '<div class="empty">Pick an arena to see its workloads.</div>';
    return;
  }
  $("workloads").innerHTML = list.length
    ? `<table><thead><tr><th>Workload</th><th>Ready</th></tr></thead><tbody>
      ${list.map((w) => `<tr class="${w.ready < w.desired ? "short" : ""}"><td>${esc(w.kind)}/${esc(w.name)}</td><td class="num">${w.ready}/${w.desired}</td></tr>`).join("")}
      </tbody></table>`
    : '<div class="empty">No workloads found.</div>';
}

// An event belongs to the arena if it names it, targets it, or is about a
// fault already listed for it.
function inArena(e) {
  if (!state.arena) return true;
  const p = e.payload || {};
  if (p.namespace === state.arena) return true;
  if ((p.targets || []).some((t) => t.namespace === state.arena)) return true;
  return !!e.fault_uid && state.faultUIDs.has(e.fault_uid);
}

function eventClass(e) {
  if (e.event === "fault.recovered") return (e.payload || {}).passed === false ? "bad" : "ok";
  if (/rejected|failed|llm_unavailable/.test(e.event)) return "warn";
  if (/applied|injected|plan\.generated/.test(e.event)) return "info";
  return "";
}

function addEvent(e) {
  if (!inArena(e)) return;
  const p = e.payload || {};
  const where = p.namespace || ((p.targets || [])[0] || {}).namespace || "";
  const placeholder = $("events").querySelector(".empty");
  if (placeholder) placeholder.remove();
  const li = document.createElement("li");
  li.innerHTML = `<span class="muted">${time(e.ts)}</span><span class="ev badge ${eventClass(e)}">${esc(e.event)}</span>
    <span>${esc(where)}${p.kind ? " · " + esc(p.kind) : ""}${e.reason ? ' <span class="muted">' + esc(e.reason) + "</span>" : ""}</span>`;
  $("events").prepend(li);
  while ($("events").children.length > 200) $("events").lastChild.remove();
}

const row = (k, v) => `<tr><td>${esc(k)}</td><td>${v}</td></tr>`;

function renderConfig(c) {
  state.config = c;
  const x = c.executor || {};
  $("limits").innerHTML =
    row("Faults at once", esc(x.max_concurrent_faults) + ' <span class="muted">executor.maxConcurrentFaults</span>') +
    row("Longest fault", esc(x.duration_ceiling) + ' <span class="muted">executor.durationCeiling</span>') +
    row("Cooldown", esc(x.min_cooldown) + ' <span class="muted">executor.minCooldown</span>') +
    row("Blast radius", esc((x.permitted_tiers || []).join(", ")) + ' <span class="muted">executor.permittedTiers</span>') +
    row("Default probes", x.default_probes ? "on" : "off") +
    (x.llm ? row("LLM", esc(x.llm)) : "");
  $("arenas").innerHTML = (c.arenas || []).length
    ? c.arenas.map((a) => row(a.name, a.excluded.length ? "excluded: " + esc(a.excluded.join(", ")) : '<span class="muted">nothing excluded</span>')).join("")
    : '<tr><td class="empty">No arenas.</td></tr>';
  const au = c.autonomous;
  if (!au) {
    $("autonomy").innerHTML = row("Autonomous mode", "not configurable here");
  } else {
    const s = au.settings;
    const where = (s.namespaces || []).map((ns) => esc(ns) + (isPaused(s, ns) ? ' <span class="badge paused">paused</span>' : "")).join(", ");
    $("autonomy").innerHTML =
      row("State", s.enabled ? (isPaused(s, "*") ? '<span class="badge paused">paused everywhere</span>' : '<span class="badge ok">on</span>') : "off") +
      row("Arenas", where || '<span class="muted">none</span>') +
      row("Every", esc(s.interval)) +
      row("Faults per cycle", esc(s.max_faults_per_cycle)) +
      row("Severity cap", esc(s.max_severity_per_cycle || "–")) +
      (s.hypothesis ? row("Hypothesis hint", esc(s.hypothesis)) : "");
    $("autonomy-source").textContent = au.source && au.source.by
      ? `Set from this page by ${au.source.by} at ${new Date(au.source.at).toLocaleString()}; the install's settings apply again on revert.`
      : "The install's settings (chart values autonomous.*).";
  }
  if (au) state.autonomous = au.settings.enabled ? au.settings.namespaces || [] : [];
  if (c.you && c.you.can_admin && au) {
    $("admin").hidden = false;
    if (!state.adminFilled) fillAdmin(c);
    renderPauses(c);
  }
}

function isPaused(s, ns) { return (s.paused || []).includes("*") || (s.paused || []).includes(ns); }

function fillAdmin(c) {
  const s = c.autonomous.settings;
  $("a-enabled").checked = s.enabled;
  $("a-namespaces").innerHTML = (c.arenas || []).map((a) =>
    `<label><input type="checkbox" class="a-ns" value="${esc(a.name)}" ${(s.namespaces || []).includes(a.name) ? "checked" : ""}> ${esc(a.name)}</label>`).join(" ");
  $("a-interval").value = s.interval;
  $("a-max").value = s.max_faults_per_cycle;
  $("a-max").max = (c.executor || {}).max_concurrent_faults || "";
  $("a-max-hint").textContent = (c.executor || {}).max_concurrent_faults
    ? `up to ${c.executor.max_concurrent_faults}, the faults allowed at once (executor.maxConcurrentFaults)` : "";
  $("a-tier").innerHTML = ((c.executor || {}).permitted_tiers || []).map((t) => `<option ${t === s.max_severity_per_cycle ? "selected" : ""}>${esc(t)}</option>`).join("");
  $("a-hint").value = s.hypothesis || "";
  state.adminFilled = true;
}

function renderPauses(c) {
  const s = c.autonomous.settings;
  const all = isPaused(s, "*");
  $("a-pauses").innerHTML = `<button type="button" data-ns="" data-pause="${!all}">${all ? "Resume" : "Pause"} everywhere</button>` +
    (s.namespaces || []).map((ns) => {
      const p = isPaused(s, ns) && !all;
      return `<button type="button" data-ns="${esc(ns)}" data-pause="${!p}" ${all ? "disabled" : ""}>${p ? "Resume" : "Pause"} ${esc(ns)}</button>`;
    }).join("");
}

async function adminAction(path, body, out, done) {
  $(out).className = "";
  $(out).textContent = "…";
  try {
    const res = await post(path, body);
    $(out).className = "ok";
    $(out).textContent = done(res);
    state.adminFilled = false;
    refresh();
  } catch (e) {
    $(out).className = "bad";
    $(out).textContent = "Refused: " + e.message;
  }
}

function setupAdmin() {
  $("settings").onclick = () => { state.adminFilled = false; if (state.config) renderConfig(state.config); $("config-dialog").showModal(); };
  $("config-close").onclick = () => $("config-dialog").close();
  $("admin").onsubmit = (ev) => {
    ev.preventDefault();
    const settings = {
      enabled: $("a-enabled").checked,
      namespaces: [...document.querySelectorAll(".a-ns:checked")].map((el) => el.value),
      interval: $("a-interval").value.trim(),
      max_faults_per_cycle: parseInt($("a-max").value, 10),
      max_severity_per_cycle: $("a-tier").value,
      hypothesis: $("a-hint").value.trim(),
      paused: (state.config.autonomous.settings.paused || []),
    };
    if (!confirm(settings.enabled ? `Run autonomous mode in ${settings.namespaces.join(", ")} every ${settings.interval}?` : "Turn autonomous mode off?")) return;
    adminAction("/api/admin/autonomous", settings, "a-result", () => "Applied.");
  };
  $("a-reset").onclick = () => {
    if (!confirm("Revert autonomous mode to the install's settings?")) return;
    adminAction("/api/admin/autonomous/reset", undefined, "a-result", () => "Reverted to the install's settings.");
  };
  $("a-pauses").addEventListener("click", (ev) => {
    const b = ev.target.closest("button");
    if (!b) return;
    const paused = b.dataset.pause === "true";
    adminAction("/api/admin/pause", { namespace: b.dataset.ns, paused }, "a-result", () => (paused ? "Paused " : "Resumed ") + (b.dataset.ns || "everywhere") + ".");
  });
  $("a-clear-all").onclick = () => {
    const ns = state.arena;
    if (!confirm(`Clear every running fault${ns ? " in " + ns : ""} now?`)) return;
    adminAction("/api/admin/clear-all", { namespace: ns }, "a-clear-result", (r) =>
      `Cleared ${r.cleared.length}${Object.keys(r.failed || {}).length ? ", " + Object.keys(r.failed).length + " could not be cleared" : ""}.`);
  };
}

async function refresh() {
  const [active, faults, cycles, workloads, config] = await Promise.allSettled([
    get(q("/api/active")), get(q("/api/faults?limit=40")), get(q("/api/cycles?limit=15")),
    state.arena ? get(q("/api/topology")) : Promise.resolve([]),
    get("/api/config"),
  ]);
  if (config.status === "fulfilled") renderConfig(config.value);
  if (active.status === "fulfilled") renderActive(active.value);
  if (faults.status === "fulfilled") renderFaults(faults.value);
  if (cycles.status === "fulfilled") renderCycles(cycles.value);
  if (workloads.status === "fulfilled") renderWorkloads(workloads.value);
}

let pending;
function refreshSoon() {
  clearTimeout(pending);
  pending = setTimeout(refresh, 400);
}

function connect() {
  const es = new EventSource("/api/events");
  es.onopen = () => $("conn").classList.add("live");
  es.onerror = () => $("conn").classList.remove("live"); // EventSource reconnects by itself
  es.onmessage = (m) => {
    addEvent(JSON.parse(m.data));
    refreshSoon();
  };
}

// The template's JSON example, without its selector: the target is the
// workload picked in the form, and the executor narrows to it.
function specFromTemplate(tpl) {
  const start = (tpl || "").indexOf("{");
  if (start < 0) return {};
  let depth = 0;
  for (let i = start; i < tpl.length; i++) {
    if (tpl[i] === "{") depth++;
    else if (tpl[i] === "}" && --depth === 0) {
      try {
        const spec = JSON.parse(tpl.slice(start, i + 1));
        delete spec.selector;
        return spec;
      } catch (e) { return {}; }
    }
  }
  return {};
}

async function loadWorkloads() {
  const ns = $("f-arena").value;
  const sel = $("f-workload");
  sel.innerHTML = "";
  if (!ns) return;
  try {
    for (const w of await get("/api/topology?namespace=" + encodeURIComponent(ns))) sel.add(new Option(w.name, w.name));
  } catch (e) { sel.add(new Option("(could not list workloads)", "")); }
}

function fillSpec() {
  const c = state.catalog[$("f-kind").selectedIndex];
  if (!c) return;
  $("f-spec").value = JSON.stringify(specFromTemplate(c.spec_template), null, 2);
  $("f-hint").textContent = (c.spec_template || "").split("\n").filter((l) => !l.trim().startsWith("{") && !l.trim().startsWith('"') && !l.trim().startsWith("}")).join("\n");
}

async function setupInject(arenas) {
  try { state.me = await get("/api/me"); } catch (e) { state.me = {}; }
  $("me").textContent = state.me.email ? state.me.email + (state.me.can_write ? "" : " · view only") : "read-only";
  $("inject-panel").hidden = false;
  if (!state.me.can_write) {
    $("inject-readonly").hidden = false;
    $("inject-readonly").innerHTML = state.me.auth === "iap"
      ? `${esc(state.me.email)} may view but not inject or clear faults. The chart's <code>ui.iap.writers</code> lists who may.`
      : "Read-only here. Injecting and clearing faults from the browser needs the UI behind Identity-Aware Proxy; from here, use <code>simian chaos</code>.";
    return;
  }
  $("inject").hidden = false;
  for (const ns of arenas) $("f-arena").add(new Option(ns, ns));
  $("f-arena").value = state.arena || arenas[0] || "";
  $("f-arena").onchange = loadWorkloads;
  loadWorkloads();
  try { state.catalog = await get("/api/catalog"); } catch (e) { state.catalog = []; }
  for (const c of state.catalog) $("f-kind").add(new Option(c.resource_kind + " (" + c.engine + ")", c.resource_kind));
  $("f-kind").onchange = fillSpec;
  fillSpec();
  document.querySelectorAll("input[name=mode]").forEach((el) => {
    el.onchange = () => {
      const intent = document.querySelector("input[name=mode]:checked").value === "intent";
      $("f-exact").hidden = intent;
      $("f-workload-label").hidden = intent; // the LLM picks the target
      $("f-intent").hidden = !intent;
    };
  });
  $("inject").onsubmit = async (ev) => {
    ev.preventDefault();
    const mode = document.querySelector("input[name=mode]:checked").value;
    const body = { mode, namespace: $("f-arena").value, workload: $("f-workload").value, duration: $("f-duration").value.trim() };
    let what;
    if (mode === "intent") {
      body.intent = $("f-text").value.trim();
      what = `"${body.intent}" in ${body.namespace}`;
    } else {
      const c = state.catalog[$("f-kind").selectedIndex] || {};
      body.kind = c.resource_kind;
      body.engine = c.engine;
      try { body.spec = JSON.parse($("f-spec").value || "{}"); } catch (e) {
        $("f-result").className = "bad";
        $("f-result").textContent = "The spec is not valid JSON: " + e.message;
        return;
      }
      what = `${body.kind} into ${body.namespace}/${body.workload || "(no workload)"}`;
    }
    if (!confirm(`Inject ${what} for ${body.duration}?`)) return;
    $("f-result").className = "";
    $("f-result").textContent = mode === "intent" ? "Asking the LLM…" : "Applying…";
    try {
      const res = await post("/api/faults", body);
      $("f-result").className = "ok";
      $("f-result").textContent = `Applied ${res.kind} as ${res.fault_uid}.`;
      refreshSoon();
    } catch (e) {
      $("f-result").className = "bad";
      $("f-result").textContent = "Refused: " + e.message;
    }
  };
  $("active").addEventListener("click", async (ev) => {
    const b = ev.target.closest("button.clear");
    if (!b || !confirm(`Clear ${b.dataset.uid} now?`)) return;
    b.disabled = true;
    try { await post("/api/faults/" + encodeURIComponent(b.dataset.uid) + "/clear"); refreshSoon(); } catch (e) { alert("Could not clear: " + e.message); b.disabled = false; }
  });
}

async function main() {
  try {
    const info = await get("/api/info");
    state.autonomous = info.autonomous || [];
    $("version").textContent = "v" + info.version;
  } catch (e) { /* the panels say what is missing */ }
  let arenas = [];
  try {
    arenas = await get("/api/arenas");
    for (const ns of arenas) $("arena").add(new Option(ns, ns));
  } catch (e) { /* All arenas still works */ }
  await setupInject(arenas);
  setupAdmin();
  $("arena").value = state.arena;
  $("arena").onchange = () => {
    state.arena = $("arena").value;
    location.hash = state.arena ? encodeURIComponent(state.arena) : "";
    $("events").innerHTML = "";
    refresh();
  };
  setInterval(() => document.querySelectorAll("[data-deadline]").forEach((el) => { el.textContent = remaining(el.dataset.deadline); }), 1000);
  setInterval(refresh, 5000);
  refresh();
  connect();
}

main();
