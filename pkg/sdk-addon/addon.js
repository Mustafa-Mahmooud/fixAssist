/*
 * Fix Assistant SDK add-on. Loads after the pinned Sentry browser bundle and
 * before the app's own scripts, so it can wrap fetch/XHR before the app (or
 * supabase-js) captures a reference to them.
 *
 * Config (set before this script runs):
 *   window.__FIX_CONFIG = { dsn: "http://<key>@host:port/<project>", environment?: "production" }
 *   window.__fixConsent = false   // optional: errors only, no analytics
 *   window.__fixConsent = true    // optional: consented first-party visitor id
 *
 * Exposes window.fix = { track(name, props), identify(accountId) } and
 * window.__fix = { release(), sessionId, flush() }.
 */
(function () {
  "use strict";
  var w = window;
  var cfg = w.__FIX_CONFIG || {};
  var Sentry = w.Sentry;
  if (!Sentry || !cfg.dsn) return;

  var MAX_BODY = 2048;
  var dsn = new URL(cfg.dsn);
  var ingestOrigin = dsn.protocol + "//" + dsn.host;
  var publicKey = dsn.username;
  var projectId = dsn.pathname.replace(/^\/+/, "");

  // ---------- scrubbing (same rules as the server) ----------
  var RE_EMAIL = /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g;
  var RE_JWT = /eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*/g;
  var RE_BEARER = /\b(Bearer|Basic)\s+[A-Za-z0-9._~+\/=-]+/gi;
  var RE_CARD = /\b(?:\d[ -]?){12,18}\d\b/g;
  // Whole key names only (so "weak_password" or "token_type" aren't hit), and
  // never object/array values, so JSON bodies stay parseable.
  var RE_SECRET_KV = /(^|[^\w])("?(?:password|passwd|secret|token|api[_-]?key|apikey|access_token|refresh_token)"?\s*[:=]\s*)("[^"]*"|[^\s,&}\]{\["][^\s,&}\]]*)/gi;
  function scrubKV(m, pre, key, val) {
    return pre + key + (val.charAt(0) === '"' ? '"[secret]"' : "[secret]");
  }

  function scrubQuery(s) {
    // Keep query keys, drop values: ?id=eq.5&select=* -> ?id=&select=
    return s.replace(/([?&][^=&#\s"]+)=[^&#\s"]*/g, "$1=");
  }

  // Only Luhn-valid digit runs are card numbers; timestamps and order IDs are not.
  function luhn(digits) {
    var sum = 0, alt = false;
    for (var i = digits.length - 1; i >= 0; i--) {
      var d = digits.charCodeAt(i) - 48;
      if (alt) { d *= 2; if (d > 9) d -= 9; }
      sum += d;
      alt = !alt;
    }
    return sum % 10 === 0;
  }

  function scrubCard(m) {
    return luhn(m.replace(/[ -]/g, "")) ? "[card]" : m;
  }

  function scrub(s) {
    if (typeof s !== "string") return s;
    return scrubQuery(
      s.replace(RE_JWT, "[jwt]")
        .replace(RE_BEARER, "$1 [token]")
        .replace(RE_SECRET_KV, scrubKV)
        .replace(RE_EMAIL, "[email]")
        .replace(RE_CARD, scrubCard)
    );
  }

  function scrubDeep(v, depth) {
    if (depth > 8 || v == null) return v;
    if (typeof v === "string") return scrub(v);
    if (Array.isArray(v)) {
      for (var i = 0; i < v.length; i++) v[i] = scrubDeep(v[i], depth + 1);
      return v;
    }
    if (typeof v === "object") {
      for (var k in v) if (Object.prototype.hasOwnProperty.call(v, k)) v[k] = scrubDeep(v[k], depth + 1);
    }
    return v;
  }

  function beforeSend(event) {
    if (event.request) {
      delete event.request.cookies;
      delete event.request.data;
      var h = event.request.headers;
      if (h) {
        for (var k in h) if (/^(authorization|cookie|apikey|x-api-key)$/i.test(k)) delete h[k];
      }
    }
    if (event.user) {
      delete event.user.email;
      delete event.user.ip_address;
      delete event.user.username;
    }
    // Never send typed input values captured in breadcrumbs.
    if (event.breadcrumbs) {
      event.breadcrumbs = event.breadcrumbs.filter(function (b) {
        return b.category !== "ui.input";
      });
    }
    return scrubDeep(event, 0);
  }

  // ---------- release ID: hash of the app's script URLs ----------
  // FNV-1a 64-bit (as two 32-bit halves): synchronous, so it is ready before
  // the first event. Our own SDK scripts (data-fix-sdk) are excluded.
  function fnv64(str) {
    var h1 = 0x811c9dc5, h2 = 0x050c5d1f;
    for (var i = 0; i < str.length; i++) {
      var c = str.charCodeAt(i);
      h1 = Math.imul(h1 ^ c, 0x01000193) >>> 0;
      h2 = Math.imul(h2 ^ c, 0x01000193 ^ 0x5bd1e995) >>> 0;
    }
    return ("0000000" + h1.toString(16)).slice(-8) + ("0000000" + h2.toString(16)).slice(-8);
  }

  var cachedRelease = null;
  function computeRelease() {
    if (cachedRelease) return cachedRelease;
    var urls = [];
    var nodes = document.querySelectorAll("script[src],link[rel=modulepreload][href],link[rel=stylesheet][href]");
    for (var i = 0; i < nodes.length; i++) {
      var n = nodes[i];
      if (n.hasAttribute("data-fix-sdk")) continue;
      urls.push(n.src || n.href);
    }
    urls.sort();
    var id = "fix-" + fnv64(urls.join("\n"));
    // Only cache once the document is parsed; earlier calls may miss scripts.
    if (document.readyState !== "loading") cachedRelease = id;
    return id;
  }

  // ---------- Sentry init ----------
  Sentry.init({
    dsn: cfg.dsn,
    environment: cfg.environment || "production",
    beforeSend: beforeSend,
    sendDefaultPii: false,
  });
  Sentry.addEventProcessor(function (event) {
    event.release = computeRelease();
    // Anonymous identity for "affected users": the per-tab session ID. The
    // server HMACs it (or the app's own user.id) and drops the raw value.
    event.contexts = event.contexts || {};
    event.contexts.fix = { session_id: sessionId };
    return event;
  });

  // ---------- network failures ----------
  function isIngest(url) {
    return typeof url === "string" && url.indexOf(ingestOrigin) === 0;
  }

  function absUrl(u) {
    try {
      return new URL(u, location.href).href;
    } catch (e) {
      return String(u);
    }
  }

  function report(method, url, status, body, errorText) {
    var path;
    try {
      path = new URL(url).pathname;
    } catch (e) {
      path = url;
    }
    var value = status ? method + " " + path + " " + status : method + " " + path + " failed: " + (errorText || "network error");
    Sentry.captureEvent({
      level: "error",
      exception: { values: [{ type: "HTTPError", value: value, mechanism: { type: "fix.http", handled: true } }] },
      contexts: {
        fix_http: {
          method: method,
          url: scrub(url),
          status: status,
          body: scrub(body || ""),
          error: errorText || "",
        },
      },
    });
  }

  // Read at most MAX_BODY bytes of a cloned response.
  function readCapped(resp) {
    if (!resp.body || !resp.body.getReader) {
      return resp.text().then(function (t) {
        return t.slice(0, MAX_BODY);
      });
    }
    var reader = resp.body.getReader();
    var chunks = [], total = 0;
    function pump() {
      return reader.read().then(function (r) {
        if (r.done || total >= MAX_BODY) {
          if (!r.done) reader.cancel();
          var all = new Uint8Array(Math.min(total, MAX_BODY)), off = 0;
          for (var i = 0; i < chunks.length && off < all.length; i++) {
            var c = chunks[i].subarray(0, all.length - off);
            all.set(c, off);
            off += c.length;
          }
          return new TextDecoder().decode(all);
        }
        chunks.push(r.value);
        total += r.value.length;
        return pump();
      });
    }
    return pump();
  }

  if (w.fetch) {
    var origFetch = w.fetch;
    w.fetch = function (input, init) {
      var url = absUrl(typeof input === "string" || input instanceof URL ? String(input) : input && input.url);
      var method = ((init && init.method) || (input && input.method) || "GET").toUpperCase();
      var p = origFetch.apply(this, arguments);
      if (isIngest(url)) return p;
      return p.then(
        function (resp) {
          detectAuth(method, url, resp.status);
          if (resp.status >= 400) {
            try {
              readCapped(resp.clone())
                .then(function (body) {
                  report(method, url, resp.status, body);
                })
                .catch(function () {
                  report(method, url, resp.status, "");
                });
            } catch (e) {
              report(method, url, resp.status, "");
            }
          }
          return resp;
        },
        function (err) {
          if (!(err && err.name === "AbortError")) report(method, url, 0, "", err && err.message);
          throw err;
        }
      );
    };
  }

  // response.json() on a non-JSON reply (usually an HTML page from a wrong
  // URL): the Sentry SDK misses these rejections, and they're common in
  // AI-built apps. Report with the response's URL, status and type, then
  // rethrow unchanged. Dedupe drops the copy if the global handler also sees it.
  if (w.Response && w.Response.prototype.json) {
    var origJson = w.Response.prototype.json;
    w.Response.prototype.json = function () {
      var resp = this;
      return origJson.apply(this, arguments).catch(function (err) {
        if (err && err.name === "SyntaxError" && !isIngest(resp.url)) {
          var ct = "";
          try {
            ct = (resp.headers && resp.headers.get("content-type")) || "";
          } catch (e) {}
          Sentry.captureException(err, {
            contexts: { fix_json: { url: scrub(resp.url || ""), status: resp.status, content_type: ct } },
          });
        }
        throw err;
      });
    };
  }

  if (w.XMLHttpRequest) {
    var XHR = w.XMLHttpRequest.prototype;
    var origOpen = XHR.open, origSend = XHR.send;
    XHR.open = function (method, url) {
      this.__fix = { method: String(method || "GET").toUpperCase(), url: absUrl(url) };
      return origOpen.apply(this, arguments);
    };
    XHR.send = function () {
      var x = this, meta = x.__fix;
      if (meta && !isIngest(meta.url)) {
        x.addEventListener("loadend", function () {
          detectAuth(meta.method, meta.url, x.status);
          if (x.status >= 400) {
            var body = "";
            try {
              if (x.responseType === "" || x.responseType === "text") body = String(x.responseText).slice(0, MAX_BODY);
            } catch (e) {}
            report(meta.method, meta.url, x.status, body);
          } else if (x.status === 0) {
            report(meta.method, meta.url, 0, "", "network error");
          }
        });
      }
      return origSend.apply(this, arguments);
    };
  }

  // ---------- usage analytics (Phase 6) ----------
  // Cookieless by default: the server derives a daily-rotating visitor hash.
  // window.__fixConsent === false: errors only. === true: a first-party
  // visitor id in localStorage (better returning-visitor numbers).
  function analyticsOn() {
    return w.__fixConsent !== false;
  }

  var sessionId, newSession = false;
  try {
    sessionId = sessionStorage.getItem("__fix_sid");
    if (!sessionId) {
      sessionId = Math.random().toString(36).slice(2) + Date.now().toString(36);
      sessionStorage.setItem("__fix_sid", sessionId);
      newSession = true;
    }
  } catch (e) {
    sessionId = Math.random().toString(36).slice(2);
    newSession = true;
  }

  function visitorId() {
    if (w.__fixConsent !== true) return "";
    try {
      var v = localStorage.getItem("__fix_vid");
      if (!v) {
        v = Math.random().toString(36).slice(2) + Date.now().toString(36);
        localStorage.setItem("__fix_vid", v);
      }
      return v;
    } catch (e) {
      return "";
    }
  }

  function device() {
    var width = w.innerWidth || 1024;
    return width < 768 ? "mobile" : width < 1024 ? "tablet" : "desktop";
  }

  var queue = [];
  var keyParam = "?sentry_key=" + encodeURIComponent(publicKey);

  function flush() {
    if (!queue.length || !navigator.sendBeacon) return;
    var batch = queue.splice(0, 50);
    navigator.sendBeacon(ingestOrigin + "/v1/a" + keyParam,
      JSON.stringify({ session_id: sessionId, vid: visitorId(), device: device(), events: batch }));
    if (queue.length) flush();
  }

  function push(evt) {
    if (!analyticsOn()) return;
    evt.path = evt.path || location.pathname;
    queue.push(evt);
    if (queue.length >= 20) flush();
  }

  setInterval(flush, 5000);
  w.addEventListener("pagehide", flush);
  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "hidden") flush();
  });

  // Signups and logins, detected from successful auth responses (Supabase,
  // Firebase), so founders get signup numbers without any setup.
  function detectAuth(method, url, status) {
    if (status < 200 || status >= 300 || method !== "POST" || typeof url !== "string") return;
    if (/\/auth\/v1\/signup(\?|$)/.test(url) || /accounts:signUp/.test(url)) push({ type: "signup" });
    else if (/\/auth\/v1\/token\?grant_type=password/.test(url) || /accounts:signInWithPassword/.test(url)) push({ type: "login" });
  }

  function sendPageview(loadMs) {
    if (!analyticsOn() || !navigator.sendBeacon) return;
    navigator.sendBeacon(ingestOrigin + "/v1/pv" + keyParam, JSON.stringify({
      project_id: projectId, path: location.pathname, session_id: sessionId, release: computeRelease(),
      load_ms: loadMs, device: device(), vid: visitorId(), ts: new Date().toISOString(),
    }));
  }

  function firstPageview() {
    if (newSession) {
      var q = new URLSearchParams(location.search), utm = {};
      ["utm_source", "utm_medium", "utm_campaign"].forEach(function (k) {
        if (q.get(k)) utm[k] = scrub(q.get(k)).slice(0, 100);
      });
      push({ type: "session_start", referrer: document.referrer ? scrubQuery(document.referrer) : "", utm: utm });
    }
    var nav = performance.getEntriesByType && performance.getEntriesByType("navigation")[0];
    sendPageview(nav ? Math.round(nav.loadEventEnd || nav.domContentLoadedEventEnd || 0) : null);
  }

  // Single-page apps change pages without reloading: count route changes.
  var lastPath = location.pathname;
  function routeChanged() {
    setTimeout(function () {
      if (location.pathname !== lastPath) {
        lastPath = location.pathname;
        sendPageview(null);
      }
    }, 50);
  }
  ["pushState", "replaceState"].forEach(function (fn) {
    var orig = history[fn];
    if (!orig) return;
    history[fn] = function () {
      var r = orig.apply(this, arguments);
      routeChanged();
      return r;
    };
  });
  w.addEventListener("popstate", routeChanged);

  // Click autocapture: button and link text only (40 characters), never
  // input values; anything inside [data-fix-ignore] is skipped.
  document.addEventListener("click", function (e) {
    var el = e.target && e.target.closest && e.target.closest("a,button,[role=button],input[type=submit],input[type=button],[data-fix-track]");
    if (!el || el.closest("[data-fix-ignore]")) return;
    var label = el.getAttribute("data-fix-track") || el.getAttribute("aria-label") ||
      (el.tagName === "INPUT" ? el.value : el.innerText) || "";
    label = scrub(label.replace(/\s+/g, " ").trim()).slice(0, 40);
    if (!label) return;
    push({ type: "click", name: label, props: { tag: el.tagName.toLowerCase(), role: el.getAttribute("role") || "" } });
  }, true);

  if (document.readyState === "complete") setTimeout(firstPageview, 0);
  else w.addEventListener("load", function () { setTimeout(firstPageview, 0); });

  // Public API: fix.track(name, props) and fix.identify(accountId).
  w.fix = {
    track: function (name, props) {
      if (typeof name === "string" && name) push({ type: "custom", name: name.slice(0, 100), props: props || {} });
    },
    identify: function (accountId) {
      if (accountId != null && accountId !== "") push({ type: "identify", account: String(accountId) });
    },
  };
  w.__fix = { release: computeRelease, sessionId: sessionId, flush: flush };
})();
