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
// ctl is the Simian the panes show (see "Several Simians").
const state = { arena: decodeURIComponent(location.hash.slice(1)), autonomous: [], active: [], faultUIDs: new Set(), me: {}, catalog: [], config: null, adminFilled: false, halted: false, ctl: null };

const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const time = (t) => (t && !t.startsWith("0001") ? new Date(t).toLocaleTimeString() : "–");
const q = (path) => path + (state.arena ? (path.includes("?") ? "&" : "?") + "namespace=" + encodeURIComponent(state.arena) : "");

// ─── Talking to Simian ──────────────────────────────────────────────
// Behind IAP and a load balancer, not every answer comes from Simian: the
// load balancer times out (504 "upstream request timeout") or cannot reach
// it (502/503, often an HTML page), and an expired sign-in turns into a
// redirect to Google. Those must not read as Simian refusing, so they are
// thrown as a NotSimian error that says what happened. Simian's own
// errors are plain text and never 503, 504 or a redirect.
//
// The page may watch several Simians (see "Several Simians" below), and
// every call goes to one of them: the one selected, unless the caller
// names another. A Simian on another origin is called directly, with the
// user's own credentials for it; when that fails before any answer, the
// browser does not say why, and the two likely reasons — not signed in to
// its Identity-Aware Proxy, or it does not list this page in
// ui.allowedOrigins — are both given.

class NotSimian extends Error {
  constructor(why, status, body, c) {
    const [head, tail] = NOT_SIMIAN[why](c ? c.name : "Simian", c ? c.url : "");
    super(head + (status ? ` (HTTP ${status}${snippet(body)})` : "") + ". " + tail);
    this.why = why;
    this.status = status;
    this.body = body;
    // A sign-in redirect, or a call the browser kept from leaving, is
    // answered before the request reaches Simian.
    this.reachedSimian = !["signin", "crossSignin", "blocked", "unreachable"].includes(why);
  }
}

// What happened, then what it means; name and url are the Simian's.
const here = () => location.origin;
const NOT_SIMIAN = {
  timeout: (n) => [`The connection to ${n} failed: the load balancer in front of it timed out waiting for the answer`, "That is not Simian refusing."],
  gateway: (n) => [`The connection to ${n} failed: the load balancer could not reach it`, "Simian may be restarting; that is not Simian refusing."],
  signin: () => ["Your sign-in has expired: Identity-Aware Proxy answered instead of Simian", "Reload the page to sign in again."],
  network: () => ["The connection to Simian failed: the request did not complete", "The network dropped, or your sign-in expired (reload the page if it keeps happening)."],
  foreign: (n) => [`Something other than ${n} answered (a proxy or load balancer page)`, "That is not Simian refusing."],
  crossSignin: (n) => [`${n} sent this page to sign in: you are not signed in to its Identity-Aware Proxy yet`, `Sign in to ${n} (it opens in a new tab), then come back here.`],
  blocked: (n) => [`${n} did not answer this page`, `Either you are not signed in to its Identity-Aware Proxy yet, or it does not list this page (${here()}) in ui.allowedOrigins. Sign in to ${n} first; if it still does not answer, ask whoever runs it to add ${here()} to ui.allowedOrigins.`],
  unreachable: (n, u) => [`${n} could not be reached at ${u}`, "It may be down, the URL may be wrong, or your network cannot reach it."],
};

function snippet(body) {
  const t = String(body || "").replace(/<[^>]*>/g, " ").replace(/\s+/g, " ").trim();
  return t && t.length < 80 ? ` “${t}”` : "";
}

// notSimian says whether a failed response came from something in front
// of Simian, and if so what kind of thing went wrong.
function notSimian(status, type, text) {
  const html = /html/i.test(type) || /^\s*</.test(text);
  // What Envoy and Google's front ends say; Simian's own 502s carry a Go
  // error instead.
  const proxyText = /^\s*(upstream|no healthy upstream)|bad gateway|service unavailable/i.test(text);
  if (status === 401 || (html && /accounts\.google\.com|ServiceLogin/i.test(text))) return "signin";
  if (status === 504 || status === 408 || (status === 502 && /upstream request timeout|gateway time-?out/i.test(text))) return "timeout";
  if (status === 503 || (status === 502 && (html || proxyText || !text.trim()))) return "gateway";
  if (html) return "foreign";
  return "";
}

// call fetches from a Simian: c, or the one selected. Redirects are not
// followed: none of /api/ redirects, so one is IAP sending the browser to
// sign in.
async function call(path, init, c = state.ctl) {
  const cross = !!(c && c.base);
  let r;
  try {
    r = await fetch((c ? c.base : "") + path,
      Object.assign({ cache: "no-store", redirect: "manual" }, cross ? { credentials: "include" } : {}, init));
  } catch (e) {
    throw cross ? await crossFailure(c) : new NotSimian("network", 0, "");
  }
  if (r.type === "opaqueredirect" || (r.status >= 300 && r.status < 400)) throw new NotSimian(cross ? "crossSignin" : "signin", r.status || 0, "", c);
  const text = await r.text();
  const type = r.headers.get("Content-Type") || "";
  let why = notSimian(r.status, type, text);
  if (why === "signin" && cross) why = "crossSignin";
  if (why) throw new NotSimian(why, r.status, text, c);
  if (!r.ok) {
    const e = new Error(text.trim() || String(r.status));
    e.status = r.status;
    throw e;
  }
  return { status: r.status, data: text ? JSON.parse(text) : {} };
}

// crossFailure tells, as well as a page can, why a call to another origin
// got no answer: if the same URL answers a no-cors request (which reads
// nothing, and so needs no permission), the server is there and the
// browser kept its answer from the page — not signed in, or this origin
// not allowed. If that fails too, it is not reachable. One probe serves
// every call that fails within a few seconds.
function crossFailure(c) {
  const now = Date.now();
  if (!c.probe || now - c.probe.at > 4000) {
    c.probe = {
      at: now,
      result: fetch(c.base + "/healthz", { mode: "no-cors", credentials: "include", cache: "no-store" })
        .then(() => new NotSimian("blocked", 0, "", c), () => new NotSimian("unreachable", 0, "", c)),
    };
  }
  return c.probe.result;
}

// Stale is a read that came back after the page moved to another Simian;
// whatever asked for it drops it.
class Stale extends Error {}

// refusal words a failed write: Simian's refusal, or — when something in
// front of Simian answered — what actually happened.
const refusal = (e) => (e instanceof NotSimian ? e.message : "Refused: " + e.message);

async function get(path, c) {
  const from = c || state.ctl;
  const data = (await call(path, undefined, from)).data;
  if (!c && from !== state.ctl) throw new Stale("moved to another Simian");
  return data;
}

// send makes a write and answers {status, data}. X-Simian-UI is what the
// server requires of every write, so a page on another site cannot make
// one with the user's cookie — and across origins, only a page the
// Simian lists in ui.allowedOrigins may send it.
function send(path, body, c) {
  return call(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Simian-UI": "1" },
    body: body === undefined ? "" : JSON.stringify(body),
  }, c || state.ctl);
}

