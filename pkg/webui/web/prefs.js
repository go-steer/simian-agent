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

// The two choices the page remembers: the theme and the layout (the room
// or the flat grid). Loaded first thing in <body> so both are applied
// before anything paints.
//
// The theme half is adapted from mast-web web/theme.js at commit da7cf52
// (github.com/go-steer/mast-web, Apache-2.0): a theme is a
// body[data-theme] block in palette.css plus an entry in THEMES, the
// default is the absence of the attribute, and the choice is one
// localStorage key. Simian carries two of mast-web's eleven themes, and
// with nothing stored it follows the browser's light/dark preference.
window.SimianPrefs = (function () {
  "use strict";

  const THEME_KEY = "simian:theme";
  const LAYOUT_KEY = "simian:layout";

  const THEMES = [
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
    return storedTheme() || (matchMedia("(prefers-color-scheme: light)").matches ? "cloud-light" : "default");
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

  applyTheme(theme(), false);
  applyLayout(layout());

  return {
    THEMES,
    theme,
    mountTheme,
    layout,
    applyLayout,
    setLayout(mode) { write(LAYOUT_KEY, mode); applyLayout(mode); },
    // For the room to follow the defaults while nothing is stored.
    onDefaultsChange(fn) {
      const again = () => { if (!read(LAYOUT_KEY)) fn(layout()); };
      still.addEventListener("change", again);
      narrow.addEventListener("change", again);
    },
  };
})();
