/* MazeRegistryUI — the small amount of behaviour HTMX does not cover.
   Everything here is progressive: with JavaScript disabled the pages still
   navigate, only the conveniences (copy, filter, highlight) go away. */
(function () {
  "use strict";

  var doc = document;

  /* ---- toast ---- */

  var toastTimer = null;
  function toast(message) {
    var el = doc.getElementById("toast");
    if (!el) return;
    el.textContent = message;
    el.classList.add("is-visible");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      el.classList.remove("is-visible");
    }, 1600);
  }

  /* ---- copy to clipboard ---- */

  function copy(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    // Plain HTTP deployments are common for internal registries, and the
    // async clipboard API is unavailable there.
    return new Promise(function (resolve, reject) {
      var ta = doc.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      doc.body.appendChild(ta);
      ta.select();
      try {
        doc.execCommand("copy") ? resolve() : reject();
      } catch (e) {
        reject(e);
      } finally {
        doc.body.removeChild(ta);
      }
    });
  }

  doc.addEventListener("click", function (ev) {
    var btn = ev.target.closest("[data-copy]");
    if (!btn) return;
    ev.preventDefault();
    copy(btn.getAttribute("data-copy")).then(
      function () { toast("Copied"); },
      function () { toast("Could not copy"); }
    );
  });

  /* ---- confirm destructive submissions ---- */

  doc.addEventListener("submit", function (ev) {
    var message = ev.target.getAttribute && ev.target.getAttribute("data-confirm");
    if (message && !window.confirm(message)) {
      ev.preventDefault();
    }
  });

  /* ---- client-side tag filter ---- */

  function applyFilter(input) {
    var list = doc.querySelector(input.getAttribute("data-filter"));
    if (!list) return;
    var needle = input.value.trim().toLowerCase();
    var rows = list.querySelectorAll("[data-filter-key]");
    for (var i = 0; i < rows.length; i++) {
      var key = rows[i].getAttribute("data-filter-key").toLowerCase();
      rows[i].classList.toggle("is-hidden", needle !== "" && key.indexOf(needle) === -1);
    }
  }

  doc.addEventListener("input", function (ev) {
    if (ev.target.matches && ev.target.matches("[data-filter]")) applyFilter(ev.target);
  });

  /* ---- layer bar <-> layer list ---- */

  function highlightLayer(index, on) {
    var seg = doc.querySelector('.layerbar__seg[data-layer="' + index + '"]');
    var row = doc.getElementById("layer-" + index);
    if (seg) seg.classList.toggle("is-active", on);
    if (row) row.classList.toggle("is-active", on);
  }

  function layerIndexOf(el) {
    var host = el.closest ? el.closest("[data-layer]") : null;
    return host ? host.getAttribute("data-layer") : null;
  }

  ["mouseover", "focusin"].forEach(function (type) {
    doc.addEventListener(type, function (ev) {
      var i = layerIndexOf(ev.target);
      if (i) highlightLayer(i, true);
    });
  });
  ["mouseout", "focusout"].forEach(function (type) {
    doc.addEventListener(type, function (ev) {
      var i = layerIndexOf(ev.target);
      if (i) highlightLayer(i, false);
    });
  });

  // Clicking a segment jumps to the layer it stands for.
  doc.addEventListener("click", function (ev) {
    var seg = ev.target.closest(".layerbar__seg");
    if (!seg) return;
    var row = doc.getElementById("layer-" + seg.getAttribute("data-layer"));
    if (row) row.scrollIntoView({ block: "center", behavior: "smooth" });
  });

  /* ---- registry picker ---- */

  var picker = doc.getElementById("registry-picker");
  if (picker) {
    doc.addEventListener("click", function (ev) {
      if (picker.open && !picker.contains(ev.target)) picker.open = false;
    });
    doc.addEventListener("keydown", function (ev) {
      if (ev.key === "Escape" && picker.open) {
        picker.open = false;
        picker.querySelector("summary").focus();
      }
    });
  }

  /* ---- request progress ---- */

  var bar = doc.createElement("div");
  bar.className = "progress";
  doc.body.appendChild(bar);

  var inflight = 0;
  function begin() {
    inflight++;
    bar.classList.add("is-busy");
    bar.style.width = "70%";
  }
  function end() {
    inflight = Math.max(0, inflight - 1);
    if (inflight > 0) return;
    bar.style.width = "100%";
    setTimeout(function () {
      bar.classList.remove("is-busy");
      bar.style.width = "0";
    }, 220);
  }

  // Lazy row loads fire constantly while scrolling; showing the bar for those
  // would make the page look permanently busy.
  function isBackgroundLoad(detail) {
    var t = detail && detail.elt;
    return !!(t && t.getAttribute && (t.getAttribute("hx-trigger") || "").indexOf("revealed") !== -1);
  }

  doc.body.addEventListener("htmx:beforeRequest", function (ev) {
    if (!isBackgroundLoad(ev.detail)) begin();
  });
  doc.body.addEventListener("htmx:afterRequest", function (ev) {
    if (!isBackgroundLoad(ev.detail)) end();
  });

  doc.body.addEventListener("htmx:responseError", function () {
    toast("The server returned an error");
  });
  doc.body.addEventListener("htmx:sendError", function () {
    toast("Could not reach the server");
  });

  // Re-apply an active tag filter to rows that arrive after it was typed.
  doc.body.addEventListener("htmx:afterSwap", function () {
    var input = doc.querySelector("[data-filter]");
    if (input && input.value) applyFilter(input);
  });
})();
