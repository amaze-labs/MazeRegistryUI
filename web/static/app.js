// SPDX-License-Identifier: GPL-3.0-or-later
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

  /* ---- client-side tag filter: text and kind ---- */

  function activeKind() {
    var on = doc.querySelector(".chip[data-kind].is-on");
    return on ? on.getAttribute("data-kind") : "";
  }

  function applyFilter(input) {
    var list = doc.querySelector(input.getAttribute("data-filter"));
    if (!list) return;
    var needle = input.value.trim().toLowerCase();
    var kind = activeKind();
    var rows = list.querySelectorAll("[data-filter-key]");
    for (var i = 0; i < rows.length; i++) {
      var key = rows[i].getAttribute("data-filter-key").toLowerCase();
      var hideText = needle !== "" && key.indexOf(needle) === -1;
      var hideKind = kind !== "" && rows[i].getAttribute("data-kind") !== kind;
      rows[i].classList.toggle("is-hidden", hideText || hideKind);
    }
  }

  doc.addEventListener("input", function (ev) {
    if (ev.target.matches && ev.target.matches("[data-filter]")) applyFilter(ev.target);
  });

  doc.addEventListener("click", function (ev) {
    var chip = ev.target.closest(".chip[data-kind]");
    if (!chip) return;
    var all = doc.querySelectorAll(".chip[data-kind]");
    for (var i = 0; i < all.length; i++) all[i].classList.toggle("is-on", all[i] === chip);
    var input = doc.querySelector("[data-filter]");
    if (input) applyFilter(input);
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

  /* ---- phone: a tap opens a clamped layer instruction ---- */

  doc.addEventListener("click", function (ev) {
    var layer = ev.target.closest(".layer");
    if (!layer || ev.target.closest("button, a")) return;
    layer.classList.toggle("is-open");
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

  /* ---- row selection, the pull bar and the keyboard ---- */

  var pull = doc.getElementById("pull");
  var pullBase = pull && pull.getAttribute("data-pull-base");

  function rows() {
    return Array.prototype.filter.call(
      doc.querySelectorAll("#main .row[data-href]"),
      function (r) { return r.offsetParent !== null; }
    );
  }

  function pullFor(row) {
    if (!row) return "";
    if (row.hasAttribute("data-pull")) return "docker pull " + row.getAttribute("data-pull");
    if (pullBase && row.hasAttribute("data-tag")) return "docker pull " + pullBase + ":" + row.getAttribute("data-tag");
    return "";
  }

  // The repository page's pull bar follows the selected row.
  function showPull(row) {
    if (!pullBase || !row || !row.hasAttribute("data-tag")) return;
    var tag = row.getAttribute("data-tag");
    var ref = pull.querySelector(".pull__ref");
    var btn = pull.querySelector(".pull__copy");
    if (ref) ref.textContent = tag;
    if (btn) btn.setAttribute("data-copy", "docker pull " + pullBase + ":" + tag);
  }

  function select(row, scroll) {
    var prev = doc.querySelector("#main .row.is-selected");
    if (prev) prev.classList.remove("is-selected");
    if (!row) return;
    row.classList.add("is-selected");
    showPull(row);
    if (scroll) row.scrollIntoView({ block: "nearest" });
  }

  doc.addEventListener("click", function (ev) {
    var row = ev.target.closest && ev.target.closest("#main .row[data-href]");
    if (!row || ev.target.closest("a, button, input, summary")) return;
    // Where a pull bar can follow the selection, a click selects; elsewhere a
    // row is just a big link.
    if (pullBase) {
      select(row, false);
    } else {
      window.location.href = row.getAttribute("data-href");
    }
  });

  function typing(el) {
    return el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName));
  }

  doc.addEventListener("keydown", function (ev) {
    if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
    var t = ev.target;
    if (typing(t)) {
      if (ev.key === "Escape") t.blur();
      if (ev.key === "ArrowDown" && t.matches("[data-hotkey]")) {
        ev.preventDefault();
        t.blur();
        select(rows()[0], true);
      }
      return;
    }
    var list, cur, i;
    switch (ev.key) {
      case "/": {
        var box = doc.querySelector("[data-hotkey='/']") || doc.querySelector(".jump__input");
        if (box && box.offsetParent !== null) {
          ev.preventDefault();
          box.focus();
          box.select();
        }
        return;
      }
      case "j":
      case "ArrowDown":
      case "k":
      case "ArrowUp":
        list = rows();
        if (!list.length) return;
        ev.preventDefault();
        cur = doc.querySelector("#main .row.is-selected");
        i = list.indexOf(cur);
        if (ev.key === "j" || ev.key === "ArrowDown") i = Math.min(list.length - 1, i + 1);
        else i = i < 0 ? 0 : Math.max(0, i - 1);
        select(list[i], true);
        return;
      case "Enter":
        cur = doc.querySelector("#main .row.is-selected");
        if (cur) window.location.href = cur.getAttribute("data-href");
        return;
      case "c":
        cur = doc.querySelector("#main .row.is-selected");
        var cmd = pullFor(cur);
        if (cmd) copy(cmd).then(function () { toast("Copied " + cmd.replace("docker pull ", "")); });
        return;
      case "Escape":
        select(null);
        return;
    }
  });

  /* ---- image page: pull by tag or by digest ---- */

  doc.addEventListener("click", function (ev) {
    var opt = ev.target.closest("[data-pull-cmd]");
    if (!opt) return;
    var box = opt.closest(".pull");
    var cmd = opt.getAttribute("data-pull-cmd");
    var text = box.querySelector("[data-pull-text]");
    var btn = box.querySelector(".pull__copy");
    if (text) text.textContent = cmd;
    if (btn) btn.setAttribute("data-copy", cmd);
    var opts = box.querySelectorAll("[data-pull-cmd]");
    for (var i = 0; i < opts.length; i++) opts[i].classList.toggle("is-on", opts[i] === opt);
  });

  /* ---- repository page: which version a moving tag points at ---- */

  // "latest" and friends are pointers. Once the rows have resolved, a moving
  // tag that shares its digest with a version gets that version next to it.
  var aliasTimer = null;
  function markAliases() {
    var list = doc.querySelectorAll("#tag-list .row[data-digest]");
    var byDigest = {};
    var i, d;
    for (i = 0; i < list.length; i++) {
      d = list[i].getAttribute("data-digest");
      if (list[i].getAttribute("data-kind") !== "moving" && !byDigest[d]) {
        byDigest[d] = list[i].getAttribute("data-tag");
      }
    }
    for (i = 0; i < list.length; i++) {
      if (list[i].getAttribute("data-kind") !== "moving") continue;
      var slot = list[i].querySelector(".alias");
      d = list[i].getAttribute("data-digest");
      if (slot) slot.textContent = byDigest[d] ? "= " + byDigest[d] : "";
    }
  }

  doc.body.addEventListener("htmx:afterSwap", function () {
    // Re-apply an active filter to rows that arrive after it was typed.
    var input = doc.querySelector("[data-filter]");
    if (input && (input.value || activeKind())) applyFilter(input);
    clearTimeout(aliasTimer);
    aliasTimer = setTimeout(markAliases, 120);
  });
})();
