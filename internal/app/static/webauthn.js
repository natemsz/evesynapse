// Second factor: the browser half of the two WebAuthn
// ceremonies (navigator.credentials), loaded only on the
// factor, enrollment, and settings Security pages.
//
// Like every page script it wires on load and on every
// content-ready, idempotently with an identity guard: the swap
// shell adopts scripts once, and the factor page also works
// from a cold full load since it sits in the sign-in path.
(function () {
  'use strict';

  var messages = {
    signin: 'Sign in again, then try once more.',
    none: 'No key is enrolled on this account.',
    verify: 'The key did not verify. Try again.',
    stale: 'That step expired. Start again.',
    exists: 'That key is already enrolled.',
    expired: 'That step expired. Sign in again.',
    locked: 'Too many failed attempts. Sign in again.',
    begin: 'Something went wrong. Try again.',
    store: 'Something went wrong. Try again.',
    rp: 'Something went wrong. Try again.',
    state: 'Something went wrong. Try again.',
    pending: 'Finish signing in first.'
  };

  function messageFor(code) {
    return messages[code] || 'Something went wrong. Try again.';
  }

  function wire(el) {
    if (!el || el.__esWebauthnWired) return false;
    el.__esWebauthnWired = true;
    return true;
  }

  // The wire format is base64url strings; the credentials API
  // wants ArrayBuffers. These convert both ways.
  function toBuffer(s) {
    s = s.replace(/-/g, '+').replace(/_/g, '/');
    while (s.length % 4) s += '=';
    var raw = window.atob(s);
    var buf = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) buf[i] = raw.charCodeAt(i);
    return buf.buffer;
  }

  function toBase64url(buf) {
    var bytes = new Uint8Array(buf);
    var s = '';
    for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
    return window.btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function postJSON(url, body) {
    return window.fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {}),
      credentials: 'same-origin'
    }).then(function (resp) {
      return resp.json().then(function (data) {
        if (!resp.ok) throw new Error(data.error || 'begin');
        return data;
      });
    });
  }

  function showError(el, err) {
    if (!el) return;
    var code = (err && err.message) || 'begin';
    // A cancelled key prompt is not an error worth words.
    if (code === 'NotAllowedError') {
      el.hidden = true;
      return;
    }
    el.textContent = messageFor(code);
    el.hidden = false;
  }

  function hideError(el) {
    if (el) el.hidden = true;
  }

  function serialiseCredential(cred) {
    var out = {
      id: cred.id,
      rawId: toBase64url(cred.rawId),
      type: cred.type,
      response: {}
    };
    var r = cred.response;
    if (r.clientDataJSON) out.response.clientDataJSON = toBase64url(r.clientDataJSON);
    if (r.authenticatorData) out.response.authenticatorData = toBase64url(r.authenticatorData);
    if (r.signature) out.response.signature = toBase64url(r.signature);
    if (r.userHandle) out.response.userHandle = toBase64url(r.userHandle);
    if (r.attestationObject) out.response.attestationObject = toBase64url(r.attestationObject);
    if (typeof cred.getClientExtensionResults === 'function') {
      out.clientExtensionResults = cred.getClientExtensionResults();
    }
    return out;
  }

  // runAssertion plays one key challenge: options from begin,
  // the browser prompt, the answer to finish. It resolves to
  // the redirect the server answers with.
  function runAssertion(beginURL, finishURL) {
    return postJSON(beginURL).then(function (options) {
      var pk = options.publicKey || options;
      pk.challenge = toBuffer(pk.challenge);
      (pk.allowCredentials || []).forEach(function (c) {
        c.id = toBuffer(c.id);
      });
      return window.navigator.credentials.get({ publicKey: pk });
    }).then(function (cred) {
      if (!cred) throw new Error('verify');
      return postJSON(finishURL, serialiseCredential(cred));
    }).then(function (data) {
      return data.redirect || '/';
    });
  }

  // runRegistration plays one key enrollment for name. It
  // resolves to the server's answer: recovery codes on a first
  // key, else a redirect.
  function runRegistration(beginURL, finishURL, name) {
    return postJSON(beginURL, { name: name }).then(function (options) {
      var pk = options.publicKey || options;
      pk.challenge = toBuffer(pk.challenge);
      pk.user.id = toBuffer(pk.user.id);
      (pk.excludeCredentials || []).forEach(function (c) {
        c.id = toBuffer(c.id);
      });
      return window.navigator.credentials.create({ publicKey: pk });
    }).then(function (cred) {
      if (!cred) throw new Error('verify');
      return postJSON(finishURL, serialiseCredential(cred));
    });
  }

  function showCodes(listEl, codes) {
    while (listEl.firstChild) listEl.removeChild(listEl.firstChild);
    codes.forEach(function (code) {
      var li = document.createElement('li');
      var el = document.createElement('code');
      el.textContent = code;
      li.appendChild(el);
      listEl.appendChild(li);
    });
  }

  function keysAvailable() {
    return !!(window.navigator && window.navigator.credentials &&
      window.fetch && window.atob);
  }

  function init() {
    // --- factor page ------------------------------------------
    var factorGo = document.getElementById('factor-go');
    if (wire(factorGo)) {
      factorGo.addEventListener('click', function () {
        var root = document.getElementById('factor-assert');
        var status = document.getElementById('factor-status');
        hideError(status);
        if (!keysAvailable()) {
          showError(status, new Error('begin'));
          return;
        }
        factorGo.disabled = true;
        runAssertion(root.getAttribute('data-begin'), root.getAttribute('data-finish')).then(function (redirect) {
          window.location.assign(redirect);
        }, function (err) {
          factorGo.disabled = false;
          showError(status, err);
        });
      });
    }

    // --- enrollment page --------------------------------------
    var enrollGo = document.getElementById('enroll-go');
    if (wire(enrollGo)) {
      enrollGo.addEventListener('click', function () {
        var root = document.getElementById('enroll-root');
        var status = document.getElementById('enroll-status');
        hideError(status);
        if (!keysAvailable()) {
          showError(status, new Error('begin'));
          return;
        }
        enrollGo.disabled = true;
        var name = document.getElementById('enroll-name').value || 'Security key';
        runRegistration(root.getAttribute('data-begin'), root.getAttribute('data-finish'), name).then(function (data) {
          if (data.redirect) {
            window.location.assign(data.redirect);
            return;
          }
          document.getElementById('enroll-form').hidden = true;
          showCodes(document.getElementById('enroll-code-list'), data.codes || []);
          document.getElementById('enroll-codes').hidden = false;
        }, function (err) {
          enrollGo.disabled = false;
          showError(status, err);
        });
      });
    }
    var enrollConfirm = document.getElementById('enroll-confirm');
    if (wire(enrollConfirm)) {
      enrollConfirm.addEventListener('click', function () {
        var root = document.getElementById('enroll-root');
        enrollConfirm.disabled = true;
        postJSON(root.getAttribute('data-confirm')).then(function (data) {
          window.location.assign(data.redirect || '/');
        }, function () {
          enrollConfirm.disabled = false;
          window.location.assign('/auth/enroll');
        });
      });
    }

    // --- settings security section ----------------------------
    var addGo = document.getElementById('security-add');
    if (wire(addGo)) {
      addGo.addEventListener('click', function () {
        var root = document.getElementById('security-register');
        var status = document.getElementById('security-status');
        hideError(status);
        if (!keysAvailable()) {
          showError(status, new Error('begin'));
          return;
        }
        addGo.disabled = true;
        var name = document.getElementById('security-name').value || 'Security key';
        // Adding to an enabled account needs a fresh factor:
        // assert first, then enroll, without leaving the page.
        var ready = Promise.resolve();
        if (root.getAttribute('data-enabled') === 'true' && root.getAttribute('data-fresh') !== 'true') {
          ready = runAssertion('/auth/factor/begin', '/auth/factor/finish').then(function () {
            root.setAttribute('data-fresh', 'true');
          });
        }
        ready.then(function () {
          return runRegistration(root.getAttribute('data-begin'), root.getAttribute('data-finish'), name);
        }).then(function (data) {
          if (data.redirect) {
            window.location.assign(data.redirect);
            return;
          }
          document.getElementById('security-form').hidden = true;
          showCodes(document.getElementById('security-code-list'), data.codes || []);
          document.getElementById('security-codes').hidden = false;
        }, function (err) {
          addGo.disabled = false;
          showError(status, err);
        });
      });
    }
    var doneBtn = document.getElementById('security-done');
    if (wire(doneBtn)) {
      doneBtn.addEventListener('click', function () {
        window.location.assign('/settings#security');
        window.location.reload();
      });
    }
    // Removing a key or regenerating codes needs a fresh
    // factor too: assert inline, then let the form go.
    var freshForms = document.querySelectorAll('form[data-needs-fresh]');
    for (var i = 0; i < freshForms.length; i++) {
      (function (form) {
        if (!wire(form)) return;
        form.addEventListener('submit', function (ev) {
          var root = document.getElementById('security-register');
          if (!root || root.getAttribute('data-fresh') === 'true') return;
          ev.preventDefault();
          var status = document.getElementById('security-status');
          hideError(status);
          if (!keysAvailable()) {
            showError(status, new Error('begin'));
            return;
          }
          runAssertion('/auth/factor/begin', '/auth/factor/finish').then(function () {
            root.setAttribute('data-fresh', 'true');
            form.submit();
          }, function (err) {
            showError(status, err);
          });
        });
      })(freshForms[i]);
    }
  }

  init();
  document.addEventListener('evesynapse:content-ready', init);
})();