async function post(path, body, c) {
  return (await send(path, body, c)).data;
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

// attend marks a pane that holds something to look at: "warn" (amber,
// --warn) for something in progress or held, "bad" (rose, --bad) for a
// failure, "" for calm. The glow itself is panel.css.
function attend(panelId, level) {
  const el = $(panelId);
  el.classList.toggle("attn-warn", level === "warn");
  el.classList.toggle("attn-bad", level === "bad");
}

// A cycle skipped for one of these is autonomous mode failing, not
// standing down.
const CYCLE_ERRORS = ["health-gate", "llm-unavailable", "no-valid-plan"];

// The Autonomous mode pane: rose when an arena's most recent cycle was
// skipped for an error; amber while halted, or when an arena's most
// recent cycle was skipped because it is paused.
function attendAutonomy() {
  if (!state.autonomous.length) return attend("plan-panel", state.halted ? "warn" : "");
  const seen = new Set();
  let level = state.halted ? "warn" : "";
  for (const c of state.cycles || []) {
    if (seen.has(c.namespace)) continue;
    seen.add(c.namespace);
    if (c.outcome !== "skipped") continue;
    if (CYCLE_ERRORS.includes(c.reason)) { level = "bad"; break; }
    if (c.reason === "paused") level = "warn";
  }
  attend("plan-panel", level);
}

function renderActive(list) {
  state.active = list;
  attend("active-panel", list.length ? "warn" : "");
  $("active-count").textContent = list.length ? "(" + list.length + ")" : "";
  $("st-active").textContent = "active: " + list.length;
  $("st-active").className = "status-item" + (list.length ? " status-live" : "");
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
  state.cycles = cycles;
  attendAutonomy();
  const halted = state.halted
    ? '<div class="halted-note">Halted: autonomous mode is paused everywhere. Nothing new starts until an admin resumes it.</div>'
    : "";
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
      : `<div class="empty">${state.halted ? "Set to run" : "Running"} in ${esc(state.autonomous.join(", "))}; no plan yet.</div>`;
  }
  if (halted) $("plan").insertAdjacentHTML("afterbegin", halted);
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
  attend("faults-panel", rows.some((r) => r.recovered === false || r.outcome === "refused" || r.outcome === "driver-failed") ? "bad" : "");
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

// renderWorkloads shows one arena's workloads, or with All arenas every
// arena's, each under its name.
function renderWorkloads(list) {
  const all = !state.arena;
  attend("workloads-panel", list.some((w) => w.ready < w.desired) ? "bad" : "");
  $("workloads").innerHTML = list.length
    ? `<table><thead><tr>${all ? "<th>Arena</th>" : ""}<th>Workload</th><th>Ready</th></tr></thead><tbody>
      ${list.map((w) => `<tr class="${w.ready < w.desired ? "short" : ""}">${all ? `<td class="muted">${esc(w.arena)}</td>` : ""}<td>${esc(w.kind)}/${esc(w.name)}</td><td class="num">${w.ready}/${w.desired}</td></tr>`).join("")}
      </tbody></table>`
    : '<div class="empty">No workloads found.</div>';
}

// workloadsEverywhere reads each arena's topology and tags its workloads
// with the arena, for the All arenas view.
async function workloadsEverywhere() {
  const lists = await Promise.all((state.arenas || []).map((ns) =>
    get("/api/topology?namespace=" + encodeURIComponent(ns))
      .then((ws) => ws.map((w) => Object.assign({ arena: ns }, w)))
      .catch(() => [])));
  return lists.flat();
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
  $("st-auto").textContent = "autonomous: " + (!au ? "–" : !au.settings.enabled ? "off"
    : isPaused(au.settings, "*") ? "paused" : (au.settings.namespaces || []).join(", ") || "on");
  if (c.you && c.you.can_admin && au) {
    $("admin").hidden = false;
    if (!state.adminFilled) fillAdmin(c);
    renderPauses(c);
  }
  renderHalt(c);
}

// ─── Emergency halt ─────────────────────────────────────────────────
// Halted is autonomous mode paused everywhere ("*"), however it got
// there. Everyone sees it — the HUD chip and the status-bar line — so
// nobody watching mistakes a stopped Simian for a quiet one. Admins get
// the button: HALT, or while halted, RESUME. Neither happens without a
// confirm, and resuming never happens by itself.
function renderHalt(c) {
  const s = (c.autonomous || {}).settings || {};
  state.halted = (s.paused || []).includes("*");
  const admin = !!(c.you && c.you.can_admin);
  document.body.classList.toggle("halted", state.halted);
  $("btn-halt").hidden = !admin;
  $("btn-halt").textContent = state.halted ? "⏻ HALTED · RESUME" : "⏻ HALT";
  $("btn-halt").title = state.halted
    ? "Autonomous mode is paused everywhere. Resume it"
    : "Emergency halt: pause autonomous mode everywhere, then clear every fault";
  $("btn-halt").classList.toggle("is-halted", state.halted);
  $("hud-halted").hidden = admin || !state.halted;
  $("st-halted").hidden = !state.halted;
  attendAutonomy();
}

const plural = (n, one, many) => n + " " + (n === 1 ? one : many);

// haltPlan says exactly what a halt would do now: the faults it would
// clear (every arena, whatever the page is filtered to) and what happens
// to autonomous mode.
async function haltPlan(c) {
  const active = await get("/api/active", c);
  const au = (state.config || {}).autonomous;
  const s = au ? au.settings : null;
  const faults = active.length ? "clear " + plural(active.length, "running fault", "running faults") : "no faults are running to clear";
  let auto = "";
  if (!s) auto = "";
  else if ((s.paused || []).includes("*")) auto = "keep autonomous mode paused everywhere";
  else if (!s.enabled) auto = "keep autonomous mode paused (it is off now, and stays paused if it is turned on)";
  else auto = "pause autonomous mode in " + ((s.namespaces || []).join(", ") || "every arena");
  const what = active.length
    ? `Halt: ${faults}${auto ? " and " + auto : ""}?`
    : `Halt: ${faults}; ${auto || "nothing else to stop"}?`;
  return { active, what };
}

let haltAction = null;

function openHaltDialog({ title, what, list, note, go, action }) {
  $("halt-title").textContent = title;
  $("halt-what").textContent = what;
  $("halt-list").innerHTML = list || "";
  $("halt-list").hidden = !list;
  $("halt-note").textContent = note || "";
  $("halt-go").textContent = go;
  $("halt-go").hidden = false;
  $("halt-go").disabled = false;
  $("halt-cancel").textContent = "Cancel";
  $("halt-result").className = "";
  $("halt-result").textContent = "";
  haltAction = action;
  if (!$("halt-dialog").open) $("halt-dialog").showModal();
  $("halt-cancel").focus();
}

// onWhich names the Simian a halt or resume acts on, when the page
// watches more than one.
const onWhich = (c) => (sims.list.length > 1 ? " · " + c.name : "");
const onlyThere = (c) => (sims.list.length > 1 ? ` This acts on ${c.name} only; the other Simians are not touched.` : "");

async function haltOrResume() {
  const c = state.ctl;
  if (!c) return;
  if (state.halted) {
    const s = ((state.config || {}).autonomous || {}).settings || {};
    openHaltDialog({
      title: "Resume autonomous mode" + onWhich(c),
      what: s.enabled
        ? `Resume autonomous mode in ${(s.namespaces || []).join(", ") || "its arenas"}? It plans and injects faults again from its next cycle.`
        : "Lift the halt? Autonomous mode is off, so nothing starts until it is turned on.",
      note: "This lifts every pause, here and per arena. It is recorded in the audit trail in your name." + onlyThere(c),
      go: "Resume",
      action: async () => {
        await post("/api/admin/pause", { namespace: "", paused: false }, c);
        return "Resumed.";
      },
    });
    return;
  }
  let plan;
  try { plan = await haltPlan(c); } catch (e) { alert("Could not read the running faults: " + e.message); return; }
  openHaltDialog({
    title: "Emergency halt" + onWhich(c),
    what: plan.what,
    list: plan.active.map((f) => `<li><span class="kind">${esc(f.manifest.resource_kind)}</span> → ${esc(target(f.manifest.targets))} <span class="muted">${esc(f.fault_uid)}</span></li>`).join(""),
    note: "Autonomous mode is paused first, so it cannot start a fault while the rest are cleared, and it stays paused until an admin resumes it. Recorded in the audit trail in your name." + onlyThere(c),
    go: "⏻ Halt",
    action: async () => {
      const r = await post("/api/admin/halt", undefined, c);
      const failed = Object.entries(r.failed || {});
      return "Halted: cleared " + plural((r.cleared || []).length, "fault", "faults") +
        (r.paused ? ", autonomous mode paused everywhere" : "") + "." +
        (failed.length ? " Could not clear " + failed.map(([uid, err]) => uid + " (" + err + ")").join(", ") + "." : "") +
        (r.warning ? " Warning: " + r.warning : "");
    },
  });
}

function setupHalt() {
  $("btn-halt").onclick = haltOrResume;
  const close = () => $("halt-dialog").close();
  $("halt-close").onclick = close;
  $("halt-cancel").onclick = close;
  $("halt-go").onclick = async () => {
    if (!haltAction) return;
    $("halt-go").disabled = true;
    $("halt-result").className = "";
    $("halt-result").textContent = "…";
    try {
      const msg = await haltAction();
      haltAction = null;
      $("halt-result").className = msg.includes("Could not") || msg.includes("Warning") ? "bad" : "ok";
      $("halt-result").textContent = msg;
      $("halt-go").hidden = true;
      $("halt-cancel").textContent = "Close";
      state.adminFilled = false;
      refresh();
    } catch (e) {
      $("halt-result").className = "bad";
      $("halt-result").textContent = refusal(e);
      $("halt-go").disabled = false;
    }
  };
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
    $(out).textContent = refusal(e);
  }
}

function setupAdmin() {
  $("settings").onclick = () => {
    state.adminFilled = false;
    $("config-title").textContent = "Configuration" + (state.ctl ? onWhich(state.ctl) : "");
    if (state.config) renderConfig(state.config);
    $("config-dialog").showModal();
  };
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
  const c = state.ctl;
  if (!c) return;
  const [active, faults, cycles, workloads, config] = await Promise.allSettled([
    get(q("/api/active")), get(q("/api/faults?limit=40")), get(q("/api/cycles?limit=15")),
    state.arena ? get(q("/api/topology")) : workloadsEverywhere(),
    get("/api/config"),
  ]);
  if (state.ctl !== c) return; // the page moved to another Simian meanwhile
  if (config.status === "fulfilled") renderConfig(config.value);
  if (active.status === "fulfilled") renderActive(active.value);
  if (faults.status === "fulfilled") renderFaults(faults.value);
  if (cycles.status === "fulfilled") renderCycles(cycles.value);
  if (workloads.status === "fulfilled") renderWorkloads(workloads.value);
  // What the strip says about this Simian comes from the same answers.
  const was = c.st.conn;
  if (active.status === "fulfilled") {
    noteStatus(c, { conn: "live", why: "" });
    // The strip counts every arena's, whatever the panes are filtered to.
    if (!state.arena) c.st.active = active.value.length;
    else get("/api/active", c).then((all) => { c.st.active = all.length; renderSims(); }, () => {});
  } else {
    noteFailure(c, active.reason);
    // Another origin's Simian not answering: say so in every pane rather
    // than show it empty (the note under the HUD says why). This page's
    // own keeps what it last showed through a blip, as before.
    const note = `<div class="empty">Not connected to ${esc(c.name)}${c.base ? " — the note at the top says why and what to do" : ""}.</div>`;
    for (const id of ["active", "plan", "cycles", "faults", "workloads"]) {
      if (c.base || $(id).querySelector(".loading")) $(id).innerHTML = id === "cycles" && c.base ? "" : note;
    }
  }
  if (config.status === "fulfilled") noteConfig(c, config.value);
  renderSims();
  if (c.st.conn === "live" && was !== "live" && was !== "unknown") {
    // Signed in somewhere else, or it came back: read the rest again.
    loadController(c);
    if (!es || es.readyState === EventSource.CLOSED) connect();
  }
}

