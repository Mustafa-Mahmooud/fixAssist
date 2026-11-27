// Fix Assistant dashboard behaviour. Plain JS, no build step, CSP-safe
// (loaded from 'self', no inline handlers). Everything here is an
// enhancement: every page works without it.
(function () {
  "use strict";

  function ready(fn) {
    if (document.readyState !== "loading") fn();
    else document.addEventListener("DOMContentLoaded", fn);
  }

  // ---------- Toasts ----------
  var toastBox;
  function toast(msg) {
    if (!toastBox) {
      toastBox = document.createElement("div");
      toastBox.id = "toasts";
      toastBox.setAttribute("role", "status");
      toastBox.setAttribute("aria-live", "polite");
      document.body.appendChild(toastBox);
    }
    var t = document.createElement("div");
    t.className = "toast";
    var i = document.createElement("i");
    i.className = "ico ico-check";
    t.appendChild(i);
    t.appendChild(document.createTextNode(msg));
    toastBox.appendChild(t);
    setTimeout(function () {
      t.classList.add("out");
      setTimeout(function () { t.remove(); }, 350);
    }, 3200);
  }

  // ---------- Copy buttons: <button data-copy="#element-id" data-copied="Message"> ----------
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
    return new Promise(function (resolve, reject) {
      var ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy") ? resolve() : reject(); } catch (e) { reject(e); }
      ta.remove();
    });
  }

  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy]");
    if (!btn) return;
    var el = document.querySelector(btn.getAttribute("data-copy"));
    if (!el) return;
    copyText(el.textContent.trim()).then(function () {
      var label = btn.querySelector(".label") || btn;
      if (!btn.dataset.label) btn.dataset.label = label.textContent;
      label.textContent = "Copied!";
      btn.classList.add("copied");
      toast(btn.getAttribute("data-copied") || "Copied. Paste it wherever you need it.");
      clearTimeout(btn._t);
      btn._t = setTimeout(function () {
        label.textContent = btn.dataset.label;
        btn.classList.remove("copied");
      }, 2500);
    }, function () {
      // Clipboard blocked: select the text so Ctrl+C / long-press works.
      var r = document.createRange();
      r.selectNodeContents(el);
      var s = window.getSelection();
      s.removeAllRanges();
      s.addRange(r);
      toast("Text selected. Press Ctrl+C (or ⌘C) to copy.");
    });
  });

  // ---------- Whole rows and cards are clickable: <tr data-href="/..."> ----------
  document.addEventListener("click", function (e) {
    var tr = e.target.closest("[data-href]");
    if (!tr || e.target.closest("a, button, input, select, textarea, form")) return;
    if (window.getSelection && String(window.getSelection())) return; // selecting text, not clicking
    if (e.ctrlKey || e.metaKey) window.open(tr.dataset.href, "_blank");
    else location.href = tr.dataset.href;
  });

  // ---------- Confirmation dialog: <form data-confirm="Delete this funnel?" data-confirm-text="..." data-confirm-ok="Delete"> ----------
  var dialog;
  function confirmDialog(title, text, ok, onYes) {
    if (!window.HTMLDialogElement) { if (window.confirm(title)) onYes(); return; }
    if (!dialog) {
      dialog = document.createElement("dialog");
      dialog.className = "confirm";
      dialog.setAttribute("aria-labelledby", "confirm-title");
      dialog.innerHTML = '<form method="dialog"><h2 id="confirm-title"></h2><p></p>' +
        '<div class="row"><button value="cancel" class="secondary">Cancel</button><button value="ok" class="danger-solid"></button></div></form>';
      document.body.appendChild(dialog);
    }
    dialog.querySelector("h2").textContent = title;
    var p = dialog.querySelector("p");
    p.textContent = text || "";
    p.hidden = !text;
    dialog.querySelector("button[value=ok]").textContent = ok || "Yes, continue";
    dialog.returnValue = "";
    dialog.onclose = function () { if (dialog.returnValue === "ok") onYes(); };
    dialog.showModal();
    dialog.querySelector("button[value=cancel]").focus(); // safe default
  }

  // ---------- Forms: confirm destructive actions, show "working" state, no double submits ----------
  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (form.method === "dialog") return;
    var btn = e.submitter || form.querySelector("button[type=submit], button:not([type])");
    var src = btn && btn.hasAttribute("data-confirm") ? btn : form;
    var question = src.getAttribute("data-confirm");
    if (question && !form.dataset.confirmed) {
      e.preventDefault();
      confirmDialog(question, src.getAttribute("data-confirm-text"), src.getAttribute("data-confirm-ok"), function () {
        form.dataset.confirmed = "1";
        if (form.requestSubmit) form.requestSubmit(btn || undefined); else form.submit();
      });
      return;
    }
    delete form.dataset.confirmed;
    if (form.dataset.sending) {
      e.preventDefault();
      return;
    }
    form.dataset.sending = "1";
    if (btn) {
      // Disable after the browser has read the button's name/value.
      setTimeout(function () {
        btn.classList.add("busy");
        var busy = btn.getAttribute("data-busy");
        if (busy) {
          btn.dataset.label = btn.textContent;
          btn.textContent = busy;
        }
        btn.disabled = true;
      }, 0);
    }
    document.body.classList.add("loading");
  });

  function unbusy(root) {
    if (root.matches && root.matches("form[data-sending]")) delete root.dataset.sending;
    root.querySelectorAll("form[data-sending]").forEach(function (f) { delete f.dataset.sending; });
    root.querySelectorAll("button.busy").forEach(function (b) {
      b.classList.remove("busy");
      b.disabled = false;
      if (b.dataset.label && b.getAttribute("data-busy")) b.textContent = b.dataset.label;
    });
  }

  // Back button (page restored from cache): undo the "working" state.
  window.addEventListener("pageshow", function (e) {
    if (!e.persisted) return;
    document.body.classList.remove("loading");
    unbusy(document);
  });

  // htmx forms (Check my site) stay on the page: ready them for another go.
  document.addEventListener("htmx:afterRequest", function (e) {
    var form = e.detail && e.detail.elt && e.detail.elt.closest && e.detail.elt.closest("form");
    if (!form) return;
    document.body.classList.remove("loading");
    unbusy(form);
  });

  // ---------- Top progress bar on navigation and user-started htmx requests ----------
  document.addEventListener("click", function (e) {
    var a = e.target.closest("a[href]");
    if (!a || e.defaultPrevented || e.ctrlKey || e.metaKey || e.shiftKey || a.target === "_blank") return;
    if (a.hasAttribute("hx-get") || a.getAttribute("href").charAt(0) === "#") return;
    if (a.origin === location.origin) document.body.classList.add("loading");
  });
  // Tabs that swap content with htmx: move the highlight to the clicked tab.
  document.addEventListener("click", function (e) {
    var a = e.target.closest(".tabs a[hx-get]");
    if (!a) return;
    a.parentNode.querySelectorAll("a").forEach(function (x) { x.classList.toggle("on", x === a); });
  });
  function userStarted(evt) {
    var elt = evt.detail && evt.detail.elt;
    return elt && elt.matches && elt.matches("a, button, form");
  }
  document.addEventListener("htmx:beforeRequest", function (e) { if (userStarted(e)) document.body.classList.add("loading"); });
  document.addEventListener("htmx:afterRequest", function () { document.body.classList.remove("loading"); });

  // ---------- Numbers: thousands separators (12345 -> 12,345) ----------
  function formatNumbers(root) {
    root.querySelectorAll("[data-n]").forEach(function (el) {
      var raw = el.textContent.trim();
      if (!/^\d+$/.test(raw)) return;
      var n = parseInt(raw, 10);
      el.textContent = n.toLocaleString();
      el.dataset.n = n;
    });
  }

  // ---------- Search box and sortable columns ----------
  // <input data-filter="#issues"> hides rows of the tables inside #issues that
  // don't contain the typed words. <th data-sort="num|text"> sorts on click.
  // Both survive live refreshes and htmx swaps (they are re-applied).
  var sortState = {};

  function cellValue(tr, i, kind) {
    var td = tr.children[i];
    if (!td) return "";
    var n = td.querySelector("[data-n]");
    if (kind === "num") {
      var v = n ? n.dataset.n : td.textContent.replace(/[^\d.-]/g, "");
      return parseFloat(v) || 0;
    }
    return td.textContent.trim().toLowerCase();
  }

  function applySort(regionId) {
    var st = sortState[regionId];
    var region = document.getElementById(regionId);
    if (!st || !region) return;
    var th = region.querySelectorAll("thead th")[st.col];
    if (!th) return;
    region.querySelectorAll("thead th[data-sort]").forEach(function (h) { h.removeAttribute("aria-sort"); });
    th.setAttribute("aria-sort", st.dir > 0 ? "ascending" : "descending");
    var tbody = th.closest("table").tBodies[0];
    var kind = th.getAttribute("data-sort");
    var rows = Array.prototype.slice.call(tbody.rows);
    rows.sort(function (a, b) {
      var x = cellValue(a, st.col, kind), y = cellValue(b, st.col, kind);
      return (x > y ? 1 : x < y ? -1 : 0) * st.dir;
    });
    rows.forEach(function (r) { tbody.appendChild(r); });
  }

  document.addEventListener("click", function (e) {
    var th = e.target.closest("th[data-sort]");
    if (!th) return;
    var region = th.closest("[id]");
    if (!region) return;
    var col = Array.prototype.indexOf.call(th.parentNode.children, th);
    var prev = sortState[region.id];
    // Numbers sort biggest first on the first click; text A to Z.
    var first = th.getAttribute("data-sort") === "num" ? -1 : 1;
    sortState[region.id] = { col: col, dir: prev && prev.col === col ? -prev.dir : first };
    applySort(region.id);
  });
  document.addEventListener("keydown", function (e) {
    var th = e.target.closest && e.target.closest("th[data-sort]");
    if (th && (e.key === "Enter" || e.key === " ")) { e.preventDefault(); th.click(); }
  });

  function applyFilter(input) {
    var region = document.querySelector(input.getAttribute("data-filter"));
    if (!region) return;
    var words = input.value.toLowerCase().split(/\s+/).filter(Boolean);
    var shown = 0, total = 0;
    region.querySelectorAll("tbody tr").forEach(function (tr) {
      total++;
      var text = tr.textContent.toLowerCase();
      var ok = words.every(function (w) { return text.indexOf(w) >= 0; });
      tr.classList.toggle("filtered-out", !ok);
      if (ok) shown++;
    });
    var msg = region.querySelector(".no-match");
    if (total && !shown) {
      if (!msg) {
        msg = document.createElement("p");
        msg.className = "no-match muted";
        var table = region.querySelector("table");
        if (table) table.parentNode.appendChild(msg);
      }
      msg.textContent = "Nothing matches “" + input.value + "”.";
    } else if (msg) {
      msg.remove();
    }
  }
  document.addEventListener("input", function (e) {
    if (e.target.matches("[data-filter]")) applyFilter(e.target);
  });

  function reapply() {
    Object.keys(sortState).forEach(applySort);
    document.querySelectorAll("[data-filter]").forEach(function (i) { if (i.value) applyFilter(i); });
    formatNumbers(document);
  }
  document.addEventListener("htmx:afterSwap", reapply);

  // ---------- Live regions ----------
  // <div id="x" data-live="20"> is re-fetched from the current URL every 20 s
  // while the tab is visible. Changed numbers glow; new rows slide in.
  var baseTitle = document.title;
  var unseen = 0;

  function interacting(region) {
    var a = document.activeElement;
    if (a && region.contains(a) && a.matches("input, select, textarea")) return true;
    if (region.querySelector("button.busy, form[data-sending]")) return true;
    var s = window.getSelection && window.getSelection();
    if (s && !s.isCollapsed && s.anchorNode && region.contains(s.anchorNode)) return true;
    return false;
  }

  function keyed(root) {
    var m = {};
    root.querySelectorAll("[data-key]").forEach(function (el) { m[el.dataset.key] = el.textContent.trim(); });
    return m;
  }

  function swap(oldEl, newEl) {
    if (oldEl.innerHTML === newEl.innerHTML) return 0;
    var before = keyed(oldEl);
    var hadKeys = Object.keys(before).length > 0;
    var open = Array.prototype.map.call(oldEl.querySelectorAll("details"), function (d) { return d.open; });
    formatNumbers(newEl);
    oldEl.replaceWith(newEl);
    newEl.querySelectorAll("details").forEach(function (d, i) { if (i < open.length) d.open = open[i]; });
    if (window.htmx) window.htmx.process(newEl);
    var fresh = 0;
    newEl.querySelectorAll("[data-key]").forEach(function (el) {
      var k = el.dataset.key;
      if (!(k in before)) {
        if (hadKeys && el.tagName === "TR") { el.classList.add("fresh"); fresh++; }
      } else if (el.tagName !== "TR" && before[k] !== el.textContent.trim()) {
        el.classList.add("bump");
      }
    });
    return fresh;
  }

  function startLive() {
    var regions = document.querySelectorAll("[data-live][id]");
    if (!regions.length) return;
    var every = 60;
    regions.forEach(function (r) { every = Math.min(every, parseInt(r.getAttribute("data-live"), 10) || 60); });
    every = Math.max(10, every) * 1000;

    var pill = document.getElementById("live-pill");
    var pillText = pill && pill.querySelector(".label");
    var last = Date.now(), timer = null, inFlight = false, failed = 0;

    function label() {
      if (!pill) return;
      pill.hidden = false;
      var paused = document.hidden || failed >= 3;
      pill.classList.toggle("paused", paused);
      if (failed >= 3) { pillText.textContent = "Offline"; return; }
      if (document.hidden) { pillText.textContent = "Paused"; return; }
      var s = Math.round((Date.now() - last) / 1000);
      pillText.textContent = s < 5 ? "Live · just updated" : "Live · updated " + (s < 60 ? s + "s" : Math.round(s / 60) + " min") + " ago";
    }

    function refresh() {
      if (inFlight || document.hidden) return;
      inFlight = true;
      fetch(location.href, { credentials: "same-origin", headers: { "X-Live-Refresh": "1" } })
        .then(function (res) {
          if (res.redirected && /\/login/.test(res.url)) throw new Error("signed out");
          if (!res.ok) throw new Error(res.status);
          return res.text();
        })
        .then(function (html) {
          var doc = new DOMParser().parseFromString(html, "text/html");
          var fresh = 0;
          document.querySelectorAll("[data-live][id]").forEach(function (oldEl) {
            var newEl = doc.getElementById(oldEl.id);
            if (!newEl || interacting(oldEl)) return;
            fresh += swap(oldEl, newEl);
          });
          reapply();
          if (fresh) {
            if (document.hidden) {
              unseen += fresh;
              document.title = "(" + unseen + " new) " + baseTitle;
            } else {
              toast(fresh === 1 ? "A new issue just came in." : fresh + " new issues just came in.");
            }
          }
          last = Date.now();
          failed = 0;
        })
        .catch(function () { failed++; })
        .then(function () { inFlight = false; label(); });
    }

    function schedule() {
      clearInterval(timer);
      timer = setInterval(refresh, every);
    }

    document.addEventListener("visibilitychange", function () {
      if (!document.hidden) {
        unseen = 0;
        document.title = baseTitle;
        if (Date.now() - last > every / 2) refresh();
        schedule();
      }
      label();
    });
    if (pill) pill.addEventListener("click", function () { failed = 0; refresh(); });
    setInterval(label, 5000);
    label();
    schedule();
  }

  // ---------- Tabs: <div data-tabs> with <section id data-tab="Label"> children ----------
  // Without JS every section shows. Links to a section's #id (or anything
  // inside it) open its tab, so checklist links like /setup#agent-loop work.
  function setupTabs(box) {
    var panels = Array.prototype.filter.call(box.children, function (el) { return el.hasAttribute("data-tab"); });
    if (panels.length < 2) return;
    var nav = document.createElement("div");
    nav.className = "tab-nav";
    nav.setAttribute("role", "tablist");
    var buttons = panels.map(function (p) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "tab";
      b.setAttribute("role", "tab");
      b.textContent = p.getAttribute("data-tab");
      b.addEventListener("click", function () {
        show(p);
        if (p.id && history.replaceState) history.replaceState(null, "", "#" + p.id);
      });
      nav.appendChild(b);
      return b;
    });
    box.insertBefore(nav, box.firstChild);
    function show(target) {
      panels.forEach(function (p, i) {
        var on = p === target;
        p.hidden = !on;
        buttons[i].classList.toggle("on", on);
        buttons[i].setAttribute("aria-selected", on ? "true" : "false");
      });
    }
    function fromHash() {
      var id = location.hash.slice(1);
      var el = id && document.getElementById(id);
      var panel = el && panels.filter(function (p) { return p === el || p.contains(el); })[0];
      if (panel) {
        show(panel);
        // The browser scrolled before the tab was shown: scroll again.
        requestAnimationFrame(function () { el.scrollIntoView({ block: "start" }); });
        return true;
      }
      return false;
    }
    if (!fromHash()) show(panels[0]);
    window.addEventListener("hashchange", fromHash);
  }

  ready(function () {
    formatNumbers(document);
    startLive();
    document.querySelectorAll("[data-tabs]").forEach(setupTabs);
  });
})();
