// Progressive enhancement for the cluster admin page.
//
// Without this script every form is a normal POST: the server falls back to
// post/redirect/get for actions and renders a result page for spoke/box
// creation. With it, forms submit over fetch() and the page updates in place,
// so a browser refresh re-issues the GET and never resubmits a create. The
// markup works unchanged with JavaScript disabled.
(function () {
  "use strict";

  function el(id) {
    return document.getElementById(id);
  }

  // setFlash renders the one-line success/error banner (or clears it).
  function setFlash(kind, text) {
    var flash = el("flash");
    if (!flash) return;
    if (!text) {
      flash.replaceChildren();
      return;
    }
    var banner = document.createElement("div");
    banner.className = "banner " + (kind === "err" ? "err" : "ok");
    banner.textContent = text;
    flash.replaceChildren(banner);
  }

  function labelled(label, valueNode) {
    var frag = document.createDocumentFragment();
    var l = document.createElement("div");
    l.className = "label";
    l.textContent = label;
    frag.appendChild(l);
    frag.appendChild(valueNode);
    return frag;
  }

  // showSpokeResult renders the one-time join token and ready-to-run command.
  function showSpokeResult(s) {
    var box = document.createElement("div");
    box.className = "result";

    var head = document.createElement("strong");
    head.textContent =
      'Join token for spoke "' + s.name + '" — one-time use, shown once.';
    box.appendChild(head);

    var tok = document.createElement("div");
    tok.className = "val";
    tok.textContent = s.token;
    box.appendChild(labelled("Token", tok));

    var cmd = document.createElement("pre");
    cmd.className = "cmd";
    cmd.textContent = s.command;
    box.appendChild(labelled("Start the spoke with", cmd));

    var note = document.createElement("p");
    note.className = "note";
    note.textContent =
      "Runs the spoke as a daemon with a persistent state volume (so it " +
      "reconnects without this one-time token) and grants the Docker socket's " +
      "group (the spoke runs as a non-root user). Adjust the image tag to match " +
      "your deployment.";
    box.appendChild(note);

    var results = el("results");
    if (results) results.replaceChildren(box);
  }

  // showBoxResult renders the activation URL for a freshly created box.
  function showBoxResult(b) {
    var box = document.createElement("div");
    box.className = "result";

    var head = document.createElement("strong");
    head.textContent =
      'Box "' + b.boxId + '" created on spoke "' + b.spoke + '".';
    box.appendChild(head);

    var link = document.createElement("a");
    link.href = b.authUrl;
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    link.textContent = b.authUrl;
    var val = document.createElement("div");
    val.className = "val";
    val.appendChild(link);
    box.appendChild(labelled("Activation URL (open to finish sign-in)", val));

    var results = el("results");
    if (results) results.replaceChildren(box);
  }

  // refresh re-fetches the dashboard and swaps in fresh Spokes and Boxes cards,
  // leaving the flash and result areas (just set from the action) untouched.
  function refresh() {
    return fetch("/admin", {
      headers: { Accept: "text/html" },
      credentials: "same-origin",
    })
      .then(function (resp) {
        return resp.text();
      })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, "text/html");
        ["spokes-card", "boxes-card"].forEach(function (id) {
          var fresh = doc.getElementById(id);
          var cur = el(id);
          if (fresh && cur) cur.replaceWith(fresh);
        });
      });
  }

  function submitButton(form) {
    return form.querySelector('button[type="submit"], button:not([type])');
  }

  function onSubmit(e) {
    var form = e.target;
    if (!(form instanceof HTMLFormElement)) return;
    var action = form.getAttribute("action") || "";
    if (action.indexOf("/admin/") !== 0) return;
    // An inline confirm() that returned false already cancelled the submit.
    if (e.defaultPrevented) return;
    e.preventDefault();

    var btn = submitButton(form);
    if (btn) btn.disabled = true;

    fetch(action, {
      method: "POST",
      headers: { Accept: "application/json" },
      body: new FormData(form),
      credentials: "same-origin",
    })
      .then(function (resp) {
        var ct = resp.headers.get("Content-Type") || "";
        if (ct.indexOf("application/json") !== -1) return resp.json();
        // Auth/CSRF failures come back as plain text with a 4xx status.
        return resp.text().then(function (t) {
          return { ok: resp.ok, err: resp.ok ? "" : t.trim() || "HTTP " + resp.status };
        });
      })
      .then(function (data) {
        if (data.err) setFlash("err", data.err);
        else if (data.msg) setFlash("ok", data.msg);
        else setFlash();
        if (data.newSpoke) showSpokeResult(data.newSpoke);
        if (data.newBox) showBoxResult(data.newBox);
        return refresh();
      })
      .catch(function (err) {
        setFlash("err", "Request failed: " + err.message);
      })
      .finally(function () {
        if (btn) btn.disabled = false;
      });
  }

  // Delegated so it keeps working after refresh() swaps in fresh card markup.
  document.addEventListener("submit", onSubmit);
})();