let pending;
function refreshSoon() {
  clearTimeout(pending);
  pending = setTimeout(refresh, 400);
}

// es is the selected Simian's event stream; the others are polled, not
// streamed.
let es = null;
function connect() {
  if (es) { es.close(); es = null; }
  const c = state.ctl;
  if (!c) return;
  const src = new EventSource(c.base + "/api/events", c.base ? { withCredentials: true } : undefined);
  es = src;
  const live = (on) => {
    if (es !== src) return;
    $("conn").classList.toggle("live", on);
    $("st-stream").textContent = on ? "stream: live" : "stream: reconnecting";
    $("st-stream").className = "status-item" + (on ? "" : " status-bad");
  };
  let dropped = false;
  src.onopen = () => {
    live(true);
    // Back after a drop: the stream replays recent events, and
    // /api/faults fills in anything older for the faults being followed.
    if (dropped) backfillAll();
    dropped = false;
  };
  src.onerror = () => { live(false); dropped = true; }; // EventSource reconnects by itself
  src.onmessage = (m) => {
    if (es !== src) return;
    const e = JSON.parse(m.data);
    onStream(e);
    addEvent(e);
    refreshSoon();
  };
}

// The template's JSON example, without its selector: the target is the
// workload picked in the form, and the executor narrows to it.
function specFromTemplate(tpl) {
  // The template mixes prose with an example. Prose can hold brace
  // fragments that are not JSON — StressChaos's hint says
  // `"stressors": {"cpu": {...}}` — so take the first balanced {…} block
  // that parses, not merely the first one.
  const t = tpl || "";
  for (let start = t.indexOf("{"); start >= 0; start = t.indexOf("{", start + 1)) {
    let depth = 0;
    for (let i = start; i < t.length; i++) {
      if (t[i] === "{") depth++;
      else if (t[i] === "}" && --depth === 0) {
        try {
          const spec = JSON.parse(t.slice(start, i + 1));
          if (spec && typeof spec === "object" && !Array.isArray(spec)) {
            delete spec.selector;
            return spec;
          }
        } catch (e) { /* prose, not the example: try the next block */ }
        break;
      }
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
  } catch (e) {
    if (!(e instanceof Stale)) sel.add(new Option("(could not list workloads)", ""));
  }
}

function fillSpec() {
  const c = state.catalog[$("f-kind").selectedIndex];
  if (!c) return;
  $("f-spec").value = JSON.stringify(specFromTemplate(c.spec_template), null, 2);
  $("f-hint").textContent = (c.spec_template || "").split("\n").filter((l) => !l.trim().startsWith("{") && !l.trim().startsWith('"') && !l.trim().startsWith("}")).join("\n");
}

// fillInject sets the pane up for the selected Simian: who you are there,
// and — if you may write there — its arenas and the kinds it offers. me is
// /api/me's answer, or the error reading it.
async function fillInject(c, me, arenas) {
  const failed = me instanceof Error;
  state.me = failed ? {} : me || {};
  $("me").textContent = state.me.email ? state.me.email + (state.me.can_write ? "" : " · view only") : failed && c.base ? "not signed in" : "read-only";
  $("inject-panel").hidden = false;
  $("inject-readonly").hidden = !!state.me.can_write;
  $("inject").hidden = !state.me.can_write;
  if (!state.me.can_write) {
    $("inject-readonly").innerHTML = failed && me instanceof NotSimian && !me.reachedSimian
      ? `Not connected to ${esc(c.name)}, so nothing can be injected there from this page yet.`
      : state.me.auth === "iap"
        ? `${esc(state.me.email)} may view but not inject or clear faults${c.base ? " on " + esc(c.name) : ""}. The chart's <code>ui.iap.writers</code> lists who may.`
        : "Read-only here. Injecting and clearing faults from the browser needs the UI behind Identity-Aware Proxy; from here, use <code>simian chaos</code>.";
    return;
  }
  $("f-arena").innerHTML = "";
  for (const ns of arenas) $("f-arena").add(new Option(ns, ns));
  $("f-arena").value = state.arena || arenas[0] || "";
  loadWorkloads();
  try { state.catalog = await get("/api/catalog"); } catch (e) {
    if (e instanceof Stale) return;
    state.catalog = [];
  }
  $("f-kind").innerHTML = "";
  for (const k of state.catalog) $("f-kind").add(new Option(k.resource_kind + " (" + k.engine + ")", k.resource_kind));
  fillSpec();
  showMode();
}

// bindInject wires the pane once; what it acts on is whichever Simian is
// selected when you act.
function bindInject() {
  $("f-arena").onchange = loadWorkloads;
  $("f-kind").onchange = fillSpec;
  document.querySelectorAll("input[name=mode]").forEach((el) => { el.onchange = showMode; });
  $("f-text").addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) {
      ev.preventDefault();
      $("inject").requestSubmit();
    }
  });
  $("inject").onsubmit = (ev) => {
    ev.preventDefault();
    if (mode() === "intent") describe();
    else review();
  };
  $("f-log").addEventListener("click", onCardClick);
  $("f-log-clear").onclick = () => {
    $("f-log").innerHTML = "";
    welcome();
    attendInject();
  };
  welcome();
  $("active").addEventListener("click", async (ev) => {
    const b = ev.target.closest("button.clear");
    if (!b || !confirm(`Clear ${b.dataset.uid}${sims.list.length > 1 ? " on " + state.ctl.name : ""} now?`)) return;
    b.disabled = true;
    try { await post("/api/faults/" + encodeURIComponent(b.dataset.uid) + "/clear"); refreshSoon(); } catch (e) { alert("Could not clear: " + e.message); b.disabled = false; }
  });
}

const mode = () => document.querySelector("input[name=mode]:checked").value;

function showMode() {
  const intent = mode() === "intent";
  $("f-exact").hidden = intent;
  $("f-workload-label").hidden = intent; // the LLM picks the target
  $("f-kind-label").hidden = intent; // and the kind
  $("f-intent").hidden = !intent;
  $("f-submit").textContent = intent ? "Send" : "Inject…";
  $("f-submit").classList.toggle("send", intent);
  $("f-submit").title = intent
    ? "Ask the LLM for a fault; it proposes one and nothing is applied until you inject it"
    : "Review the fault in the log, then inject it from there";
  $("f-result").textContent = "";
}

// ─── The log ────────────────────────────────────────────────────────
// A terminal both modes write to: what you asked, what Simian proposed,
// and each stage of a fault as the audit trail records it. Newest at the
// bottom; the last LOG_MAX lines; nothing kept after the page goes.

const LOG_MAX = 100;
const clock = () => new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });

// Each Simian has its own log. The one on screen is #f-log; the others
// are kept off-page and swapped in when their Simian is selected, so a
// fault still being followed on one keeps writing to its own history.
const logs = new Map();

// logFor is ctl's log: #f-log when it is the one on screen.
function logFor(ctl) {
  if (!ctl || ctl === state.ctl) return $("f-log");
  if (!logs.has(ctl)) logs.set(ctl, document.createElement("ol"));
  return logs.get(ctl);
}

// swapLog parks the log on screen with from, and brings to's back — or
// starts it with a welcome on its first visit.
function swapLog(from, to) {
  const log = $("f-log");
  const parked = document.createElement("ol");
  parked.append(...log.childNodes);
  if (from) logs.set(from, parked);
  const back = logs.get(to);
  logs.delete(to);
  if (back && back.childNodes.length) {
    log.append(...back.childNodes);
  } else {
    welcome();
  }
  log.scrollTop = log.scrollHeight;
}

// say appends a line to ctl's log (the one on screen by default). who is
// "you", "simian", or "" for a fault's stage (mark is then its ✓/✗); cls
// colours it: ok, bad, warn, info, dim.
function say({ who = "", mark = "", cls = "", html = "", tag = "", detail = "", ctl = null }) {
  const log = logFor(ctl);
  const stick = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
  const li = document.createElement("li");
  li.className = "ln" + (who ? " " + who : " stage") + (cls ? " " + cls : "");
  li.title = new Date().toLocaleString();
  const lead = who ? `<span class="who">${who} ›</span>` : `<span class="mark">${mark}</span>`;
  li.innerHTML = `<span class="ts">${clock()}</span> ${lead} <span class="msg">${html}` +
    (tag ? ` <span class="uid">${esc(tag)}</span>` : "") +
    (detail ? `<span class="detail">${esc(detail)}</span>` : "") + "</span>";
  log.append(li);
  while (log.children.length > LOG_MAX) log.firstElementChild.remove();
  if (stick) log.scrollTop = log.scrollHeight;
  return li;
}

// update rewrites a line said earlier: "translating…" becomes the answer.
function update(li, { cls = "", html = "", detail = "" }) {
  li.classList.remove("working", "ok", "bad", "warn", "info", "dim");
  if (cls) li.classList.add(...cls.split(" "));
  li.querySelector(".msg").innerHTML = html + (detail ? `<span class="detail">${esc(detail)}</span>` : "");
}

