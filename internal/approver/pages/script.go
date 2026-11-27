package pages

// script is the one inline script every page carries. It only wires buttons
// that exist on the page: the FROM tap, approve and deny, passkey registration
// and the queue sign-in. It never builds markup from server data.
const script = `
(function () {
  "use strict";
  var $ = function (id) { return document.getElementById(id); };
  var statusEl = $("status");
  function say(msg, bad) {
    if (!statusEl) return;
    statusEl.textContent = msg;
    statusEl.className = "status" + (bad ? " error" : "");
  }
  function b2u(buf) {
    var s = "", b = new Uint8Array(buf);
    for (var i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }
  function u2b(s) {
    s = s.replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    var r = atob(s), a = new Uint8Array(r.length);
    for (var i = 0; i < r.length; i++) a[i] = r.charCodeAt(i);
    return a.buffer;
  }
  function send(url, body) {
    return fetch(url, {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {})
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) throw new Error(j.message || j.error || ("HTTP " + r.status));
        return j;
      });
    });
  }
  function decodeKey(pk) {
    pk.challenge = u2b(pk.challenge);
    if (pk.user && pk.user.id) pk.user.id = u2b(pk.user.id);
    ["allowCredentials", "excludeCredentials"].forEach(function (k) {
      if (pk[k]) pk[k].forEach(function (c) { c.id = u2b(c.id); });
    });
    return pk;
  }
  function assertion(c) {
    var r = c.response;
    return {
      id: c.id, rawId: b2u(c.rawId), type: c.type,
      response: {
        clientDataJSON: b2u(r.clientDataJSON), authenticatorData: b2u(r.authenticatorData),
        signature: b2u(r.signature), userHandle: r.userHandle ? b2u(r.userHandle) : undefined
      },
      authenticatorAttachment: c.authenticatorAttachment || undefined,
      clientExtensionResults: c.getClientExtensionResults()
    };
  }
  function attestation(c) {
    var r = c.response;
    return {
      id: c.id, rawId: b2u(c.rawId), type: c.type,
      response: {
        clientDataJSON: b2u(r.clientDataJSON), attestationObject: b2u(r.attestationObject),
        transports: r.getTransports ? r.getTransports() : []
      },
      authenticatorAttachment: c.authenticatorAttachment || undefined,
      clientExtensionResults: c.getClientExtensionResults()
    };
  }
  function busy(on) {
    document.querySelectorAll("button").forEach(function (b) { b.disabled = on; });
  }
  var base = location.pathname.replace(/\/+$/, "");

  function ceremony(beginURL, finishURL, beginBody, getter, encode, done) {
    busy(true);
    say("Waiting for your passkey...");
    send(beginURL, beginBody).then(function (j) {
      return getter(decodeKey(j.publicKey));
    }).then(function (c) {
      return send(finishURL, encode(c));
    }).then(done).catch(function (e) {
      say(e && e.message ? e.message : "That did not work.", true);
      busy(false);
    });
  }
  function get(pk) { return navigator.credentials.get({ publicKey: pk }); }
  function create(pk) { return navigator.credentials.create({ publicKey: pk }); }
  function reload() { location.reload(); }

  var fromBtn = $("confirm-from");
  if (fromBtn) fromBtn.addEventListener("click", function () {
    fromBtn.disabled = true;
    send(base + "/from").then(function () {
      var step = $("from-step");
      if (step) step.hidden = true;
      var d = $("decide");
      if (d) d.hidden = false;
      say("Sender confirmed. Approve with your passkey, or deny.");
      var a = $("approve");
      if (a) a.focus();
    }).catch(function (e) { fromBtn.disabled = false; say(e.message, true); });
  });

  function decide(action) {
    var drops = [];
    document.querySelectorAll('input[name="drop-item"]').forEach(function (i) { if (!i.checked) drops.push(i.value); });
    ceremony(base + "/begin", base + "/finish", { action: action, drop: drops }, get, assertion, reload);
  }
  var ap = $("approve"), dn = $("deny");
  if (ap) ap.addEventListener("click", function () { decide("approve"); });
  if (dn) dn.addEventListener("click", function () { decide("deny"); });

  var reg = $("register");
  if (reg) reg.addEventListener("click", function () {
    ceremony(base + "/begin", base + "/finish", {}, create, attestation, function () {
      say("Passkey saved. You can close this page.");
    });
  });

  var login = $("login");
  if (login) login.addEventListener("click", function () {
    ceremony("/approve/queue/login/begin", "/approve/queue/login/finish", {}, get, assertion, reload);
  });
})();
`
