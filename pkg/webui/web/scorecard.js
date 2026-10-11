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

// Scorecard mode: the page pointed at a recording instead of a live
// controller ("One site, two data sources" in web-ui-design.md). The
// recording is the JSON document `simian-eval report` writes — every run's
// scores, the faults, the ground truth and each subject's report — so
// nothing here needs a server: it opens from ?scorecard=<url>, from a file
// dropped on the page or picked, or from the list `simian web --scorecards`
// serves.
//
// Two rules carry the weight (issue #72):
//   1. A scenario whose fault did not land in a run is *not measured* there,
//      never zero. Its cell shows a dash and says so, whatever the document
//      says its value is.
//   2. The deterministic detector (k8s-lookout) is the floor, not a peer:
//      its column is marked, and a subject below it on a row is flagged as
//      such — a different failure from a plain low score.
//
// The live panes keep running behind this mode; app.js knows only enter(),
// leave() and boot().
(function () {
  "use strict";

  const FORMAT = "simian.scorecard";
  const VERSION = 1;

  const $ = (id) => document.getElementById(id);
  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const body = document.body;

  const sc = { doc: null, source: "", selected: null, openSims: null, standalone: false };

  // ─── Formatting ───────────────────────────────────────────────────

  const isSet = (t) => !!t && !String(t).startsWith("0001");
  const when = (t) => (isSet(t) ? new Date(t).toLocaleString([], { dateStyle: "medium", timeStyle: "short" }) : "–");
  const clock = (t) => (isSet(t) ? new Date(t).toLocaleTimeString([], { hour12: false }) : "–");
  const frac = (v) => (typeof v === "number" ? v.toFixed(2) : "–");
  // Seconds as the text scorecard prints them, so the two can be compared.
  const secs = (v) => (typeof v === "number" ? v.toFixed(1) + "s" : "–");
  const unitOf = (doc, name) => ((doc.measures || []).find((m) => m.name === name) || {}).unit || "fraction";
  const fmt = (doc, name, v) => (unitOf(doc, name) === "seconds" ? secs(v) : frac(v));
  const label = (name) => String(name || "").replace(/_/g, " ");

  function scoreOf(cell, name) {
    return (cell && (cell.scores || []).find((s) => s.name === name)) || null;
  }

  // measured is the one place Rule 1 is decided: a cell is measured only if
  // the fault landed in that run and the headline measure applied.
  function measured(cell) {
    return !!cell && cell.manifested === true && typeof cell.value === "number";
  }

  // ─── Loading ──────────────────────────────────────────────────────

  function check(doc) {
    if (!doc || typeof doc !== "object") throw new Error("not a JSON object");
    if (doc.format !== FORMAT) throw new Error(`not a Simian scorecard (format is ${JSON.stringify(doc.format ?? null)}, want "${FORMAT}"); make one with simian-eval report`);
    if (doc.version > VERSION) throw new Error(`scorecard version ${doc.version} is newer than this page reads (${VERSION}); open it in a newer Simian`);
    if (!Array.isArray(doc.runs) || !Array.isArray(doc.scenarios)) throw new Error("the scorecard has no runs or scenarios");
    return doc;
  }

  function show(doc, source) {
    sc.doc = check(doc);
    sc.source = source;
    sc.selected = null;
    $("sc-error").hidden = true;
    render();
    const first = sc.doc.scenarios.find((s) => Object.values(s.results || {}).some((c) => c.below_floor)) ||
      sc.doc.scenarios.find((s) => Object.values(s.results || {}).some((c) => !c.manifested)) || sc.doc.scenarios[0];
    if (first) select(keyOf(first));
  }

  function fail(why) {
    sc.doc = null;
    render();
    const el = $("sc-error");
    el.textContent = "Could not open the scorecard: " + why;
    el.hidden = false;
  }

  // Only this site, or https: the page's CSP allows https: (for other
  // Simians) and this origin, and a scorecard from anywhere else would be
  // refused by the browser with a less useful message.
  function resolve(raw) {
    const u = new URL(raw, location.href);
    if (u.origin !== location.origin && u.protocol !== "https:") throw new Error(`${u.href}: only this site or https:// URLs`);
    return u;
  }

  async function loadURL(raw, { remember = true } = {}) {
    enter();
    let u;
    try {
      u = resolve(raw);
    } catch (e) {
      return fail(e.message);
    }
    try {
      const r = await fetch(u.href, { cache: "no-store", credentials: u.origin === location.origin ? "same-origin" : "omit" });
      if (!r.ok) throw new Error(`${u.href}: HTTP ${r.status}`);
      show(JSON.parse(await r.text()), u.origin === location.origin ? u.pathname + u.search : u.href);
      if (remember) {
        const q = new URLSearchParams(location.search);
        q.set("scorecard", raw);
        history.replaceState(null, "", location.pathname + "?" + q.toString() + location.hash);
      }
    } catch (e) {
      fail(e.message);
    }
  }

  function loadFile(file) {
    enter();
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => {
      try {
        show(JSON.parse(String(reader.result)), file.name);
        forgetURL();
      } catch (e) {
        fail(`${file.name}: ${e.message}`);
      }
    };
    reader.onerror = () => fail(`${file.name}: could not be read`);
    reader.readAsText(file);
  }

  function forgetURL() {
    const q = new URLSearchParams(location.search);
    if (!q.has("scorecard")) return;
    q.delete("scorecard");
    const rest = q.toString();
    history.replaceState(null, "", location.pathname + (rest ? "?" + rest : "") + location.hash);
  }

  // The list simian web --scorecards serves. Asked only of the standalone
  // page: a controller serves no such list, and its 404 would be noise.
  async function loadServed() {
    if (location.protocol === "file:") return;
    try {
      const r = await fetch("/scorecards/", { cache: "no-store" });
      if (!r.ok) return;
      const names = await r.json();
      if (!Array.isArray(names) || !names.length) return;
      $("sc-served-list").innerHTML = names.map((n) =>
        `<li><button type="button" class="sc-served-btn" data-url="/scorecards/${esc(encodeURIComponent(n))}">${esc(n)}</button></li>`).join("");
      $("sc-served").hidden = false;
    } catch (e) { /* no list here */ }
  }

  // ─── Mode ─────────────────────────────────────────────────────────

  const active = () => body.classList.contains("mode-scorecard");

  function setMode(scorecard) {
    if (window.SimianRoom) window.SimianRoom.setMode(scorecard ? "scorecard" : "live");
    else body.classList.toggle("mode-scorecard", scorecard);
    $("btn-mode-live").setAttribute("aria-pressed", String(!scorecard));
    $("btn-mode-scorecard").setAttribute("aria-pressed", String(scorecard));
  }

  function enter() {
    if (!active()) setMode(true);
    renderChrome();
  }

  function leave() {
    if (active()) setMode(false);
  }

  // ─── Rendering ────────────────────────────────────────────────────

  function renderChrome() {
    const d = sc.doc;
    $("hud-sc-name").textContent = d ? sc.source : "";
    $("hud-sc-name").title = d ? sc.source : "";
    $("btn-sc-open").hidden = !d;
    $("st-scorecard").textContent = d
      ? `scorecard: ${sc.source} · ${d.runs.length} run${d.runs.length === 1 ? "" : "s"} · ${d.scenarios.length} scenario${d.scenarios.length === 1 ? "" : "s"} · ${d.packs ? d.packs.join(", ") : ""}`
      : "scorecard: none open";
    $("sc-watch").hidden = !(sc.standalone && sc.openSims);
  }

  function render() {
    const d = sc.doc;
    $("sc-landing").hidden = !!d;
    $("sc-doc").hidden = !d;
    $("sc-count").textContent = d ? `${d.scenarios.length} × ${d.runs.length}` : "";
    renderChrome();
    if (!d) {
      $("sc-detail").innerHTML = '<div class="empty">Open a scorecard to see its scenarios.</div>';
      $("sd-name").textContent = "";
      return;
    }
    $("sc-runs").innerHTML = renderRuns(d);
    $("sc-grid").innerHTML = renderGrid(d);
    // Through the CSSOM: the page's CSP allows no inline style attributes.
    $("sc-grid").querySelectorAll("[data-w]").forEach((el) => el.style.setProperty("--w", (Number(el.dataset.w) * 100).toFixed(1) + "%"));
    $("sc-legend").innerHTML = renderLegend(d);
  }

  const floorNames = (d) => (d.floor && d.floor.subjects && d.floor.subjects.length ? d.floor.subjects.join(", ") : "");

  function renderRuns(d) {
    const floor = floorNames(d);
    const head = `<div class="sc-meta">
      <span>${d.runs.length} run${d.runs.length === 1 ? "" : "s"} · pack${d.packs && d.packs.length === 1 ? "" : "s"} <b>${esc((d.packs || []).join(", "))}</b></span>
      <span class="muted">generated ${esc(when(d.generated_at))}${d.generator ? " by " + esc(d.generator) : ""}</span>
      <span class="muted">cells show <b>${esc(label(d.headline))}</b> (controls: ${esc(label(d.control_headline))})</span>
    </div>`;
    const warn = [];
    if (d.mixed_packs) warn.push(`<div class="sc-warn info">These runs used different packs (${esc(d.packs.join(", "))}). Scenarios are grouped by pack; a run has no cell in a pack it did not run.</div>`);
    for (const r of d.runs) {
      if (r.below_min_efficacy) {
        warn.push(`<div class="sc-warn bad" data-run="${esc(r.id)}"><b>${esc(r.label)}</b>: only ${r.manifested} of ${r.scenarios} scenarios' faults landed (efficacy ${frac(r.efficacy_rate)}, below ${frac(d.min_efficacy)}). This run measured the harness, not the subject: read its numbers as unmeasured, not as poor.</div>`);
      }
    }
    if (!floor) warn.push(`<div class="sc-warn info">No floor in this scorecard: no run is the deterministic detector (k8s-lookout), so nothing is flagged below it.</div>`);
    const rows = d.runs.map((r) => {
      const recall = r.means && typeof r.means[d.headline] === "number" ? frac(r.means[d.headline]) : "–";
      const role = r.floor ? '<span class="badge sc-floor-badge" title="The deterministic detector: the floor every subject is compared against">floor</span>'
        : r.null_subject ? '<span class="badge" title="The noop subject: answers &quot;nothing is wrong&quot; to everything; the zero baseline">null</span>' : "";
      const eff = `${r.manifested}/${r.scenarios} <span class="muted">(${frac(r.efficacy_rate)})</span>`;
      return `<tr class="${r.floor ? "sc-floor-row" : ""}">
        <td><b>${esc(r.subject)}</b> ${role}<div class="muted sc-run-label" title="${esc(r.source || "")}">${esc(r.label)}</div></td>
        <td>${esc(r.pack)}</td>
        <td>${esc(r.cluster || "–")}</td>
        <td class="num">${esc(when(r.started_at))}</td>
        <td class="num ${r.below_min_efficacy ? "sc-bad" : ""}">${eff}${r.below_min_efficacy ? ' <span class="badge bad">below min</span>' : ""}</td>
        <td class="num">${recall}</td>
      </tr>`;
    }).join("");
    // Open while it is short; with many runs the grid comes first and the
    // runs' headers are a click away (the grid's own column heads carry
    // each run's subject, name and efficacy).
    const open = d.runs.length <= 3 ? " open" : "";
    return head + warn.join("") + `<details class="sc-runs-box"${open}><summary>Runs <span class="muted">· subject, pack, cluster, when, how many faults landed</span></summary>
      <table class="sc-runs"><thead><tr><th>Subject · run</th><th>Pack</th><th>Cluster</th><th>Started</th><th>Faults landed</th><th>Mean ${esc(label(d.headline))}</th></tr></thead><tbody>${rows}</tbody></table></details>`;
  }

  // A row is a scenario of a pack: two packs may share a scenario ID.
  const keyOf = (s) => s.pack + "|" + s.id;

  function efficacyMark(d, s) {
    const cells = d.runs.map((r) => s.results[r.id]).filter(Boolean);
    const landed = cells.filter((c) => c.manifested).length;
    if (!cells.length) return '<span class="muted">–</span>';
    if (landed === cells.length) return `<span class="sc-ok" title="The fault landed in every run that included this scenario">✓</span>`;
    if (landed === 0) return `<span class="sc-bad" title="The fault did not land in any run: nothing was measured">✗</span>`;
    return `<span class="sc-warn-text" title="The fault landed in ${landed} of ${cells.length} runs">✓ ${landed}/${cells.length}</span>`;
  }

  function hoverText(d, s, cell) {
    const lines = (cell.scores || []).map((x) => `${label(x.name)}: ${x.skipped ? "–" : fmt(d, x.name, x.value)}${x.comment ? " — " + x.comment : ""}`);
    if (cell.subject_error) lines.push("subject error: " + cell.subject_error.slice(0, 200));
    return lines.join("\n");
  }

  function renderCell(d, s, r) {
    const cell = s.results[r.id];
    const cls = ["sc-cell"];
    if (r.floor) cls.push("sc-floor");
    if (!cell) {
      cls.push("sc-notrun");
      return `<td class="${cls.join(" ")}" title="${esc(r.label)} did not run this scenario"><span class="sc-dot">·</span></td>`;
    }
    if (!measured(cell)) {
      // Rule 1: never a number.
      cls.push("sc-nm");
      const why = cell.manifested ? "the measure did not apply" : "the fault did not land: " + (cell.inject_error || "no passing efficacy record");
      return `<td class="${cls.join(" ")}" title="Not measured — ${esc(why)}"><span class="sc-dash">—</span><span class="sc-nm-label">not measured</span></td>`;
    }
    const v = cell.value;
    if (cell.below_floor) cls.push("sc-below");
    if (v <= 0) cls.push("sc-zero");
    const ttd = scoreOf(cell, "time_to_detect");
    const ttr = scoreOf(cell, "time_to_remediate");
    const times = [ttd && !ttd.skipped ? "ttd " + secs(ttd.value) : "", ttr && !ttr.skipped ? "ttr " + secs(ttr.value) : ""].filter(Boolean).join(" · ");
    const flag = cell.below_floor
      ? `<span class="sc-below-mark" title="Below the floor: the deterministic detector scored ${frac(cell.floor_value)} on this scenario. The subject is not using its tools — a different fix from failing to diagnose.">▼ below floor ${frac(cell.floor_value)}</span>`
      : "";
    const err = cell.subject_error ? '<span class="badge bad" title="The subject failed on this scenario (scored, not skipped)">error</span>' : "";
    return `<td class="${cls.join(" ")}" title="${esc(hoverText(d, s, cell))}">
      <span class="sc-val">${frac(v)}</span>${err}
      <span class="sc-bar" aria-hidden="true"><i data-w="${Math.max(0, Math.min(1, v))}"></i></span>
      ${times ? `<span class="sc-times">${esc(times)}</span>` : ""}${flag}
    </td>`;
  }

  function rowNote(d, s) {
    const cells = d.runs.map((r) => [r, s.results[r.id]]).filter(([, c]) => c);
    const nm = cells.filter(([, c]) => !c.manifested).length;
    const below = cells.filter(([, c]) => c.below_floor).map(([r]) => r.subject);
    const notes = [];
    if (nm && nm === cells.length) notes.push('<span class="sc-note-nm">not measured</span>');
    else if (nm) notes.push(`<span class="sc-note-nm">${nm} not measured</span>`);
    if (below.length) notes.push(`<span class="sc-note-below">← ${esc([...new Set(below)].join(", "))} below ${below.length === 1 ? "its" : "their"} own tool</span>`);
    return notes.join(" ");
  }

  function renderGrid(d) {
    const style = d.runs.length > 5 ? " sc-dense" : "";
    const head = d.runs.map((r) => {
      const role = r.floor ? '<span class="badge sc-floor-badge">floor</span>' : r.null_subject ? '<span class="badge">null</span>' : "";
      const eff = `<span class="sc-col-eff${r.below_min_efficacy ? " sc-bad" : ""}" title="Faults that landed in this run (efficacy ${frac(r.efficacy_rate)})${r.below_min_efficacy ? ": below the minimum, so this run measured the harness, not the subject" : ""}">${r.below_min_efficacy ? "⚠ " : ""}${r.manifested}/${r.scenarios} landed</span>`;
      return `<th class="sc-col${r.floor ? " sc-floor" : ""}" title="${esc(r.subject)} — ${esc(r.label)}${r.cluster ? " · " + esc(r.cluster) : ""}"><span class="sc-col-subject">${esc(r.subject)}</span>${role}<span class="sc-col-label">${esc(r.label)}</span>${eff}</th>`;
    }).join("");
    const groups = [];
    for (const s of d.scenarios) {
      let g = groups.find((x) => x.pack === s.pack);
      if (!g) groups.push((g = { pack: s.pack, rows: [] }));
      g.rows.push(s);
    }
    const width = d.runs.length + 3;
    const bodyRows = groups.map((g) => {
      const header = d.mixed_packs ? `<tr class="sc-pack"><td colspan="${width}">pack <b>${esc(g.pack)}</b></td></tr>` : "";
      return header + g.rows.map((s) => {
        const key = keyOf(s);
        const nmAll = d.runs.every((r) => !s.results[r.id] || !s.results[r.id].manifested);
        return `<tr class="sc-row${sc.selected === key ? " on" : ""}${nmAll ? " sc-row-nm" : ""}" data-key="${esc(key)}">
          <th scope="row" class="sc-name"><button type="button" class="sc-pick" data-key="${esc(key)}" title="${esc(s.id)}">${esc(s.name)}</button>${s.control ? ' <span class="badge info" title="A healthy control: nothing is broken; the cell shows hallucinated fault">control</span>' : ""}</th>
          <td class="sc-eff">${efficacyMark(d, s)}</td>
          ${d.runs.map((r) => renderCell(d, s, r)).join("")}
          <td class="sc-note">${rowNote(d, s)}</td>
        </tr>`;
      }).join("");
    }).join("");
    return `<table class="sc-grid${style}"><thead><tr><th class="sc-name">Scenario</th><th class="sc-eff" title="Did the fault land (efficacy)?">Landed</th>${head}<th></th></tr></thead><tbody>${bodyRows}</tbody></table>`;
  }

  function renderLegend(d) {
    const floor = floorNames(d);
    return [
      `<span><span class="sc-chip sc-chip-nm">—</span> not measured: the fault did not land in that run, so nothing about the subject was measured (never a zero)</span>`,
      floor ? `<span><span class="sc-chip sc-chip-floor">floor</span> ${esc(floor)}: the deterministic detector${d.floor.rule === "flag" ? " (named with --floor)" : ""}</span>` : "",
      floor ? `<span><span class="sc-chip sc-chip-below">▼</span> below floor: scored under the detector on that scenario — not using its tools, a different fix from a wrong diagnosis</span>` : "",
      `<span><span class="sc-chip">·</span> not in that run</span>`,
      `<span>hover a cell for every measure; pick a scenario for its fault, ground truth and reports</span>`,
    ].filter(Boolean).join("");
  }

  // ─── Drill-down ───────────────────────────────────────────────────

  function select(key) {
    sc.selected = key;
    document.querySelectorAll("#sc-grid .sc-row").forEach((tr) => tr.classList.toggle("on", tr.dataset.key === key));
    const s = sc.doc && sc.doc.scenarios.find((x) => keyOf(x) === key);
    if (!s) return;
    $("sd-name").textContent = s.name;
    $("sc-detail").innerHTML = renderDetail(sc.doc, s);
    $("sc-detail").scrollTop = 0;
    const pb = $("sc-detail").closest(".panel-body");
    if (pb) pb.scrollTop = 0;
  }

  function targetText(t) {
    const sel = t.labels ? Object.entries(t.labels).map(([k, v]) => `${k}=${v}`).join(",") : "";
    return `${t.namespace ? t.namespace + "/" : ""}${t.kind ? t.kind + "/" : ""}${t.name || (sel ? "{" + sel + "}" : "?")}`;
  }

  function renderFaults(s) {
    if (!s.faults || !s.faults.length) return '<div class="empty">No fault: a healthy control.</div>';
    return s.faults.map((f) => `<div class="sd-fault">
      <div><span class="kind">${esc(f.kind || "?")}</span> <span class="muted">${esc(f.engine || "")}</span>${f.duration ? ` <span class="badge">${esc(f.duration)}</span>` : ""}</div>
      ${(f.targets || []).length ? `<div class="muted">on ${f.targets.map((t) => `<code>${esc(targetText(t))}</code>`).join(" ")}</div>` : ""}
      ${f.spec && Object.keys(f.spec).length ? `<details><summary class="muted">spec</summary><pre class="sd-pre">${esc(JSON.stringify(f.spec, null, 2))}</pre></details>` : ""}
      ${(f.probes || []).length ? `<details><summary class="muted">${f.probes.length} efficacy gate${f.probes.length === 1 ? "" : "s"}</summary>${f.probes.map((p) => `<code>${esc(p)}</code>`).join(" ")}</details>` : ""}
    </div>`).join("");
  }

  function renderTruth(s) {
    const gt = s.ground_truth || {};
    const expect = (gt.expect || []).map((e) => `<li><span class="kind">${esc(e.kind)}</span>/${esc(e.name)}${(e.reasons || []).length ? ` <span class="muted">${esc(e.reasons.join(" | "))}</span>` : ""}${e.root ? ' <span class="badge warn" title="The root cause; other expectations are its symptoms">root</span>' : ""}${e.min_severity ? ` <span class="muted">≥ ${esc(e.min_severity)}</span>` : ""}${(e.also_accept_kinds || []).length ? ` <span class="muted">(or ${esc(e.also_accept_kinds.join(", "))})</span>` : ""}</li>`).join("");
    return `${expect ? `<ul class="sd-list">${expect}</ul>` : '<div class="empty">Nothing to find: a correct report claims no failure.</div>'}
      ${gt.severity ? `<div class="muted">report severity: <b>${esc(gt.severity)}</b></div>` : ""}
      ${(gt.also_true || []).length ? `<div class="muted">also true, not required: ${gt.also_true.map((x) => `<code>${esc(x)}</code>`).join(" ")}</div>` : ""}`;
  }

  function gatesOf(c) {
    return (c.faults || []).flatMap((f) => (f.efficacy || []).map((e) =>
      `<li class="${e.passed ? "" : "sd-gate-bad"}"><span class="${e.passed ? "sc-ok" : "sc-bad"}">${e.passed ? "✓" : "✗"}</span> <code>${esc(e.probe)}</code> <span class="muted">${esc(clock(e.at))}</span>${e.passed ? "" : `<div class="muted">expected ${esc(e.expected)}; observed ${esc(e.observed)}</div>`}</li>`));
  }

  const trailOf = (c) => (c.faults || []).map((f) => `<span class="muted">${esc(f.kind || "fault")} …${esc(f.uid.slice(-6))}: applied ${esc(clock(f.applied_at))}, cleared ${esc(clock(f.cleared_at))}${f.problem ? ` — <span class="sc-bad">${esc(f.problem)}</span>` : ""}</span>`).join("<br>");

  // Where the fault did not land, each run says why, in full: that is the
  // row's whole story. Where it did, one line each, behind a summary.
  function renderLanded(d, s) {
    const runs = d.runs.filter((r) => s.results[r.id]);
    const failed = runs.filter((r) => !s.results[r.id].manifested);
    const landed = runs.filter((r) => s.results[r.id].manifested);
    const bad = failed.map((r) => {
      const c = s.results[r.id];
      const gates = gatesOf(c);
      return `<div class="sd-landed sd-landed-bad">
        <div><b>${esc(r.subject)}</b> <span class="muted">${esc(r.label)}</span> <span class="badge bad">did not land · not measured</span></div>
        ${c.inject_error ? `<div class="sc-bad sd-why">${esc(c.inject_error)}</div>` : ""}
        ${trailOf(c) ? `<div>${trailOf(c)}</div>` : ""}
        ${gates.length ? `<details open><summary class="muted">${gates.length} efficacy record${gates.length === 1 ? "" : "s"}</summary><ul class="sd-gates">${gates.join("")}</ul></details>` : ""}
      </div>`;
    }).join("");
    const good = landed.length ? `<details class="sd-landed sd-landed-ok"><summary><span class="sc-ok">✓</span> landed in ${landed.length === runs.length ? (runs.length === 1 ? "its run" : `all ${runs.length} runs`) : `${landed.length} of ${runs.length} runs`} <span class="muted">· every efficacy gate passed</span></summary>
      <ul class="sd-landed-runs">${landed.map((r) => {
        const c = s.results[r.id];
        const gates = gatesOf(c);
        return `<li><b>${esc(r.subject)}</b> <span class="muted">${esc(r.label)}</span><br>${trailOf(c)}${gates.length ? `<details><summary class="muted">${gates.length} efficacy record${gates.length === 1 ? "" : "s"}</summary><ul class="sd-gates">${gates.join("")}</ul></details>` : ""}</li>`;
      }).join("")}</ul></details>` : "";
    return bad + good;
  }

  function renderReport(d, s, r) {
    const c = s.results[r.id];
    if (!c) return "";
    const head = measured(c) ? `<span class="sd-head-val ${c.below_floor ? "sc-below-text" : ""}">${frac(c.value)}</span> <span class="muted">${esc(label(s.headline))}</span>`
      : '<span class="sd-head-val sc-nm-text">—</span> <span class="muted">not measured</span>';
    const ttd = scoreOf(c, "time_to_detect");
    const ttr = scoreOf(c, "time_to_remediate");
    const timing = `<div class="sd-times">
      <span>detect <b>${ttd && !ttd.skipped ? secs(ttd.value) : "–"}</b></span>
      <span>remediate <b>${ttr && !ttr.skipped ? secs(ttr.value) : "–"}</b></span>
      <span class="muted">injected ${esc(clock(c.injected_at))} · reported ${esc(clock(c.detected_at))}${isSet(c.cleared_at) ? " · cleared " + esc(clock(c.cleared_at)) : ""}</span>
    </div>`;
    const scores = (c.scores || []).map((x) => `<tr class="${x.skipped ? "sd-skip" : ""}"><td>${esc(label(x.name))}</td><td class="num">${x.skipped ? "–" : fmt(d, x.name, x.value)}</td><td class="muted">${esc(x.comment || "")}</td></tr>`).join("");
    const rep = c.report;
    let findings;
    if (!rep) findings = '<div class="empty">No structured report.</div>';
    else if (!rep.findings || !rep.findings.length) findings = '<div class="empty">Reported nothing wrong.</div>';
    else {
      findings = `<ul class="sd-findings">${rep.findings.map((f) => `<li><span class="sev sev-${esc(f.severity)}">${esc(f.severity || "?")}</span> <span class="kind">${esc(f.kind)}</span>/${esc(f.resource_name)} <span class="muted">${esc(f.reason)}</span>${f.namespace ? ` <span class="muted">· ${esc(f.namespace)}</span>` : ""}</li>`).join("")}</ul>`;
      if (c.findings_total > rep.findings.length) findings += `<div class="muted">… and ${c.findings_total - rep.findings.length} more (trimmed)</div>`;
    }
    return `<div class="sd-report${r.floor ? " sd-floor" : ""}${c.below_floor ? " sd-below" : ""}">
      <div class="sd-report-head"><b>${esc(r.subject)}</b>${r.floor ? ' <span class="badge sc-floor-badge">floor</span>' : ""} <span class="muted">${esc(r.label)}</span></div>
      <div>${head}${c.below_floor ? ` <span class="sc-below-mark">▼ below floor ${frac(c.floor_value)}</span>` : ""}</div>
      ${timing}
      <h4>Report${rep && rep.overall_severity ? ` <span class="muted">· overall ${esc(rep.overall_severity)}</span>` : ""}</h4>
      ${findings}
      ${c.subject_error ? `<details open><summary class="sc-bad">subject error</summary><pre class="sd-pre">${esc(c.subject_error)}</pre></details>` : ""}
      ${c.teardown_error ? `<div class="sc-warn-text">arena left behind: ${esc(c.teardown_error)}</div>` : ""}
      <details><summary class="muted">every measure</summary><table class="sd-scores">${scores}</table></details>
      ${(r.evidence || []).length ? `<div class="muted sd-evidence">in the run directory: ${r.evidence.map((e) => `<code>${esc(e)}</code>`).join(" ")}</div>` : ""}
    </div>`;
  }

  function renderDetail(d, s) {
    const runs = d.runs.filter((r) => s.results[r.id]);
    return `<div class="sd-title"><b>${esc(s.name)}</b> <code>${esc(s.id)}</code> <span class="muted">pack ${esc(s.pack)}</span>${s.control ? ' <span class="badge info">control</span>' : ""}</div>
      ${s.prompt ? `<div class="sd-prompt"><span class="muted">asked:</span> ${esc(s.prompt)}</div>` : ""}
      <div class="sd-two">
        <section><h3>Injected fault</h3>${renderFaults(s)}</section>
        <section><h3>Ground truth</h3>${renderTruth(s)}</section>
      </div>
      <h3>Did it land</h3>
      ${renderLanded(d, s)}
      <h3>Reports, side by side</h3>
      <div class="sd-reports">${runs.map((r) => renderReport(d, s, r)).join("")}</div>`;
  }

  // ─── Wiring ───────────────────────────────────────────────────────

  function bind() {
    $("btn-mode-live").addEventListener("click", leave);
    $("btn-mode-scorecard").addEventListener("click", enter);
    $("sc-file").addEventListener("change", (e) => {
      loadFile(e.target.files[0]);
      e.target.value = "";
    });
    $("btn-sc-open").addEventListener("click", () => {
      sc.doc = null;
      forgetURL();
      render();
    });
    $("sc-url-form").addEventListener("submit", (e) => {
      e.preventDefault();
      loadURL($("sc-url").value.trim());
    });
    $("sc-served-list").addEventListener("click", (e) => {
      const b = e.target.closest("[data-url]");
      if (b) loadURL(b.dataset.url);
    });
    $("sc-watch-btn").addEventListener("click", () => {
      leave();
      if (sc.openSims) sc.openSims();
    });
    $("sc-grid").addEventListener("click", (e) => {
      const row = e.target.closest("[data-key]");
      if (row) select(row.dataset.key);
    });

    // A scorecard dropped anywhere on the page opens it, in either mode;
    // anything else dropped is refused rather than navigated to.
    const hasFiles = (e) => e.dataTransfer && Array.from(e.dataTransfer.types || []).includes("Files");
    window.addEventListener("dragover", (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      body.classList.add("sc-dragging");
    });
    window.addEventListener("dragleave", (e) => {
      if (!e.relatedTarget) body.classList.remove("sc-dragging");
    });
    window.addEventListener("drop", (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      body.classList.remove("sc-dragging");
      loadFile(e.dataTransfer.files[0]);
    });
  }

  // boot is app.js's first call: a ?scorecard= link opens Scorecard mode at
  // once, before the page has looked for a controller. It answers whether
  // it did.
  function boot() {
    const url = new URLSearchParams(location.search).get("scorecard");
    if (!url) return false;
    loadURL(url, { remember: false });
    return true;
  }

  // landing is app.js's second, once it knows whether a controller serves
  // the page: with none (simian web, or the page opened from a file) and
  // nothing to watch, Scorecard mode is where the page lands, with a way
  // to the Simians dialog.
  function landing({ standalone = false, nothingToWatch = false, openSims = null } = {}) {
    sc.standalone = standalone;
    sc.openSims = openSims;
    if (standalone) loadServed();
    if (standalone && nothingToWatch) enter();
    renderChrome();
    return active();
  }

  bind();
  render();

  window.SimianScorecard = { boot, landing, enter, leave, active, loadURL, show };
})();