function welcome() {
  say({ who: "simian", cls: "dim", html: "Ready. Name a fault (Exact) or say what should go wrong (Describe it). It goes through the CLI's safety checks, and the audit trail records it in your name." });
  // In the room the pane is small where it stands; say how to get more.
  setTimeout(() => {
    if (document.body.classList.contains("room") && $("f-log").clientHeight < 150 && !$("inject-panel").closest(".panel-anchor").classList.contains("centred")) {
      say({ who: "simian", cls: "dim", html: "Short on room here: □ at the top right (or a double-click on the title) brings this pane to the middle; the corner grip resizes it." });
    }
  }, 800);
}

// The inject pane's glow: amber while a fault it started is being
// followed, rose when the last thing it did failed.
let injectTrouble = false;
function attendInject(trouble) {
  if (trouble !== undefined) injectTrouble = trouble;
  const following = [...follows.values()].some((f) => !f.done);
  attend("inject-panel", injectTrouble ? "bad" : following ? "warn" : "");
}

const shortDur = (d) => String(d || "").replace(/(\d)m0s$/, "$1m").replace(/(\d)h0m$/, "$1h");
const short = (uid) => "…" + String(uid).slice(-6);

// ─── Proposals ──────────────────────────────────────────────────────
// Both modes stage the fault as a card in the log before anything is
// applied: the LLM's proposal, or the exact fault for review. The card's
// Inject button is the confirmation.

const proposals = new Map();
let proposalSeq = 0;

function card(p, from) {
  const id = "p" + ++proposalSeq;
  proposals.set(id, Object.assign({ from }, p));
  const spec = JSON.stringify(p.spec || {}, null, 2);
  const oneLine = JSON.stringify(p.spec || {});
  const li = say({ who: "simian", html: from === "llm" ? "Here is the fault I would inject. Nothing is applied until you press Inject." : "Ready to inject. Check it, then press Inject." });
  const box = document.createElement("li");
  box.className = "ln card-ln";
  box.innerHTML = `<div class="proposal" data-id="${id}">
      <div class="p-head"><span class="p-label">${from === "llm" ? "proposal" : "review"}</span><span class="kind">${esc(p.kind)}</span><span class="badge">${esc(p.engine)}</span></div>
      <dl class="p-grid">
        <dt>target</dt><dd>${esc(p.namespace)}/${esc(p.workload || "(whole arena)")}</dd>
        <dt>for</dt><dd>${esc(shortDur(p.duration))}</dd>
        ${p.rationale ? `<dt>why</dt><dd class="p-why">${esc(p.rationale)}</dd>` : ""}
      </dl>
      <details class="p-spec"><summary>spec <span class="p-peek">${esc(oneLine.length > 60 ? oneLine.slice(0, 57) + "…" : oneLine)}</span></summary><pre>${esc(spec)}</pre></details>
      <div class="p-actions">
        <button type="button" class="p-inject" title="Inject this fault now">↯ Inject</button>
        ${from === "llm" ? '<button type="button" class="p-edit" title="Copy it into the Exact form to change it first">Edit as exact</button>' : ""}
        <button type="button" class="p-discard">Discard</button>
        <span class="p-state"></span>
      </div>
    </div>`;
  li.after(box);
  roomToRead(box);
  const log = $("f-log");
  log.scrollTop = log.scrollHeight;
  return id;
}

// roomToRead: in the room the pane is small where it stands. The first
// time a card will not fit in the log, bring the pane to the middle (□,
// Escape or a double-click puts it back); after that, only the tip.
let centredOnce = false;
function roomToRead(box) {
  const room = window.SimianRoom;
  const anchor = $("inject-panel").closest(".panel-anchor");
  if (centredOnce || !room || !document.body.classList.contains("room") || anchor.classList.contains("centred")) return;
  if (box.offsetHeight + 40 <= $("f-log").clientHeight) return;
  centredOnce = true;
  const pane = (room.panes || []).find((p) => p.id === "inject");
  if (pane) room.centre(pane);
}

// settle marks a card used, so its buttons cannot act twice.
function settleCard(el, text) {
  el.classList.add("used");
  el.querySelectorAll("button").forEach((b) => { b.disabled = true; });
  el.querySelector(".p-state").textContent = text;
  proposals.delete(el.dataset.id);
}

// Only the newest card is live: staging another retires the rest.
function retireCards(why = "superseded") {
  document.querySelectorAll("#f-log .proposal:not(.used)").forEach((el) => settleCard(el, why));
}

async function onCardClick(ev) {
  const b = ev.target.closest(".p-actions button");
  if (!b) return;
  const el = b.closest(".proposal");
  const p = proposals.get(el.dataset.id);
  if (!p) return;
  if (b.classList.contains("p-discard")) {
    settleCard(el, "discarded");
    say({ who: "simian", cls: "dim", html: "Discarded. Nothing was applied." });
  } else if (b.classList.contains("p-edit")) {
    settleCard(el, "copied to Exact");
    await editAsExact(p);
  } else if (b.classList.contains("p-inject")) {
    settleCard(el, "submitted");
    submit(p);
  }
}

async function editAsExact(p) {
  document.querySelector("input[name=mode][value=manifest]").checked = true;
  showMode();
  if ($("f-arena").value !== p.namespace) {
    $("f-arena").value = p.namespace;
    await loadWorkloads();
  }
  const wl = $("f-workload");
  if (p.workload && ![...wl.options].some((o) => o.value === p.workload)) wl.add(new Option(p.workload, p.workload));
  wl.value = p.workload || "";
  const i = state.catalog.findIndex((c) => c.resource_kind === p.kind && (!p.engine || c.engine === p.engine));
  if (i >= 0) {
    $("f-kind").selectedIndex = i;
    fillSpec();
  }
  $("f-spec").value = JSON.stringify(p.spec || {}, null, 2);
  $("f-duration").value = shortDur(p.duration);
  say({ who: "simian", cls: "dim", html: "Copied into the Exact form: change what you like, then Inject…" });
  $("f-spec").focus();
}

// review stages the Exact form's fault as a card.
function review() {
  const c = state.catalog[$("f-kind").selectedIndex] || {};
  let spec;
  try { spec = JSON.parse($("f-spec").value || "{}"); } catch (e) {
    $("f-result").className = "bad";
    $("f-result").textContent = "The spec is not valid JSON: " + e.message;
    return;
  }
  if (c.engine === "chaos-mesh" && Object.keys(spec).length === 0) {
    // Chaos Mesh rejects a spec without even a mode, after the executor has
    // accepted it — better said here, with where to start.
    $("f-result").className = "bad";
    $("f-result").textContent = `The spec is empty. ${c.resource_kind} needs at least a "mode" and its settings — open ▸ template for an example.`;
    return;
  }
  $("f-result").textContent = "";
  const p = { engine: c.engine, kind: c.resource_kind, namespace: $("f-arena").value, workload: $("f-workload").value, spec, duration: $("f-duration").value.trim(), ctl: state.ctl };
  retireCards();
  say({ who: "you", html: `${esc(p.kind)} → ${esc(p.namespace)}/${esc(p.workload || "(whole arena)")} for ${esc(p.duration)}` });
  card(p, "exact");
}

// describe asks the LLM for a proposal; it applies nothing.
let translating = false;
async function describe() {
  const intent = $("f-text").value.trim();
  if (!intent) {
    $("f-result").className = "bad";
    $("f-result").textContent = "Say what should go wrong first.";
    return;
  }
  if (translating) return;
  $("f-result").textContent = "";
  translating = true;
  $("f-submit").disabled = true;
  retireCards();
  const body = { namespace: $("f-arena").value, intent, duration: $("f-duration").value.trim() };
  const c = state.ctl;
  say({ who: "you", html: esc(intent) });
  const li = say({ who: "simian", cls: "working", html: `translating with the LLM <span class="muted">(${esc(body.namespace)}, ${esc(body.duration)})</span>` });
  try {
    const { data } = await send("/api/translate", body, c);
    if (state.ctl !== c) {
      update(li, { cls: "dim", html: `translated by ${esc(c.name)}, but the page has moved to another Simian since, so the proposal is dropped. Nothing was applied.` });
      return;
    }
    update(li, { cls: "dim", html: `translated <span class="muted">(${esc(body.namespace)}, ${esc(body.duration)})</span>` });
    card(Object.assign({ ctl: c }, data), "llm");
  } catch (e) {
    if (e instanceof NotSimian) {
      update(li, { cls: "bad", html: esc(e.message) + " Nothing was applied: a translation never applies anything. Try again." });
    } else {
      update(li, { cls: "bad", html: "I could not turn that into a fault. Nothing was applied; try saying it another way, or use Exact.", detail: e.message });
    }
  } finally {
    translating = false;
    $("f-submit").disabled = false;
  }
}

// resetAfterSuccess clears what was sent, keeping where it went: the
// describe text goes; Exact keeps arena, workload and duration and gets
// the kind's template back.
function resetAfterSuccess() {
  $("f-text").value = "";
  fillSpec();
}

// ─── Submitting and following ───────────────────────────────────────
// A submit is an exact one (mode "manifest"), however it was staged. The
// server answers 200 when the executor has finished (applied, injected,
// its effect seen), 422 with the executor's refusal, or 202 "applying"
// when it takes longer than its wait. Either way the fault is followed on
// the event stream by its UID until it has ended and its recovery is
// known, with /api/faults to fill in anything the stream missed.

