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

// The room: Simian's panes floating in a 3D scene built from CSS
// perspective transforms, with a camera you can orbit, fly and reset, and
// panes you can drag by their title bars, bring to the front, and pull to
// the middle at reading size. In the flat layout this file only wires the
// HUD's theme and layout controls; the panes are an ordinary grid.
//
// Adapted from mast-web web/spatial.js at commit da7cf52
// (github.com/go-steer/mast-web, Apache-2.0): the camera and its limits,
// pointer parallax, facingFor(), aerial fog, floor casts, the radar, the
// settle and boot motion, panel drag, orbit, wheel dolly, WASD/QE fly,
// tile and reset view, and the saved workspace. Dropped: terminals and
// their sessions, the daemon sidebar, the status-bar module, room audio,
// the shell/slash-command machinery. Simian's panes are fixed (six, in
// the page), so where mast-web parks terminals in slots, this lays the
// six out to fit the window: each pane has a place on screen, and the
// world position, size and facing that put it there are solved for.
//
// app.js knows nothing about any of this. The panes are real DOM, so
// forms, buttons, tables, focus and selection work at any camera angle.
(function () {
  "use strict";

  const prefs = window.SimianPrefs;
  const body = document.body;
  const world = document.getElementById("scene-world");
  const viewport = document.getElementById("scene-viewport");
  const hudCamera = document.getElementById("hud-camera");
  const radar = document.getElementById("hud-radar");
  const radarBlips = radar.querySelector(".radar-blips");

  const isRoom = () => body.classList.contains("room");
  const clamp = (v, lo, hi) => (v < lo ? lo : v > hi ? hi : v);
  const rad = Math.PI / 180;

  // ─── Camera ───────────────────────────────────────────────────────
  // Orbit is applied to the world: rotateY the turntable, rotateX the
  // tilt, translateZ the dolly, scale the fit to the window.

  const HOME = { yaw: 0, pitch: 4, dolly: 0 };
  const cam = Object.assign({}, HOME);
  const YAW_LIMIT = 55;
  const PITCH_MIN = -18;
  const PITCH_MAX = 30;
  const DOLLY_MIN = -700;
  const DOLLY_MAX = 420;

  // The vertical half of #scene-viewport's perspective-origin (room.css).
  // The perspective itself grows with the window (written to the viewport
  // in frame()), so a wide screen gets the same gentle convergence as a
  // laptop rather than panes twisted hard at its edges.
  const PERSPECTIVE = 1250;
  const perspectiveFor = () => Math.max(PERSPECTIVE, window.innerWidth * 0.9);
  const ORIGIN_Y = 0.44;

  // The arrangement is designed against the window itself, so the world
  // only scales down (for small windows), never up: scaling up would
  // enlarge text the browser has already rasterised, which blurs it.
  function fitScale() {
    return clamp(Math.min(window.innerWidth / 1440, window.innerHeight / 860), 0.6, 1);
  }

  // ─── Parallax: a few degrees of lean toward the pointer ───────────

  const PARALLAX_YAW = 3.2;
  const PARALLAX_PITCH = 1.8;
  const parallax = { yaw: 0, pitch: 0 };
  const parallaxAim = { yaw: 0, pitch: 0 };
  const stillness = window.matchMedia("(prefers-reduced-motion: reduce)");
  let parallaxFrame = 0;

  function aimParallax(e) {
    if (!isRoom() || drag || orbit || resize || stillness.matches) return;
    const damp = centred ? 0.45 : 1;
    parallaxAim.yaw = ((e.clientX / window.innerWidth) * 2 - 1) * PARALLAX_YAW * damp;
    parallaxAim.pitch = -((e.clientY / window.innerHeight) * 2 - 1) * PARALLAX_PITCH * damp;
    if (!parallaxFrame) parallaxFrame = window.requestAnimationFrame(stepParallax);
  }

  // Exponential ease toward the aim, by elapsed time rather than per
  // frame: about mast-web's 0.075 a frame at 60fps, and the same
  // wall-clock settle on a machine that manages five. Low rate, so the
  // room has mass.
  let parallaxLast = 0;

  function stepParallax(now) {
    parallaxFrame = 0;
    const dt = parallaxLast ? Math.min((now - parallaxLast) / 1000, 0.5) : 1 / 60;
    parallaxLast = now;
    const dy = parallaxAim.yaw - parallax.yaw;
    const dp = parallaxAim.pitch - parallax.pitch;
    if (Math.abs(dy) < 0.02 && Math.abs(dp) < 0.02) {
      parallax.yaw = parallaxAim.yaw;
      parallax.pitch = parallaxAim.pitch;
      parallaxLast = 0;
      applyCamera();
      return;
    }
    const k = 1 - Math.exp(-dt / 0.22);
    parallax.yaw += dy * k;
    parallax.pitch += dp * k;
    applyCamera();
    parallaxFrame = window.requestAnimationFrame(stepParallax);
  }

  function applyCamera() {
    const pitch = clamp(cam.pitch + parallax.pitch, -22, 34);
    world.style.setProperty("--cam-yaw", (cam.yaw + parallax.yaw).toFixed(2) + "deg");
    world.style.setProperty("--cam-pitch", pitch.toFixed(2) + "deg");
    world.style.setProperty("--cam-dolly", cam.dolly.toFixed(0) + "px");
    world.style.setProperty("--cam-scale", fitScale().toFixed(3));
    hudCamera.textContent = "yaw " + Math.round(cam.yaw) + "° pitch " + Math.round(cam.pitch) + "°";
    radar.style.setProperty("--radar-yaw", cam.yaw.toFixed(2) + "deg");
  }

  function nudgeCamera(dYaw, dPitch, dDolly) {
    cam.yaw = clamp(cam.yaw + dYaw, -YAW_LIMIT, YAW_LIMIT);
    cam.pitch = clamp(cam.pitch + dPitch, PITCH_MIN, PITCH_MAX);
    cam.dolly = clamp(cam.dolly + dDolly, DOLLY_MIN, DOLLY_MAX);
    applyCamera();
    save();
  }

  // ─── Projection ───────────────────────────────────────────────────
  // Where a world point lands on screen, relative to the window centre,
  // with the camera at HOME. The same arithmetic the browser does with
  // #scene-world's transform and the viewport's perspective.

  function frame() {
    const s = fitScale();
    const o = (ORIGIN_Y - 0.5) * window.innerHeight; // the vanishing point, from centre
    const P = perspectiveFor();
    viewport.style.perspective = Math.round(P) + "px";
    return { s, o, P, sin: Math.sin(HOME.pitch * rad), cos: Math.cos(HOME.pitch * rad) };
  }

  function project(f, x, y, z) {
    const y1 = y * f.cos - z * f.sin;
    const z1 = y * f.sin + z * f.cos; // scale() is 2D: depth is not scaled
    const m = f.P / (f.P - z1);
    return { x: x * f.s * m, y: f.o + (y1 * f.s - f.o) * m, m: m * f.s };
  }

  // The angle a pane at pos has to hold to look at the eye, so every pane
  // sits on a cylinder around the viewer and none is ever edge-on. Both
  // are damped: mast-web damps the pitch; Simian's outer panes are wider
  // than a terminal, and turning them fully swings their near edge far
  // enough forward to loom over the HUD.
  function facingFor(f, pos) {
    const depth = Math.max(f.P / f.s - pos.z, 240);
    return {
      ry: (-Math.atan2(pos.x, depth) / rad) * 0.6,
      rx: (Math.atan2(pos.y - f.o / f.s, depth) / rad) * 0.5,
    };
  }

  // The screen box of a placed pane: its four corners, turned by its
  // facing (rotateY(ry) rotateX(rx), as .panel-anchor applies them) and
  // projected.
  function box(f, p) {
    const cy = Math.cos(p.ry * rad), sy = Math.sin(p.ry * rad);
    const cx = Math.cos(p.rx * rad), sx = Math.sin(p.rx * rad);
    const b = { l: Infinity, r: -Infinity, t: Infinity, btm: -Infinity };
    for (const lx of [-p.w / 2, p.w / 2]) {
      for (const ly of [-p.h / 2, p.h / 2]) {
        const y1 = ly * cx;
        const z1 = ly * sx;
        const at = project(f, p.x + lx * cy + z1 * sy, p.y + y1, p.z - lx * sy + z1 * cy);
        b.l = Math.min(b.l, at.x); b.r = Math.max(b.r, at.x);
        b.t = Math.min(b.t, at.y); b.btm = Math.max(b.btm, at.y);
      }
    }
    return b;
  }

  // Solves for the world placement that puts a pane on a given screen
  // rectangle (window-centre coordinates) at a given depth: position,
  // size, facing, and the text zoom that keeps it legible further back.
  function solve(f, rect, z, face) {
    const cx = (rect.x0 + rect.x1) / 2;
    const cy = (rect.y0 + rect.y1) / 2;
    const m0 = project(f, 0, cy / f.s, z).m;
    const p = { x: cx / m0, y: cy / f.s, z, w: (rect.x1 - rect.x0) / m0, h: (rect.y1 - rect.y0) / m0, ry: 0, rx: 0 };
    for (let i = 0; i < 8; i++) {
      if (face !== false) Object.assign(p, facingFor(f, p));
      const b = box(f, p);
      const m = project(f, p.x, p.y, z).m;
      p.w *= (rect.x1 - rect.x0) / (b.r - b.l);
      p.h *= (rect.y1 - rect.y0) / (b.btm - b.t);
      p.x += (cx - (b.l + b.r) / 2) / m;
      p.y += (cy - (b.t + b.btm) / 2) / m;
    }
    const m = project(f, p.x, p.y, z).m;
    p.zoom = clamp(Math.pow(1 / m, 0.85), 1, 1.4);
    return p;
  }

  // ─── The arrangement ──────────────────────────────────────────────
  // The inject pane stands tall down the left, from the HUD to the
  // floor, so its conversation has room for a proposal card between the
  // controls and the input. To its right, two tiers in a gentle arc:
  // the three most watched panes (live events, active faults in the
  // middle of them, autonomous mode) in front, nearer, the outer one
  // angled in toward you; behind and above them recent faults (the
  // widest, for its table) and workloads. u is across the window
  // (-1…1), v down the usable height between HUD and status bar (0…1);
  // the strip below the front tier is floor, with the radar in its
  // corner. Changing it means bumping the saved room's version (save()).
  const PLACES = {
    inject: { u: [-1, -0.45], v: [0, 0.86], z: -80 },
    faults: { u: [-0.415, 0.6], v: [0, 0.4], z: -240 },
    workloads: { u: [0.63, 1], v: [0, 0.4], z: -200 },
    events: { u: [-0.415, 0.035], v: [0.44, 0.86], z: 10 },
    active: { u: [0.065, 0.52], v: [0.44, 0.86], z: 20 },
    auto: { u: [0.55, 1], v: [0.44, 0.86], z: 40 },
  };

  // The HUD's height, and the alert bar's under it while it shows.
  function chromeTop(cs) {
    return (parseFloat(cs.getPropertyValue("--hud-h")) || 36) + (parseFloat(cs.getPropertyValue("--alert-h")) || 0);
  }

  function usable() {
    const cs = getComputedStyle(body);
    const hud = chromeTop(cs);
    const status = parseFloat(cs.getPropertyValue("--status-h")) || 26;
    const W = window.innerWidth;
    const H = window.innerHeight;
    // On a big screen the arrangement stops growing: a room with space
    // around its panes reads as a room, and 12px text in a pane the size
    // of a laptop screen reads as nothing.
    const spare = Math.max(0, H - hud - status - 28 - 1060);
    const top = -H / 2 + hud + 14 + spare * 0.3;
    const bottom = Math.min(H / 2 - status - 14, top + 1060);
    return { hw: Math.min(W / 2 - 18, 1000), top, bottom };
  }

  function homeFor(id, f, area) {
    const pl = PLACES[id];
    const vh = area.bottom - area.top;
    return solve(f, {
      x0: pl.u[0] * area.hw,
      x1: pl.u[1] * area.hw,
      y0: area.top + pl.v[0] * vh,
      y1: area.top + pl.v[1] * vh,
    }, pl.z);
  }

  function centredFor(f, area) {
    const W = window.innerWidth;
    const hw = Math.min(W * 0.4, 820);
    const cy = (area.top + area.bottom) / 2;
    const hh = Math.min((area.bottom - area.top) * 0.47, 560);
    return solve(f, { x0: -hw, x1: hw, y0: cy - hh, y1: cy + hh }, 0, false);
  }

  // ─── Panes ────────────────────────────────────────────────────────

  const panes = [];
  let front = null;
  let centred = null;
  let viewBeforeCentre = null;

  const castLayer = document.createElement("div");
  castLayer.id = "panel-casts";
  castLayer.setAttribute("aria-hidden", "true");
  world.appendChild(castLayer);

  document.querySelectorAll(".panel-anchor[data-pane]").forEach((el, index) => {
    const id = el.dataset.pane;
    const panel = el.querySelector(".panel");
    const bar = el.querySelector(".panel-bar");
    const p = { id, index, el, panel, bar, pos: null, home: null, moved: false, size: null };

    const controls = document.createElement("span");
    controls.className = "panel-controls";
    const centreBtn = document.createElement("button");
    centreBtn.type = "button";
    centreBtn.className = "panel-ctl";
    centreBtn.textContent = "□";
    const title = bar.querySelector(".panel-title").childNodes[0].textContent.trim();
    centreBtn.title = "Bring " + title + " to the middle, or put it back (or double-click the title bar)";
    centreBtn.setAttribute("aria-label", "Centre " + title);
    centreBtn.addEventListener("click", (e) => { e.stopPropagation(); toggleCentre(p); });
    controls.appendChild(centreBtn);
    bar.appendChild(controls);

    panel.style.setProperty("--drift-delay", (index * -3.7).toFixed(1) + "s");

    // The resize grip: the bottom-right corner in the room, the bottom
    // edge in the flat grid. Out of the tab order (the panes' contents
    // own the keyboard); named for screen readers and the pointer.
    p.title = title;
    p.grip = document.createElement("button");
    p.grip.type = "button";
    p.grip.tabIndex = -1;
    p.grip.className = "panel-grip";
    p.grip.setAttribute("aria-label", "Resize " + title);
    p.grip.title = "Drag to resize " + title + "; double-click for its usual size";
    panel.appendChild(p.grip);

    p.cast = document.createElement("div");
    p.cast.className = "panel-cast";
    p.cast.style.setProperty("--hue", "var(--hue-" + id + ")");
    castLayer.appendChild(p.cast);
    p.blip = document.createElement("div");
    p.blip.className = "radar-blip";
    p.blip.style.setProperty("--hue", "var(--hue-" + id + ")");
    radarBlips.appendChild(p.blip);

    // A pane app.js has not shown yet (the inject form, until it knows
    // who you are) throws no light and has no blip.
    const showHidden = () => {
      p.cast.hidden = p.blip.hidden = panel.hidden;
    };
    new MutationObserver(showHidden).observe(panel, { attributes: true, attributeFilter: ["hidden"] });
    showHidden();

    p.grip.addEventListener("pointerdown", (e) => startResize(p, e));
    p.grip.addEventListener("pointermove", moveResize);
    p.grip.addEventListener("pointerup", endResize);
    p.grip.addEventListener("pointercancel", endResize);
    p.grip.addEventListener("lostpointercapture", endResize);
    p.grip.addEventListener("dblclick", (e) => { e.stopPropagation(); restoreSize(p); });
    p.grip.addEventListener("click", (e) => e.stopPropagation());

    bar.addEventListener("dblclick", (e) => {
      if (!isRoom() || e.target.closest("button, select, input")) return;
      toggleCentre(p);
    });
    panes.push(p);
  });

  const pane = (id) => panes.find((p) => p.id === id);

  // Running faults make the Active pane's pool breathe and its blip ping.
  const activeList = document.getElementById("active");
  new MutationObserver(() => {
    const busy = !!activeList.querySelector(".card");
    const p = pane("active");
    p.cast.classList.toggle("cast-busy", busy);
    p.blip.classList.toggle("blip-busy", busy);
  }).observe(activeList, { childList: true });

  // Aerial perspective: a pane further back is hazed toward the room.
  const fogFor = (z) => clamp((40 - z) / 1240, 0, 1);

  // Keep in step with the plotted box: the authored places span about
  // x ±1000 and z -240…+40, and dragging has no bounds.
  const RADAR_Z_NEAR = 320;
  const RADAR_Z_FAR = -1100;

  function place(p) {
    const at = p.pos;
    const lift = p === front && p !== centred ? 14 : 0;
    p.el.style.setProperty("--tx", Math.round(at.x) + "px");
    p.el.style.setProperty("--ty", Math.round(at.y) + "px");
    p.el.style.setProperty("--tz", Math.round(at.z + lift) + "px");
    p.el.style.setProperty("--ry", at.ry.toFixed(1) + "deg");
    p.el.style.setProperty("--rx", at.rx.toFixed(1) + "deg");
    p.el.style.setProperty("--fog", fogFor(at.z).toFixed(3));
    p.panel.style.setProperty("--pw", Math.round(at.w) + "px");
    p.panel.style.setProperty("--ph", Math.round(at.h) + "px");
    p.panel.style.setProperty("--body-zoom", at.zoom.toFixed(3));

    p.cast.style.setProperty("--cx", Math.round(at.x) + "px");
    p.cast.style.setProperty("--cz", Math.round(at.z) + "px");
    p.cast.style.setProperty("--cspread", clamp((floorY - at.y) / 520, 0.6, 1.6).toFixed(2));
    p.cast.classList.toggle("cast-active", p === front || p === centred);

    const radarX = Math.max(window.innerWidth / fitScale() / 2, 700) * 1.25;
    const bx = clamp((at.x / radarX + 1) / 2, 0.05, 0.95);
    const by = clamp((at.z - RADAR_Z_FAR) / (RADAR_Z_NEAR - RADAR_Z_FAR), 0.05, 0.95);
    p.blip.style.setProperty("--bx", (bx * 100).toFixed(1) + "%");
    p.blip.style.setProperty("--by", (by * 100).toFixed(1) + "%");
    p.blip.classList.toggle("blip-active", p === front || p === centred);
  }

  // The floor sits a little below the lowest pane and the ceiling a
  // little above the highest, whatever the window's shape.
  let floorY = 420;

  function hangPlanes() {
    let lo = 300;
    let hi = -420;
    for (const p of panes) {
      if (!p.home) continue;
      lo = Math.max(lo, p.home.y + p.home.h / 2 + 70);
      hi = Math.min(hi, p.home.y - p.home.h / 2 - 160);
    }
    floorY = lo;
    world.style.setProperty("--floor-y", Math.round(lo) + "px");
    world.style.setProperty("--ceil-y", Math.round(hi) + "px");
  }

  // Lays every pane out for the current window. A pane the user dragged
  // keeps its position (and takes its new home size); the rest go home.
  function layout() {
    const f = frame();
    const area = usable();
    for (const p of panes) {
      p.home = homeFor(p.id, f, area);
      if (p === centred) {
        p.pos = centredFor(f, area);
      } else if (p.moved && p.pos) {
        Object.assign(p.pos, sizeFor(p), { zoom: p.home.zoom }, facingFor(f, p.pos));
      } else {
        p.pos = Object.assign({}, p.home, sizeFor(p));
      }
    }
    hangPlanes();
    panes.forEach(place);
  }

  // ─── Front and centre ─────────────────────────────────────────────
  // Touching a pane brings it to the front: it lights its title bar,
  // holds still, and steps a few pixels toward you so it wins any
  // overlap. Centring pulls it to the middle at reading size while the
  // rest fall back; Escape, □ or a double-click puts it back.

  function setFront(p) {
    if (front === p) return;
    const was = front;
    front = p;
    if (was) { was.el.classList.remove("front"); place(was); }
    if (p) { p.el.classList.add("front"); place(p); }
  }

  const SETTLE_MS = 460;

  function settle(p, fromX) {
    p.el.style.setProperty("--settle-dir", p.pos.x < fromX ? "-1" : "1");
    p.el.classList.remove("settling");
    void p.el.offsetWidth;
    p.el.classList.add("settling");
    window.clearTimeout(p.settleTimer);
    p.settleTimer = window.setTimeout(() => p.el.classList.remove("settling"), SETTLE_MS);
  }

  function centre(p) {
    if (centred === p) return;
    if (centred) uncentre();
    centred = p;
    p.parked = p.pos;
    // A pane brought to the middle for reading faces you: square the
    // camera up, and give the view back when the pane goes home.
    viewBeforeCentre = Object.assign({}, cam);
    Object.assign(cam, HOME);
    applyCamera();
    const fromX = p.pos.x;
    p.pos = centredFor(frame(), usable());
    p.el.classList.add("centred");
    body.classList.add("has-centred");
    setFront(p);
    place(p);
    settle(p, fromX);
  }

  function uncentre() {
    const p = centred;
    if (!p) return;
    centred = null;
    const fromX = p.pos.x;
    p.pos = p.moved && p.parked ? p.parked : Object.assign({}, p.home, sizeFor(p));
    p.parked = null;
    p.el.classList.remove("centred");
    body.classList.remove("has-centred");
    place(p);
    settle(p, fromX);
    if (viewBeforeCentre) {
      Object.assign(cam, viewBeforeCentre);
      viewBeforeCentre = null;
      applyCamera();
    }
  }

  function toggleCentre(p) {
    if (centred === p) uncentre();
    else centre(p);
  }

  function tile() {
    uncentre();
    for (const p of panes) {
      p.moved = false;
      p.size = null;
      p.pos = Object.assign({}, p.home);
      place(p);
    }
    save();
  }

  // ─── Resize: a pane's real size, not a scale ──────────────────────
  // The grip changes the pane's CSS width and height (--pw/--ph), so its
  // text stays the size it was and more of its rows fit. In the room the
  // top-left corner stays put: the pane is centred on its position, so
  // growing it by dw moves the position dw/2 along the pane's own x axis
  // (turned by its facing) and dh/2 down. Pointer movement is turned into
  // pane pixels by the ratio of the pane's on-screen box to its CSS size,
  // taken when the drag starts — exact face-on, close enough at the
  // angles the room allows. In the flat grid only the height changes.

  const MIN_W = 260;
  const MIN_H = 160;
  const MAX_GROW = 1.6;

  function sizeBounds(p) {
    const base = p.home || { w: MIN_W, h: MIN_H };
    return {
      w0: MIN_W, w1: Math.max(MIN_W, base.w * MAX_GROW),
      h0: MIN_H, h1: Math.max(MIN_H, base.h * MAX_GROW),
    };
  }

  // The size a pane takes in the room: the one you gave it, within its
  // bounds for this window, or its home size.
  function sizeFor(p) {
    if (!p.size) return { w: p.home.w, h: p.home.h };
    const b = sizeBounds(p);
    return { w: clamp(p.size.w, b.w0, b.w1), h: clamp(p.size.h, b.h0, b.h1) };
  }

  let resize = null;

  function startResize(p, e) {
    if (e.button !== 0) return;
    e.stopPropagation();
    e.preventDefault();
    if (isRoom()) {
      if (p === centred) return;
      setFront(p);
      const r = p.panel.getBoundingClientRect();
      resize = {
        p, x: e.clientX, y: e.clientY, room: true,
        w: p.pos.w, h: p.pos.h, origin: Object.assign({}, p.pos),
        kx: r.width / p.pos.w || 1, ky: r.height / p.pos.h || 1,
      };
    } else {
      resize = { p, y: e.clientY, room: false, h: p.panel.getBoundingClientRect().height };
    }
    p.el.classList.add("resizing");
    body.classList.add(resize.room ? "resizing-pane" : "resizing-flat");
    p.grip.setPointerCapture(e.pointerId);
  }

  function moveResize(e) {
    if (!resize) return;
    const p = resize.p;
    if (!resize.room) {
      setFlatHeight(p, resize.h + (e.clientY - resize.y));
      return;
    }
    // The corner follows the pointer, and the pointer is held inside
    // the room's window, so the grip never ends up off screen.
    const cs = getComputedStyle(body);
    const top = chromeTop(cs) + 6;
    const bottom = window.innerHeight - (parseFloat(cs.getPropertyValue("--status-h")) || 26) - 6;
    const px = clamp(e.clientX, 8, window.innerWidth - 18);
    const py = clamp(e.clientY, top, bottom);
    const b = sizeBounds(p);
    const w = clamp(resize.w + (px - resize.x) / resize.kx, b.w0, b.w1);
    const h = clamp(resize.h + (py - resize.y) / resize.ky, b.h0, b.h1);
    applySize(p, resize.origin, resize.w, resize.h, w, h);
    if (Math.abs(w - resize.w) > 1 || Math.abs(h - resize.h) > 1) p.moved = true;
    place(p);
  }

  // Sets a room pane's size, from a placement of size (w0, h0), keeping
  // its top-left corner where it was.
  function applySize(p, from, w0, h0, w, h) {
    const ry = from.ry * rad;
    const dw = (w - w0) / 2;
    const dh = (h - h0) / 2;
    p.pos.w = w;
    p.pos.h = h;
    p.pos.x = from.x + dw * Math.cos(ry);
    p.pos.z = from.z - dw * Math.sin(ry);
    p.pos.y = from.y + dh * Math.cos(from.rx * rad);
    p.size = { w, h };
  }

  function endResize() {
    if (!resize) return;
    const r = resize;
    resize = null;
    r.p.el.classList.remove("resizing");
    body.classList.remove("resizing-pane", "resizing-flat");
    if (r.room) save();
    else saveFlat();
  }

  // Double-click the grip: the pane's usual size, top-left kept; back
  // home if that is where it came from.
  function restoreSize(p) {
    if (!isRoom()) {
      setFlatHeight(p, null);
      saveFlat();
      return;
    }
    if (p === centred || !p.size) return;
    const from = Object.assign({}, p.pos);
    applySize(p, from, from.w, from.h, p.home.w, p.home.h);
    p.size = null;
    if (Math.hypot(p.pos.x - p.home.x, p.pos.y - p.home.y, p.pos.z - p.home.z) < 3) {
      p.moved = false;
      p.pos = Object.assign({}, p.home);
    }
    place(p);
    save();
  }

  // ─── Flat heights ─────────────────────────────────────────────────
  // The grid sets the widths; a pane's height is yours to change, and is
  // kept under its own key.

  const FLAT_KEY = "simian:flat";

  function flatBounds() {
    return { h0: MIN_H, h1: Math.max(600, window.innerHeight * 1.5) };
  }

  function setFlatHeight(p, h) {
    if (h === null) {
      p.flatH = null;
      p.panel.classList.remove("flat-sized");
      p.panel.style.removeProperty("--flat-h");
      return;
    }
    const b = flatBounds();
    p.flatH = Math.round(clamp(h, b.h0, b.h1));
    p.panel.classList.add("flat-sized");
    p.panel.style.setProperty("--flat-h", p.flatH + "px");
  }

  function saveFlat() {
    const heights = {};
    for (const p of panes) if (p.flatH) heights[p.id] = p.flatH;
    try { localStorage.setItem(FLAT_KEY, JSON.stringify({ v: 1, heights })); } catch (e) { /* this visit only */ }
  }

  function restoreFlat() {
    let saved = null;
    try { saved = JSON.parse(localStorage.getItem(FLAT_KEY) || "null"); } catch (e) { saved = null; }
    if (!saved || saved.v !== 1) return;
    for (const [id, h] of Object.entries(saved.heights || {})) {
      const p = pane(id);
      if (p && Number.isFinite(h)) setFlatHeight(p, h);
    }
  }

  function resetView() {
    viewBeforeCentre = null;
    Object.assign(cam, HOME);
    applyCamera();
    tile();
  }

  // ─── Pointer: drag a pane by its title bar, orbit from anywhere else

  let drag = null;
  let orbit = null;

  viewport.addEventListener("pointerdown", (e) => {
    if (!isRoom() || e.button !== 0) return;
    const anchor = e.target.closest(".panel-anchor");
    if (anchor) {
      const p = panes.find((x) => x.el === anchor);
      setFront(p);
      const onBar = e.target.closest(".panel-bar") && !e.target.closest("button, select, input, a");
      if (onBar && p !== centred) {
        drag = { p, x: e.clientX, y: e.clientY, origin: Object.assign({}, p.pos) };
        p.el.classList.add("dragging");
        viewport.setPointerCapture(e.pointerId);
        e.preventDefault();
      }
      return;
    }
    orbit = { x: e.clientX, y: e.clientY, yaw: cam.yaw, pitch: cam.pitch };
    body.classList.add("dragging-camera");
    viewport.setPointerCapture(e.pointerId);
  });

  viewport.addEventListener("pointermove", (e) => {
    if (drag) {
      const s = fitScale();
      const dx = (e.clientX - drag.x) / s;
      const dy = (e.clientY - drag.y) / s;
      const yaw = cam.yaw * rad;
      const p = drag.p;
      p.pos.x = drag.origin.x + dx * Math.cos(yaw);
      p.pos.z = drag.origin.z + dx * Math.sin(yaw);
      p.pos.y = drag.origin.y + dy;
      Object.assign(p.pos, facingFor(frame(), p.pos));
      if (Math.hypot(e.clientX - drag.x, e.clientY - drag.y) > 3) p.moved = true;
      place(p);
      return;
    }
    if (orbit) {
      cam.yaw = clamp(orbit.yaw + (e.clientX - orbit.x) * 0.13, -YAW_LIMIT, YAW_LIMIT);
      cam.pitch = clamp(orbit.pitch - (e.clientY - orbit.y) * 0.08, PITCH_MIN, PITCH_MAX);
      applyCamera();
    }
  });

  function endPointer() {
    if (drag) {
      drag.p.el.classList.remove("dragging");
      drag = null;
      save();
    }
    if (orbit) {
      orbit = null;
      body.classList.remove("dragging-camera");
      save();
    }
  }

  viewport.addEventListener("pointerup", endPointer);
  viewport.addEventListener("pointercancel", endPointer);
  window.addEventListener("pointermove", aimParallax);

  // The wheel dollies the camera, except over a pane's contents, which
  // scroll as they would anywhere else.
  viewport.addEventListener("wheel", (e) => {
    if (!isRoom() || e.target.closest(".panel-body")) return;
    e.preventDefault();
    nudgeCamera(0, 0, -e.deltaY * 0.6);
  }, { passive: false });

  // Belt and braces for overflow: clip — nothing may scroll the room.
  viewport.addEventListener("scroll", () => {
    if (isRoom() && (viewport.scrollTop || viewport.scrollLeft)) viewport.scrollTo(0, 0);
  });

  // ─── Keyboard: hold WASD to fly, Q/E to tilt (mast-web) ───────────

  const LOOK_KEYS = { w: [0, 0, 1], s: [0, 0, -1], a: [-1, 0, 0], d: [1, 0, 0], q: [0, 1, 0], e: [0, -1, 0] };
  const LOOK_YAW_DPS = 33;
  const LOOK_PITCH_DPS = 21;
  const LOOK_DOLLY_PPS = 540;
  const LOOK_RAMP_S = 0.5;
  const held = new Set();
  let lookFrame = 0;
  let lookSpeed = 0;
  let lookLast = 0;

  function stepLook(now) {
    if (!held.size) {
      lookFrame = 0;
      lookSpeed = 0;
      lookLast = 0;
      return;
    }
    const dt = Math.min(lookLast ? (now - lookLast) / 1000 : 1 / 60, 0.05);
    lookLast = now;
    lookSpeed = Math.min(1, lookSpeed + dt / LOOK_RAMP_S);
    let dy = 0;
    let dp = 0;
    let dd = 0;
    held.forEach((k) => { dy += LOOK_KEYS[k][0]; dp += LOOK_KEYS[k][1]; dd += LOOK_KEYS[k][2]; });
    const step = lookSpeed * dt;
    cam.yaw = clamp(cam.yaw + dy * LOOK_YAW_DPS * step, -YAW_LIMIT, YAW_LIMIT);
    cam.pitch = clamp(cam.pitch + dp * LOOK_PITCH_DPS * step, PITCH_MIN, PITCH_MAX);
    cam.dolly = clamp(cam.dolly + dd * LOOK_DOLLY_PPS * step, DOLLY_MIN, DOLLY_MAX);
    applyCamera();
    lookFrame = window.requestAnimationFrame(stepLook);
  }

  function stopLook() {
    if (!held.size) return;
    held.clear();
    save();
  }

  document.addEventListener("keyup", (e) => {
    if (held.delete(e.key.toLowerCase()) && !held.size) save();
  });
  window.addEventListener("blur", stopLook);

  // Keys belong to whatever has focus first: a form control, an open
  // dialog, or anything typed with a modifier is never the camera's.
  function typing(e) {
    return e.metaKey || e.ctrlKey || e.altKey ||
      !!document.querySelector("dialog[open]") ||
      !!e.target.closest("input, textarea, select, [contenteditable], dialog");
  }

  document.addEventListener("keydown", (e) => {
    if (!isRoom()) return;
    if (e.key === "Escape" && centred && !document.querySelector("dialog[open]")) {
      uncentre();
      e.preventDefault();
      return;
    }
    if (typing(e)) return;
    const k = e.key.toLowerCase();
    if (LOOK_KEYS[k]) {
      if (!held.has(k)) {
        held.add(k);
        if (!lookFrame) lookFrame = window.requestAnimationFrame(stepLook);
      }
      e.preventDefault();
      return;
    }
    const keys = {
      ArrowLeft: () => nudgeCamera(-4, 0, 0),
      ArrowRight: () => nudgeCamera(4, 0, 0),
      ArrowUp: () => nudgeCamera(0, 2, 0),
      ArrowDown: () => nudgeCamera(0, -2, 0),
      "+": () => nudgeCamera(0, 0, 60),
      "=": () => nudgeCamera(0, 0, 60),
      "-": () => nudgeCamera(0, 0, -60),
      r: resetView,
      R: resetView,
      0: resetView,
    };
    if (!keys[e.key]) return;
    keys[e.key]();
    e.preventDefault();
  });

  // ─── The saved room: camera and the panes you moved ───────────────

  const ROOM_KEY = "simian:room";
  // 3: the arrangement with the tall inject pane. Positions and sizes
  // saved against an earlier arrangement would land among the new one's
  // panes, so those are dropped; the camera is kept.
  const ROOM_V = 3;
  let saveTimer = 0;

  function save() {
    if (saveTimer) return;
    saveTimer = window.setTimeout(() => {
      saveTimer = 0;
      const moved = {};
      const sizes = {};
      for (const p of panes) {
        const at = p === centred ? p.parked : p.pos;
        if (p.moved && at) moved[p.id] = { x: Math.round(at.x), y: Math.round(at.y), z: Math.round(at.z) };
        if (p.size) sizes[p.id] = { w: Math.round(p.size.w), h: Math.round(p.size.h) };
      }
      try {
        localStorage.setItem(ROOM_KEY, JSON.stringify({ v: ROOM_V, cam: { yaw: cam.yaw, pitch: cam.pitch, dolly: cam.dolly }, moved, sizes }));
      } catch (e) { /* blocked storage: the room still holds for this visit */ }
    }, 250);
  }

  function restore() {
    let saved = null;
    try { saved = JSON.parse(localStorage.getItem(ROOM_KEY) || "null"); } catch (e) { saved = null; }
    // v1 and v2 were the earlier arrangement (v1 without sizes): keep
    // their camera, not their panes.
    if (!saved || ![1, 2, ROOM_V].includes(saved.v)) return;
    if (saved.cam) {
      cam.yaw = clamp(Number(saved.cam.yaw) || 0, -YAW_LIMIT, YAW_LIMIT);
      cam.pitch = clamp(Number(saved.cam.pitch) || 0, PITCH_MIN, PITCH_MAX);
      cam.dolly = clamp(Number(saved.cam.dolly) || 0, DOLLY_MIN, DOLLY_MAX);
    }
    if (saved.v !== ROOM_V) return;
    for (const [id, at] of Object.entries(saved.moved || {})) {
      const p = pane(id);
      if (!p || ![at.x, at.y, at.z].every(Number.isFinite)) continue;
      p.moved = true;
      p.pos = { x: at.x, y: at.y, z: at.z, w: 0, h: 0, zoom: 1, ry: 0, rx: 0 };
    }
    for (const [id, sz] of Object.entries(saved.sizes || {})) {
      const p = pane(id);
      if (p && [sz.w, sz.h].every(Number.isFinite)) p.size = { w: sz.w, h: sz.h };
    }
  }

  // ─── Boot: the panes come on one after another ────────────────────

  const BOOT_MS = 700;
  const BOOT_STAGGER_MS = 90;

  function boot() {
    const order = ["active", "auto", "events", "faults", "inject", "workloads"];
    order.forEach((id, i) => {
      const p = pane(id);
      p.el.style.setProperty("--boot-delay", i * BOOT_STAGGER_MS + "ms");
      p.el.classList.add("booting");
      window.setTimeout(() => {
        p.el.classList.remove("booting");
        p.el.style.removeProperty("--boot-delay");
      }, i * BOOT_STAGGER_MS + BOOT_MS);
    });
  }

  // ─── Layout switch and theme ──────────────────────────────────────

  const layoutBtn = document.getElementById("btn-layout");

  function enter(mode) {
    prefs.applyLayout(mode);
    layoutBtn.textContent = mode === "room" ? "flat ▦" : "room ◈";
    layoutBtn.title = mode === "room" ? "Lay the panes out flat, in a grid" : "Stand the panes up in the room";
    if (mode === "room") {
      applyCamera();
      layout();
    } else {
      uncentre();
      setFront(null);
      stopLook();
      viewport.scrollTo(0, 0);
    }
  }

  layoutBtn.addEventListener("click", () => {
    const next = isRoom() ? "flat" : "room";
    prefs.setLayout(next);
    enter(next);
    if (next === "room") boot();
  });
  prefs.onDefaultsChange(enter);
  prefs.mountTheme(document.getElementById("hud-theme"));
  prefs.mountMotion(document.getElementById("btn-motion"));

  document.getElementById("btn-tile").addEventListener("click", tile);
  document.getElementById("btn-reset").addEventListener("click", resetView);

  // The alert bar under the HUD (app.js shows it when the selected
  // Simian does not answer): its height is --alert-h, which the flat grid
  // starts below (room.css) and the room's usable area leaves out, so no
  // pane stands behind it. A pane you moved stays where you put it.
  const alertBar = document.getElementById("sim-alert");
  let alertH = -1;
  new ResizeObserver(() => {
    const h = alertBar.hidden ? 0 : Math.ceil(alertBar.getBoundingClientRect().height);
    if (h === alertH) return;
    alertH = h;
    body.style.setProperty("--alert-h", h + "px");
    if (isRoom()) layout();
  }).observe(alertBar);

  window.addEventListener("resize", () => {
    if (!isRoom()) return;
    applyCamera();
    layout();
  });

  if ("scrollRestoration" in history) history.scrollRestoration = "manual";

  restore();
  restoreFlat();
  enter(prefs.layout());
  if (isRoom()) boot();

  window.SimianRoom = { panes, camera: cam, centre, uncentre, tile, resetView, layout, restoreSize };
})();
