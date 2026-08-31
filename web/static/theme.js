// SPDX-License-Identifier: GPL-3.0-or-later
/* Resolves the theme before first paint so the page never flashes the wrong
   one. Loaded synchronously in <head> on purpose: a deferred script would run
   after the first paint and the flash is exactly what we are avoiding. */
(function () {
  try {
    var m = document.cookie.match(/(?:^|; )mrui_theme=([^;]+)/);
    var t = m && decodeURIComponent(m[1]);
    if (t !== "dark" && t !== "light") {
      t = window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
    }
    document.documentElement.dataset.theme = t;
  } catch (e) {
    document.documentElement.dataset.theme = "dark";
  }
})();