const follows = new Map(); // fault UID → what is known of it
const byUID = new Map(); // fault UID → the stream's events for it, as they came
const FOLLOW_FOR = 10 * 60 * 1000;
let awaiting = null; // a submit whose UID is not known yet

// remember keeps the stream's events per fault, so a fault followed once
// its UID is known replays what came first.
function remember(e) {
  if (!e.fault_uid) return;
  if (!byUID.has(e.fault_uid)) {
    byUID.set(e.fault_uid, []);
    if (byUID.size > 400) byUID.delete(byUID.keys().next().value);
  }
  byUID.get(e.fault_uid).push(e);
}

// onStream is every event from /api/events.
function onStream(e) {
  const fresh = e.fault_uid && !byUID.has(e.fault_uid);
  remember(e);
  if (fresh && awaiting && e.event === "executor.received" && isOurs(e, awaiting)) adopt(e.fault_uid);
  const f = follows.get(e.fault_uid);
  if (f && !f.done) stage(f, e);
}

// isOurs: the executor received the fault this page just submitted — in
// this user's name, of this kind, at this target — under a UID the stream
// had not shown before.
function isOurs(e, a) {
  const p = e.payload || {};
  const t = (p.targets || [])[0] || {};
  return p.actor === state.me.email && p.kind === a.p.kind && t.namespace === a.p.namespace && (t.name || "") === (a.p.workload || "");
}

function adopt(uid) {
  const a = awaiting;
  if (!a || a.uid) return;
  a.uid = uid;
  if (a.lost) {
    clearTimeout(a.lost);
    say({ who: "simian", cls: "info", html: `Found it on the event stream: <b>${esc(uid)}</b>. Following it live.` });
    awaiting = null;
  }
  follow(uid, a.p);
}

async function submit(p) {
  const body = { mode: "manifest", namespace: p.namespace, workload: p.workload, engine: p.engine, kind: p.kind, spec: p.spec, duration: shortDur(p.duration) };
  say({ who: "you", html: `inject ${esc(p.kind)} → ${esc(p.namespace)}/${esc(p.workload || "(whole arena)")} for ${esc(body.duration)}` });
  const c = p.ctl || state.ctl;
  const li = say({ who: "simian", cls: "working", html: "submitting to the executor" + (sims.list.length > 1 ? " of " + esc(c.name) : "") });
  attendInject(false);
  const a = { p, uid: "", at: Date.now() };
  awaiting = a;
  const started = Date.now();
  try {
    const { status, data } = await send("/api/faults", body, c);
    if (awaiting === a) awaiting = null;
    const secs = Math.round((Date.now() - started) / 1000);
    if (status === 202) {
      update(li, { cls: "warn", html: `<b>${esc(data.fault_uid)}</b> is still applying (waiting for the engine and the probes) after ${secs}s. Following it live.` });
    } else {
      update(li, { cls: "ok", html: `Applied as <b>${esc(data.fault_uid)}</b>: the executor accepted it and its checks passed (${secs}s).` });
    }
    resetAfterSuccess();
    const f = follow(data.fault_uid, p);
    if (status === 202) backfill(f);
  } catch (e) {
    if (e instanceof NotSimian) {
      update(li, { cls: "bad", html: esc(e.message) });
      if (!e.reachedSimian) {
        if (awaiting === a) awaiting = null;
        say({ who: "simian", cls: "dim", html: "Nothing was applied: the request never reached Simian." });
      } else if (a.uid) {
        if (awaiting === a) awaiting = null;
        say({ who: "simian", cls: "warn", html: `The fault was received as <b>${esc(a.uid)}</b> before the connection failed; still following it. Check Active faults too.` });
        backfill(follows.get(a.uid));
      } else {
        say({ who: "simian", cls: "warn", html: "The fault may still have been applied — check Active faults. Watching the event stream for it…" });
        a.lost = setTimeout(() => {
          if (awaiting !== a) return;
          awaiting = null;
          say({ who: "simian", cls: "dim", html: "No sign of it on the event stream after a minute and a half, so it was probably not applied. Active faults and Recent faults have the last word." });
        }, 90 * 1000);
      }
      attendInject(true);
      return;
    }
    if (awaiting === a) awaiting = null;
    const f = a.uid && follows.get(a.uid);
    if (f && f.seen.has("refused")) {
      update(li, { cls: "dim", html: "submitted; the executor refused it" });
    } else if (e.status === 422) {
      update(li, { cls: "dim", html: "submitted; the executor refused it" });
      refused(f, e.message, "");
    } else {
      update(li, { cls: "bad", html: "Simian did not apply it.", detail: e.message });
    }
    if (f) finish(f);
    attendInject(true);
  }
}

// follow starts (or continues) watching a fault by its UID.
function follow(uid, p) {
  if (follows.has(uid)) return follows.get(uid);
  const f = { uid, p, ctl: p.ctl || state.ctl, seen: new Set(), done: false, verifiedBy: null, ended: "", timers: [] };
  follows.set(uid, f);
  f.timers.push(setTimeout(() => {
    if (!f.done) finish(f, "Stopped following after 10 minutes; Recent faults has the rest.");
  }, FOLLOW_FOR));
  f.timers.push(setInterval(() => backfill(f), 8000));
  for (const e of byUID.get(uid) || []) stage(f, e);
  attendInject();
  return f;
}

function finish(f, why) {
  if (f.done) return;
  f.done = true;
  f.timers.forEach((t) => { clearTimeout(t); clearInterval(t); });
  if (f.waiting) f.waiting.remove();
  if (why) say({ who: "simian", cls: "dim", html: esc(why), tag: short(f.uid), ctl: f.ctl });
  attendInject();
}

// once says a stage's line the first time it is seen, whichever of the
// stream and /api/faults tells of it first.
function once(f, key, line) {
  if (f.seen.has(key)) return;
  f.seen.add(key);
  return say(Object.assign({ tag: short(f.uid), ctl: f.ctl }, line));
}

const passedOf = (e) => (e.payload || {}).passed;

// stage turns one audit event for a followed fault into its line.
function stage(f, e) {
  const p = e.payload || {};
  switch (e.event) {
    case "executor.received":
      once(f, "received", { mark: "·", cls: "dim", html: "received by the executor" });
      break;
    case "executor.validated": {
      const probes = p.default_probes || [];
      once(f, "validated", { mark: "✓", cls: "ok", html: "safety checks passed" + (probes.length ? ` <span class="muted">· probes ${esc(probes.join(", "))}</span>` : "") });
      break;
    }
    case "fault.precheck":
      if (passedOf(e) === false) once(f, "precheck", { mark: "✗", cls: "bad", html: `precheck ${esc(p.probe || "")}: the workload was not in the starting state`, detail: p.error || "" });
      else once(f, "precheck", { mark: "✓", cls: "ok", html: `precheck passed <span class="muted">· ${esc(p.probe || "")}</span>` });
      break;
    case "driver.applied": {
      once(f, "validated", { mark: "✓", cls: "ok", html: "safety checks passed" });
      if (Array.isArray(p.verified_by)) f.verifiedBy = p.verified_by;
      const until = p.deadline ? ` until ${time(p.deadline)}` : "";
      once(f, "applied", { mark: "✓", cls: "ok", html: `applied: ${esc(p.kind || f.p.kind)} created in the cluster${until}` + (p.engine_uid ? ` <span class="muted">· object ${esc(p.engine_uid)}</span>` : "") });
      if (f.verifiedBy && !f.verifiedBy.length) once(f, "efficacy", { mark: "–", cls: "dim", html: "nothing checks that it took: this engine reports no status and there is no probe for this kind" });
      break;
    }
    case "fault.injected":
      if (passedOf(e) === true) {
        once(f, "injected", { mark: "✓", cls: "ok", html: "injected: the engine confirms it took" + (p.observed ? ` <span class="muted">· ${esc(p.observed)}</span>` : "") });
        if (f.verifiedBy && !f.verifiedBy.some((v) => v !== "engine-status")) {
          once(f, "efficacy", { mark: "–", cls: "dim", html: "effect seen: no probe for this kind, so the engine's word is the check" });
        }
      } else if (passedOf(e) === false) {
        once(f, "injected", { mark: "✗", cls: "bad", html: "not injected: the engine says it did not take, so Simian takes it back out", detail: p.error || "" });
      } else {
        once(f, "injected", { mark: "?", cls: "warn", html: "Simian stopped before the engine answered" });
      }
      break;
    case "fault.efficacy": {
      const name = p.probe ? ` <span class="muted">· ${esc(p.probe)}${p.observed ? ": " + esc(p.observed) : ""}</span>` : "";
      if (passedOf(e) === true) once(f, "efficacy:" + (p.probe || ""), { mark: "✓", cls: "ok", html: "effect seen" + name });
      else if (passedOf(e) === false) once(f, "efficacy:" + (p.probe || ""), { mark: "✗", cls: "bad", html: "effect not seen, so Simian takes it back out" + name, detail: p.expected ? `expected ${p.expected}` : p.error || "" });
      f.seen.add("efficacy");
      break;
    }
    case "executor.rejected":
      refused(f, p.error || p.message || "", e.reason);
      finish(f);
      attendInject(true);
      break;
    case "driver.failed":
      once(f, "refused", { mark: "✗", cls: "bad", html: "the engine would not create it, so nothing was applied", detail: p.error || "" });
      finish(f);
      attendInject(true);
      break;
    case "lease.cleared":
      if (p.left_to_reaper || e.reason === "driver-clear-failed") {
        once(f, "left", { mark: "!", cls: "warn", html: "Simian could not delete it; the reaper takes it out at the deadline", detail: p.clear_error || p.error || "" });
        break;
      }
      if (e.reason === "explicit-clear") {
        ended(f, `ended: cleared${p.actor ? " by " + esc(p.actor) : ""} before its deadline`);
        finish(f, "No recovery check follows a clear; Workloads shows how the targets are doing.");
      } else {
        ended(f, `ended: taken back out <span class="muted">· ${esc(e.reason || "")}</span>`);
        finish(f);
      }
      break;
    case "lease.expired":
      ended(f, "ended: expired at its deadline");
      awaitRecovery(f);
      break;
    case "fault.recovered":
      recovered(f, passedOf(e), p.unready || [], p.waited);
      break;
  }
}

