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

// The choices the page remembers: the theme, the layout (the room or the
// flat grid) and whether the ambient motion runs. Loaded first thing in
// <body> so all three are applied before anything paints.
//
// The theme half is adapted from mast-web web/theme.js at commit da7cf52
// (github.com/go-steer/mast-web, Apache-2.0): a theme is a
// body[data-theme] block in palette.css plus an entry in THEMES, the
// default is the absence of the attribute, and the choice is one
// localStorage key. Simian carries two of mast-web's eleven themes (Neon,
// the attribute-less base, and Cloud) plus its own Chaos pair, which is
// what a visitor gets with nothing stored: Chaos dark, or Chaos light
// when the browser prefers light.
window.SimianPrefs = (function () {
  "use strict";

  const THEME_KEY = "simian:theme";
  const LAYOUT_KEY = "simian:layout";
  const MOTION_KEY = "simian:motion";

  const THEMES = [
    { id: "chaos-dark", label: "Chaos · dark" },
    { id: "chaos-light", label: "Chaos · light" },
    { id: "default", label: "Neon · dark" },
    { id: "cloud-light", label: "Cloud · light" },
  ];

  function read(key) {
    try { return localStorage.getItem(key); } catch (e) { return null; }
  }

  function write(key, value) {
    try {
      if (value === null) localStorage.removeItem(key);
      else localStorage.setItem(key, value);
    } catch (e) { /* blocked storage: the choice holds for this visit */ }
  }

  const known = (id) => THEMES.some((t) => t.id === id);

  function storedTheme() {
    const id = read(THEME_KEY);
    return known(id) ? id : null;
  }

  function theme() {
    return storedTheme() || (matchMedia("(prefers-color-scheme: light)").matches ? "chaos-light" : "chaos-dark");
  }

  function applyTheme(id, remember) {
    if (id && id !== "default") document.body.setAttribute("data-theme", id);
    else document.body.removeAttribute("data-theme");
    if (remember) write(THEME_KEY, id);
  }

  // Fills the HUD's <select> and keeps it and the page in step.
  function mountTheme(selectEl) {
    for (const t of THEMES) selectEl.add(new Option(t.label, t.id));
    selectEl.value = theme();
    selectEl.addEventListener("change", () => applyTheme(selectEl.value, true));
  }

  // The room by default; the flat grid for anyone who asked for less
  // motion or whose window is too narrow for a room. An explicit choice
  // from the HUD outranks both, and is the only thing stored.
  const still = matchMedia("(prefers-reduced-motion: reduce)");
  const narrow = matchMedia("(max-width: 899px)");

  function layout() {
    const stored = read(LAYOUT_KEY);
    if (stored === "room" || stored === "flat") return stored;
    return still.matches || narrow.matches ? "flat" : "room";
  }

  function applyLayout(mode) {
    document.body.classList.toggle("room", mode === "room");
    document.body.classList.toggle("flat", mode === "flat");
  }

  // Ambient motion: Chaos's drifting glows, the panes' idle drift in the
  // room, the grid's crawl, the radar's sweep. Pretty on a machine with a
  // GPU, a drag on one without. Off by default for anyone who asked for
  // less motion, on otherwise; the HUD's toggle is stored and outranks
  // that. Off is a class on <html> (html.no-motion) that the stylesheets
  // key off, so it costs nothing. The camera, and anything you move,
  // still moves.
  function motion() {
    const stored = read(MOTION_KEY);
    if (stored === "on" || stored === "off") return stored === "on";
    return !still.matches;
  }

  function applyMotion(on) {
    document.documentElement.classList.toggle("no-motion", !on);
  }

  function mountMotion(btn) {
    const mark = document.createElement("span");
    mark.setAttribute("aria-hidden", "true");
    const word = document.createElement("span");
    word.className = "hud-motion-word";
    word.textContent = " motion";
    btn.replaceChildren(mark, word);
    btn.setAttribute("aria-label", "Ambient motion");
    const show = () => {
      const on = motion();
      btn.setAttribute("aria-pressed", String(on));
      mark.textContent = on ? "◉" : "◌";
      btn.title = on
        ? "Ambient motion is on: the glows and the panes drift. Turn it off on a slow machine."
        : "Ambient motion is off: the glows and the panes hold still. Turn it back on.";
    };
    btn.addEventListener("click", () => {
      const on = !motion();
      write(MOTION_KEY, on ? "on" : "off");
      applyMotion(on);
      show();
    });
    still.addEventListener("change", () => {
      if (read(MOTION_KEY)) return;
      applyMotion(motion());
      show();
    });
    show();
  }

  applyTheme(theme(), false);
  applyLayout(layout());
  applyMotion(motion());

  return {
    THEMES,
    theme,
    mountTheme,
    layout,
    applyLayout,
    motion,
    mountMotion,
    setLayout(mode) { write(LAYOUT_KEY, mode); applyLayout(mode); },
    // For the room to follow the defaults while nothing is stored.
    onDefaultsChange(fn) {
      const again = () => { if (!read(LAYOUT_KEY)) fn(layout()); };
      still.addEventListener("change", again);
      narrow.addEventListener("change", again);
    },
  };
})();
