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

// Simian's web UI: a read-only view over the controller's /api/. It polls
// every few seconds and refreshes at once when the live event stream says
// something happened. No framework and no build step, on purpose.
"use strict";

const $ = (id) => document.getElementById(id);
const state = { arena: decodeURIComponent(location.hash.slice(1)), autonomous: [], active: [], faultUIDs: new Set() };

const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const time = (t) => (t && !t.startsWith("0001") ? new Date(t).toLocaleTimeString() : "–");
const q = (path) => path + (state.arena ? (path.includes("?") ? "&" : "?") + "namespace=" + encodeURIComponent(state.arena) : "");

async function get(path) {
  const r = await fetch(path, { cache: "no-store" });
  if (!r.ok) throw new Error(path + ": " + r.status);
  return r.json();
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
        </div>
        <div class="muted">${esc(f.fault_uid)} · applied ${time(f.applied_at)} · until ${time(f.deadline)}</div>
      </div>`).join("")
    : '<div class="empty">No faults running.</div>';
}

function renderCycles(cycles) {
  if (!state.autonomous.length) {
    $("plan").innerHTML = '<div class="empty">Autonomous mode is off. Turn it on with the chart\'s <code>autonomous.enabled</code>.</div>';
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
          <td>${esc(r.source)}</td>
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

async function refresh() {
  const [active, faults, cycles, workloads] = await Promise.allSettled([
    get(q("/api/active")), get(q("/api/faults?limit=40")), get(q("/api/cycles?limit=15")),
    state.arena ? get(q("/api/topology")) : Promise.resolve([]),
  ]);
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

async function main() {
  try {
    const info = await get("/api/info");
    state.autonomous = info.autonomous || [];
    $("version").textContent = "v" + info.version;
  } catch (e) { /* the panels say what is missing */ }
  try {
    const arenas = await get("/api/arenas");
    for (const ns of arenas) $("arena").add(new Option(ns, ns));
  } catch (e) { /* All arenas still works */ }
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