function ended(f, html) {
  f.ended = html;
  once(f, "ended", { mark: "▪", cls: "info", html });
}

// awaitRecovery: after a fault runs out, the executor checks that a Chaos
// Mesh fault's targets come back Ready (up to 5 minutes). Other engines'
// objects are the fault and go with it, so nothing is checked.
function awaitRecovery(f) {
  if (f.seen.has("recovered") || f.done) return;
  const engine = f.p.engine;
  if (engine && engine !== "chaos-mesh") {
    finish(f, "No recovery check for this engine: its objects are the fault and went with it.");
    return;
  }
  if (!f.waiting) f.waiting = say({ who: "simian", cls: "working dim", html: "checking that the workload recovers", tag: short(f.uid), ctl: f.ctl });
  f.timers.push(setTimeout(() => finish(f, "No recovery verdict was recorded within 6 minutes."), 6 * 60 * 1000));
}

function recovered(f, passed, unready, waited) {
  if (f.waiting) { f.waiting.remove(); f.waiting = null; }
  if (passed === true) {
    once(f, "recovered", { mark: "✓", cls: "ok", html: "recovered: the targets are Ready again" + (waited ? ` <span class="muted">· after ${esc(waited)}</span>` : "") });
  } else if (passed === false) {
    once(f, "recovered", { mark: "✗", cls: "bad", html: "not recovered" + (waited ? ` after ${esc(waited)}` : "") + (unready.length ? ": still not Ready" : ""), detail: unready.join("; ") });
    attendInject(true);
  } else {
    return;
  }
  finish(f, "Done.");
}

// ─── Refusals in plain words ────────────────────────────────────────

const STAGES = {
  safety: "the safety checks", schema: "schema validation", precheck: "the precheck", probe: "the settle probe",
  driver: "the engine", lease: "the lease", audit: "the audit trail",
};

function refusalWords(reason, msg) {
  const x = (state.config || {}).executor || {};
  switch (reason) {
    case "workload-excluded": return "that workload is excluded from chaos in this arena.";
    case "namespace-not-eligible": return "that namespace is not an arena: it has not opted in to chaos.";
    case "duration-over-ceiling": return /positive/.test(msg) ? "the duration must be more than zero." : `the duration is longer than this install allows${x.duration_ceiling ? " (" + x.duration_ceiling + ")" : ""}.`;
    case "budget-exceeded":
      if (/cooldown/.test(msg)) return `this arena had a fault recently and is cooling down${x.min_cooldown ? " (" + x.min_cooldown + " between faults)" : ""}.`;
      if (/being applied/.test(msg)) return "another fault is being applied to this arena right now.";
      return `a fault is already running, and this install allows ${x.max_concurrent_faults || "a limited number"} at a time. Wait for it to end, or clear it.`;
    case "tier-not-permitted": return `its blast radius is wider than this install permits${(x.permitted_tiers || []).length ? " (" + x.permitted_tiers.join(", ") + ")" : ""}.`;
    case "target-incompatible": return "the target cannot take this fault as written (for example, it has no container or port the spec names).";
    case "precheck-failed": return "the workload was not healthy before the fault, so a probe could not have proved anything. Nothing was applied.";
    case "cannot-gate": return "the probe that would verify this fault could not run, and Simian does not apply a fault it cannot verify.";
    case "probe-failed": return "it was applied, but the probe saw no effect, so Simian took it back out.";
    case "injection-failed": return "it was applied, but the engine says it did not take, so Simian took it back out.";
    case "schema-invalid": return "the spec does not match the engine's schema for this kind.";
    case "unknown-gvk": return "that kind is not installed in the cluster.";
    case "rbac-denied": return "Simian's service account is not allowed to create that object.";
    case "driver-failed": return "the engine would not create the object.";
    case "lease-failed": return "Simian could not register the fault's lease, so it did not run.";
    case "interrupted": return "Simian stopped (a restart?) before the outcome was known.";
    case "probe-not-configured": return "the fault needs a probe, and this install has no prober.";
    default: return "";
  }
}

// refused says why the executor said no. text is the server's (an
// ExecutorError reads "executor[stage:reason]: message"); reason is the
// event's, when it came from the stream.
function refused(f, text, reason) {
  const m = /executor\[([\w-]+):([\w-]+)\]:\s*(.*)/s.exec(text || "");
  const stg = m ? m[1] : "";
  reason = reason || (m ? m[2] : "");
  const msg = m ? m[3] : text;
  const words = refusalWords(reason, msg);
  const backOut = reason === "probe-failed" || reason === "injection-failed";
  const lead = backOut ? "Taken back out" : "Refused" + (STAGES[stg] ? " by " + STAGES[stg] : "");
  const line = {
    mark: "✗", cls: "bad",
    html: `<b>${lead}</b>${reason ? ` <span class="reason">${esc(reason)}</span>` : ""}: ${esc(words || msg || "the executor said no.")}`,
    detail: words ? text : "",
  };
  if (f) return once(f, "refused", line);
  return say(line);
}

// backfill reads /api/faults for a followed fault, for whatever the
// stream missed (a reconnect, or a page that was busy).
async function backfill(f) {
  if (!f || f.done) return;
  let rows;
  try { rows = await get("/api/faults?limit=50&namespace=" + encodeURIComponent(f.p.namespace), f.ctl); } catch (e) { return; }
  const r = rows.find((x) => x.fault_uid === f.uid);
  if (!r || f.done) return;
  const ev = (event, extra) => stage(f, Object.assign({ event, fault_uid: f.uid, payload: {} }, extra));
  ev("executor.received");
  if (r.applied_at) ev("driver.applied", { payload: { kind: r.kind, engine_uid: r.engine_uid, deadline: r.deadline } });
  if (r.injected != null) ev("fault.injected", { payload: { passed: r.injected } });
  if (r.efficacy != null && !f.seen.has("efficacy")) ev("fault.efficacy", { payload: { passed: r.efficacy } });
  switch (r.outcome) {
    case "refused": ev("executor.rejected", { reason: r.reason, payload: { error: r.error } }); return;
    case "driver-failed": ev("driver.failed", { payload: { error: r.error } }); return;
    case "cleared":
      if (r.recovered != null) ev("fault.recovered", { payload: { passed: r.recovered, unready: r.unready } });
      ev("lease.cleared", { reason: r.reason, payload: { actor: r.cleared_by } });
      return;
    case "expired": ev("lease.expired", { reason: r.reason }); break;
  }
  if (r.recovered != null) ev("fault.recovered", { payload: { passed: r.recovered, unready: r.unready } });
}

function backfillAll() {
  for (const f of follows.values()) backfill(f);
}

// ─── Several Simians ────────────────────────────────────────────────
// The page can watch several Simian controllers: the one serving it (if
// any), the others its deployment lists (/api/controllers, from
// --ui-controllers or `simian web --controllers`), and any the user adds,
// kept in this browser. The browser calls each directly, signed in to
// each, so every controller checks who the user is and records them in
// its own audit trail; nothing is proxied. The panes, the event stream,
// the configuration dialog and HALT are the selected Simian's; the status
// strip polls the others lightly.

const SIMS_KEY = "simian:controllers"; // {added: [{name, url}], hidden: [url]}
const SEL_KEY = "simian:controller"; // the selected Simian's URL
const POLL_MS = 15000;

const sims = { list: [], hiddenSeeded: [], standalone: false, store: { added: [], hidden: [] } };

function readSims() {
  try {
    const v = JSON.parse(localStorage.getItem(SIMS_KEY) || "{}");
    return { added: Array.isArray(v.added) ? v.added : [], hidden: Array.isArray(v.hidden) ? v.hidden : [] };
  } catch (e) { return { added: [], hidden: [] }; }
}

function writeSims() {
  try { localStorage.setItem(SIMS_KEY, JSON.stringify(sims.store)); } catch (e) { /* this visit only */ }
}

function keep(key, value) {
  try { if (value) localStorage.setItem(key, value); else localStorage.removeItem(key); } catch (e) { /* this visit only */ }
}

// originOf reads what someone typed as a controller's URL: its origin, or
// an error saying why not. Only https, and http on this machine for
// development, can be called from the page (its connect-src).
function originOf(text) {
  let u;
  try { u = new URL(String(text).trim()); } catch (e) { throw new Error("That is not a URL; give the controller UI's address, e.g. https://simian-2.example.com."); }
  if (u.protocol !== "https:" && u.protocol !== "http:") throw new Error("Give an https:// address, e.g. https://simian-2.example.com.");
  if (u.protocol === "http:" && !["localhost", "127.0.0.1"].includes(u.hostname)) {
    throw new Error("This page may call https:// addresses only (and http://localhost while developing). Use the controller UI's https:// address.");
  }
  if (!["", "/", "/ui", "/ui/"].includes(u.pathname) || u.search || u.hash) throw new Error("Give just the controller UI's origin, e.g. https://simian-2.example.com — the page calls /api/ there.");
  return u.origin;
}

function mkCtl(o) {
  return Object.assign({ base: "", url: "", name: "", self: false, seeded: false, added: false,
    st: { conn: "unknown", why: "", active: null, auto: "", halted: false, role: "", version: "", auth: "" } }, o);
}

// Connection states, as the strip and the list word them.
const CONN = {
  unknown: "connecting", live: "live", signin: "needs sign-in", blocked: "not allowed / sign in",
  unreachable: "not reachable", error: "error",
};

// The same, short enough for the switcher.
const SHORT = { unknown: "…", signin: "sign in", blocked: "sign in?", unreachable: "unreachable", error: "error" };

function noteStatus(c, patch) {
  Object.assign(c.st, patch);
}

function noteFailure(c, e) {
  if (e instanceof Stale) return;
  const conn = !(e instanceof NotSimian) ? "error"
    : e.why === "crossSignin" || e.why === "signin" ? "signin"
      : e.why === "blocked" ? "blocked"
        : e.why === "unreachable" || e.why === "network" ? "unreachable" : "error";
  const why = e instanceof NotSimian ? e.message
    : e.status === 404 ? `${c.name} answered 404: ${c.url} is not a Simian controller (the standalone UI, or an older Simian without this API).`
      : `${c.name}: ${e.message}`;
  noteStatus(c, { conn, why });
}

const roleOf = (you) => (you.can_admin ? "admin" : you.can_write ? "writer" : "viewer");

function noteConfig(c, cfg) {
  const au = cfg.autonomous;
  const s = au ? au.settings : null;
  noteStatus(c, {
    version: cfg.version || c.st.version,
    auth: (cfg.ui || {}).auth || c.st.auth,
    role: cfg.you ? roleOf(cfg.you) : c.st.role,
    auto: !s ? "" : !s.enabled ? "off" : isPaused(s, "*") ? "paused" : "on",
    halted: !!(s && (s.paused || []).includes("*")),
  });
}

// pollOne reads what the strip shows of a Simian that is not selected.
async function pollOne(c) {
  const [info, active, config] = await Promise.allSettled([get("/api/info", c), get("/api/active", c), get("/api/config", c)]);
  if (info.status === "rejected") {
    noteFailure(c, info.reason);
  } else {
    noteStatus(c, { conn: "live", why: "", version: info.value.version || c.st.version });
    if (active.status === "fulfilled") c.st.active = active.value.length;
    if (config.status === "fulfilled") noteConfig(c, config.value);
  }
  renderSims();
}

function pollOthers() {
  if (document.hidden) return;
  for (const c of sims.list) if (c !== state.ctl) pollOne(c);
}

// discover builds the list: this page's Simian (unless /api/info is 404,
// the standalone UI), the deployment's, and the user's.
async function discover() {
  sims.store = readSims();
  const self = mkCtl({ base: "", url: here(), name: location.host, self: true });
  let seeded = [];
  try {
    const info = await get("/api/info", self);
    if (info.name) self.name = info.name;
    state.autonomous = info.autonomous || [];
    noteStatus(self, { conn: "live", version: info.version });
  } catch (e) {
    if (e.status === 404) sims.standalone = true;
    else noteFailure(self, e);
  }
  try { seeded = await get("/api/controllers", self); } catch (e) { /* an older controller lists none */ }
  const list = sims.standalone ? [] : [self];
  const has = (url) => list.some((c) => c.url === url);
  const listed = new Set();
  for (const x of Array.isArray(seeded) ? seeded : []) {
    let url;
    try { url = originOf(x.url); } catch (e) { continue; }
    listed.add(url);
    if (has(url)) continue;
    const c = mkCtl({ base: url, url, name: x.name || new URL(url).host, seeded: true });
    if (sims.store.hidden.includes(url)) sims.hiddenSeeded.push(c);
    else list.push(c);
  }
  // A hidden one the deployment no longer lists is forgotten, so it comes
  // back if the deployment lists it again.
  sims.store.hidden = sims.store.hidden.filter((u) => listed.has(u));
  for (const x of sims.store.added) {
    let url;
    try { url = originOf(x.url); } catch (e) { continue; }
    if (has(url) || sims.hiddenSeeded.some((c) => c.url === url)) continue;
    list.push(mkCtl({ base: url, url, name: x.name || new URL(url).host, added: true }));
  }
  writeSims();
  sims.list = list;
}

// select points every pane, the stream, the configuration and HALT at c.
async function select(c) {
  if (!c || state.ctl === c) return;
  const first = !state.ctl;
  const prev = state.ctl;
  state.ctl = c;
  keep(SEL_KEY, c.self ? "" : c.url);
  Object.assign(state, { arenas: [], me: {}, catalog: [], config: null, adminFilled: false, halted: false, faultUIDs: new Set(), autonomous: [], active: [], cycles: [] });
  if (!first) {
    state.arena = "";
    history.replaceState(null, "", location.pathname + location.search);
    $("admin").hidden = true;
    $("btn-halt").hidden = true;
    $("hud-halted").hidden = true;
    $("st-halted").hidden = true;
    document.body.classList.remove("halted");
    // A staged card belongs to the Simian it was staged on.
    retireCards("another Simian selected");
    const firstVisit = !logs.has(c);
    swapLog(prev, c);
    if (firstVisit && !$("inject").hidden && sims.list.length > 1) say({ who: "simian", cls: "info", html: `This is <b>${esc(c.name)}</b>'s log: what you inject or clear from here goes to ${esc(c.name)}, in your name there.` });
  }
  const loading = `<div class="empty loading">Loading ${esc(c.name)}…</div>`;
  for (const id of ["active", "plan", "cycles", "faults", "workloads"]) $(id).innerHTML = loading;
  $("active-count").textContent = "";
  $("events").innerHTML = "";
  $("version").textContent = c.st.version ? "v" + c.st.version : "";
  renderSims();
  connect();
  await loadController(c);
  refresh();
}

// loadController reads what the HUD and the inject pane need from c.
async function loadController(c) {
  const [info, arenas, me] = await Promise.allSettled([get("/api/info", c), get("/api/arenas", c), get("/api/me", c)]);
  if (state.ctl !== c) return;
  if (info.status === "fulfilled") {
    state.autonomous = info.value.autonomous || [];
    noteStatus(c, { version: info.value.version });
    $("version").textContent = "v" + info.value.version;
  }
  const list = arenas.status === "fulfilled" ? arenas.value : [];
  state.arenas = list;
  $("arena").innerHTML = '<option value="">All arenas</option>';
  for (const ns of list) $("arena").add(new Option(ns, ns));
  if (state.arena && !list.includes(state.arena)) {
    if (arenas.status === "fulfilled") state.arena = "";
  }
  $("arena").value = state.arena;
  await fillInject(c, me.status === "fulfilled" ? me.value : me.reason, list);
  renderSims();
}

// retry reads a Simian again — after signing in to it, say.
function retry(c) {
  if (c !== state.ctl) return pollOne(c);
  return loadController(c).then(() => {
    refresh();
    if (!es || es.readyState === EventSource.CLOSED) connect();
  });
}

// signIn opens c's own page in a new tab: Identity-Aware Proxy signs you
// in only on a top-level visit. Back here, the page tries c again.
function signIn(c) {
  window.open(c.url + "/ui/", "_blank", "noopener");
}

function retryTroubled() {
  for (const c of sims.list) if (c.st.conn !== "live") retry(c);
}

function renderSims() {
  const multi = sims.list.length > 1;
  document.body.classList.toggle("multi", multi);
  renderSwitcher(multi);
  renderStrip(multi);
  renderAlert();
  if ($("sims-dialog").open) renderSimsDialog();
}

const attnOf = (c) => (c.st.halted || ["unreachable", "error"].includes(c.st.conn) ? "bad" : c.st.active ? "warn" : "");

function factsOf(c) {
  if (c.st.conn !== "live") return CONN[c.st.conn];
  return [c.st.active == null ? "" : c.st.active + " active", c.st.auto ? "auto " + c.st.auto : "", c.st.role].filter(Boolean).join(" · ");
}

function renderSwitcher(multi) {
  const c = state.ctl;
  $("simian-name").hidden = multi;
  $("simian-name").textContent = c ? c.name : "pick a Simian";
  $("simian").hidden = !multi;
  $("btn-sims").hidden = !multi;
  if (multi) {
    const sel = $("simian");
    sel.innerHTML = (c ? "" : '<option value="">pick a Simian</option>') + sims.list.map((x, i) =>
      `<option value="${i}">${esc(x.name)}${x.st.conn === "live" ? "" : " · " + esc(SHORT[x.st.conn])}</option>`).join("");
    sel.value = c ? String(sims.list.indexOf(c)) : "";
  }
}

function renderStrip(multi) {
  $("st-sims").hidden = !multi;
  if (!multi) { $("st-sims").innerHTML = ""; return; }
  $("st-sims").innerHTML = sims.list.map((c, i) => {
    const on = c === state.ctl;
    const title = `${c.name} — ${c.url}${c.self ? " (this page)" : ""}\n${CONN[c.st.conn]}${c.st.why ? ": " + c.st.why : ""}` +
      (c.st.conn === "live" ? `\n${c.st.active ?? "?"} faults running · autonomous ${c.st.auto || "–"}${c.st.halted ? " · HALTED" : ""} · you are ${c.st.role || "?"} there` : "") +
      (on ? "" : "\nClick to watch it");
    return `<button type="button" class="sim-chip conn-${c.st.conn}${attnOf(c) ? " attn-" + attnOf(c) : ""}${on ? " on" : ""}" data-i="${i}" aria-pressed="${on}" title="${esc(title)}">` +
      `<span class="sim-dot" aria-hidden="true"></span><span class="sim-name">${esc(c.name)}</span>` +
      `<span class="sim-facts">${esc(factsOf(c))}</span>${c.st.halted ? '<span class="sim-halted">HALTED</span>' : ""}</button>`;
  }).join("");
}

// renderAlert explains a selected Simian on another origin that does not
// answer, with what to do; this page's own keeps today's quieter signs.
function renderAlert() {
  const c = state.ctl;
  const trouble = !c || (c.base && !["live", "unknown"].includes(c.st.conn));
  $("sim-alert").hidden = !trouble;
  if (!trouble) return;
  if (!c) {
    $("sim-alert-text").textContent = sims.list.length
      ? "No Simian serves this page. Pick one to watch; the first time, sign in to it."
      : "No Simian serves this page, and none is listed. Add one by its URL.";
    $("sim-alert-signin").hidden = true;
    $("sim-alert-retry").hidden = true;
    return;
  }
  $("sim-alert-text").textContent = c.st.why || `${c.name}: ${CONN[c.st.conn]}`;
  $("sim-alert-signin").hidden = false;
  $("sim-alert-signin").textContent = "Sign in to " + c.name;
  $("sim-alert-retry").hidden = false;
}

// ─── The Simians dialog ─────────────────────────────────────────────

function renderSimsDialog() {
  const c = state.ctl;
  $("sims-lead").textContent = !c && sims.standalone
    ? "No Simian serves this page. Pick one below to watch. The first time, sign in to it: that opens its own page in a new tab, and this page tries again when you come back."
    : "Watch several Simians from this page. Your browser calls each directly, signed in as you there, so each one checks who you are and records what you do in its own audit trail.";
  $("sims-table").innerHTML = sims.list.length
    ? `<thead><tr><th>Simian</th><th>URL</th><th>State</th><th>Version</th><th>Auth</th><th>You</th><th></th></tr></thead><tbody>` +
      sims.list.map((x, i) => {
        const where = x.self ? "this page" : x.seeded ? "from the deployment" : "added by you";
        const cls = x.st.conn === "live" ? (attnOf(x) || "ok") : ["unreachable", "error"].includes(x.st.conn) ? "bad" : "warn";
        return `<tr class="${x === c ? "on" : ""}">
          <td><b>${esc(x.name)}</b><div class="muted">${where}</div></td>
          <td class="url">${esc(x.url)}</td>
          <td><span class="badge ${cls}">${esc(CONN[x.st.conn])}</span>${x.st.halted ? ' <span class="badge bad">halted</span>' : ""}${x.st.conn === "live" && x.st.active != null ? ` <span class="muted">${x.st.active} active</span>` : ""}
            ${x.st.why ? `<div class="muted why">${esc(x.st.why)}</div>` : ""}</td>
          <td>${esc(x.st.version || "–")}</td>
          <td>${esc(x.st.auth || "–")}</td>
          <td>${esc(x.st.role || "–")}</td>
          <td class="acts">
            ${x === c ? '<span class="badge info">watching</span>' : `<button type="button" data-act="watch" data-i="${i}">Watch</button>`}
            ${!x.self && x.st.conn !== "live" ? `<button type="button" data-act="signin" data-i="${i}">Sign in</button>` : ""}
            ${x.st.conn !== "live" ? `<button type="button" data-act="retry" data-i="${i}">Retry</button>` : ""}
            ${x.seeded ? `<button type="button" data-act="hide" data-i="${i}" title="Hide it in this browser; the deployment still lists it">Hide</button>` : ""}
            ${x.added ? `<button type="button" class="danger" data-act="remove" data-i="${i}">Remove</button>` : ""}
          </td></tr>`;
      }).join("") + "</tbody>"
    : '<tbody><tr><td class="empty">No Simians yet. Add one by its URL below.</td></tr></tbody>';
  $("sims-hidden").innerHTML = sims.hiddenSeeded.length
    ? "Hidden in this browser: " + sims.hiddenSeeded.map((x, i) => `${esc(x.name)} <button type="button" data-act="unhide" data-i="${i}">Show</button>`).join(" ")
    : "";
  $("sims-origin").textContent = here();
}

function openSims() {
  renderSimsDialog();
  if (!$("sims-dialog").open) $("sims-dialog").showModal();
}

// afterLoss picks another Simian when the selected one leaves the list.
function afterLoss(c) {
  if (state.ctl !== c) return;
  state.ctl = null;
  if (sims.list.length) select(sims.list[0]);
  else { connect(); renderSims(); }
}

function addSim(ev) {
  ev.preventDefault();
  const out = $("sims-add-result");
  out.className = "";
  let url;
  try { url = originOf($("sims-url").value); } catch (e) { out.className = "bad"; out.textContent = e.message; return; }
  const name = $("sims-name").value.trim() || new URL(url).host;
  const hidden = sims.hiddenSeeded.findIndex((x) => x.url === url);
  if (sims.list.some((x) => x.url === url)) { out.className = "bad"; out.textContent = "That Simian is already in the list."; return; }
  let c;
  if (hidden >= 0) {
    c = sims.hiddenSeeded.splice(hidden, 1)[0];
    sims.store.hidden = sims.store.hidden.filter((u) => u !== url);
  } else {
    c = mkCtl({ base: url, url, name, added: true });
    sims.store.added.push({ name, url });
  }
  writeSims();
  sims.list.push(c);
  $("sims-url").value = "";
  $("sims-name").value = "";
  out.className = "ok";
  out.textContent = `Added ${c.name}. Checking it…`;
  renderSims();
  pollOne(c).then(() => { out.textContent = `Added ${c.name}: ${CONN[c.st.conn]}.`; });
}

function onSimsAction(ev) {
  const b = ev.target.closest("button[data-act]");
  if (!b) return;
  const i = Number(b.dataset.i);
  const act = b.dataset.act;
  if (act === "unhide") {
    const c = sims.hiddenSeeded.splice(i, 1)[0];
    sims.store.hidden = sims.store.hidden.filter((u) => u !== c.url);
    writeSims();
    sims.list.push(c);
    renderSims();
    pollOne(c);
    return;
  }
  const c = sims.list[i];
  if (!c) return;
  if (act === "watch") { $("sims-dialog").close(); select(c); }
  else if (act === "signin") signIn(c);
  else if (act === "retry") retry(c);
  else if (act === "hide" || act === "remove") {
    if (act === "remove" && !confirm(`Remove ${c.name} from this browser's list?`)) return;
    sims.list.splice(i, 1);
    if (act === "hide") { sims.store.hidden.push(c.url); sims.hiddenSeeded.push(c); }
    else sims.store.added = sims.store.added.filter((x) => x.url !== c.url);
    writeSims();
    afterLoss(c);
    renderSims();
  }
}

function bindSims() {
  $("simian-name").onclick = openSims;
  $("btn-sims").onclick = openSims;
  $("simian").onchange = () => { const c = sims.list[Number($("simian").value)]; if (c) select(c); };
  $("st-sims").addEventListener("click", (ev) => {
    const b = ev.target.closest(".sim-chip");
    if (b) select(sims.list[Number(b.dataset.i)]);
  });
  $("sims-close").onclick = () => $("sims-dialog").close();
  $("sims-add").onsubmit = addSim;
  $("sims-dialog").addEventListener("click", onSimsAction);
  $("sim-alert-signin").onclick = () => { if (state.ctl) signIn(state.ctl); };
  $("sim-alert-retry").onclick = () => { if (state.ctl) retry(state.ctl); };
  $("sim-alert-pick").onclick = openSims;
  // Back from signing in (or from anywhere): try whoever was not answering.
  window.addEventListener("focus", retryTroubled);
}

async function main() {
  setupAdmin();
  setupHalt();
  bindInject();
  bindSims();
  $("arena").onchange = () => {
    state.arena = $("arena").value;
    location.hash = state.arena ? encodeURIComponent(state.arena) : "";
    $("events").innerHTML = "";
    refresh();
  };
  setInterval(() => document.querySelectorAll("[data-deadline]").forEach((el) => { el.textContent = remaining(el.dataset.deadline); }), 1000);
  await discover();
  let stored = null;
  try { stored = localStorage.getItem(SEL_KEY); } catch (e) { /* none */ }
  const first = sims.list.find((c) => stored && c.url === stored) || sims.list.find((c) => c.self) ||
    (sims.standalone ? null : sims.list[0]);
  renderSims();
  if (first) await select(first);
  else if (sims.standalone && sims.list.length === 1) await select(sims.list[0]);
  else {
    for (const id of ["active", "plan", "faults", "workloads"]) $(id).innerHTML = '<div class="empty">Pick a Simian to watch.</div>';
    openSims();
  }
  setInterval(refresh, 5000);
  setInterval(pollOthers, POLL_MS);
  pollOthers();
}

main();
