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

  function say(el, message) {
    if (!el) return;
    el.textContent = message || '';
    el.hidden = !message;
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

  function requestOptions(options) {
    var pk = options.publicKey || options;
    pk.challenge = toBuffer(pk.challenge);
    (pk.allowCredentials || []).forEach(function (c) {
      c.id = toBuffer(c.id);
    });
    return pk;
  }

  function creationOptions(options) {
    var pk = options.publicKey || options;
    pk.challenge = toBuffer(pk.challenge);
    pk.user.id = toBuffer(pk.user.id);
    (pk.excludeCredentials || []).forEach(function (c) {
      c.id = toBuffer(c.id);
    });
    return pk;
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

  async function hasNativeHybrid() {
    if (!window.PublicKeyCredential || !window.navigator.credentials) return false;
    if (typeof PublicKeyCredential.getClientCapabilities === 'function') {
      try {
        var caps = await PublicKeyCredential.getClientCapabilities();
        if (typeof caps.hybridTransport === 'boolean') return caps.hybridTransport;
      } catch (err) { /* use the conservative browser fallback below */ }
    }
    // Older Firefox builds expose WebAuthn without hybrid transport.
    // Other browsers keep their existing native prompt unless they
    // explicitly report that hybrid is unavailable.
    return !/firefox|fxios/i.test(navigator.userAgent);
  }

  function initWebAuthnQR() {
    var boxes = document.querySelectorAll('[data-webauthn-qr]');
    var nativeHybridPromise = boxes.length ? hasNativeHybrid() : Promise.resolve(true);
    if (boxes.length) {
      nativeHybridPromise.then(function (nativeHybrid) {
        if (!nativeHybrid) {
          Array.prototype.forEach.call(boxes, function (box) { box.hidden = false; });
        }
      });
    }

    Array.prototype.forEach.call(document.querySelectorAll('[data-webauthn-qr]'), function (box) {
      var button = box.querySelector('[data-webauthn-qr-start]');
      var panel = box.querySelector('[data-qr-panel]');
      var img = box.querySelector('[data-qr-img]');
      var code = box.querySelector('[data-qr-code]');
      var status = box.querySelector('[data-qr-status]');
      var cancel = box.querySelector('[data-qr-cancel]');
      if (!panel || !img || !code || !status || !cancel) return;

      var timer = null;
      var active = false;
      var starting = false;
      var reset = function () {
        img.hidden = true;
        img.removeAttribute('src');
        code.textContent = '';
      };
      var stop = function () {
        active = false;
        if (timer) window.clearTimeout(timer);
        timer = null;
      };
      var poll = async function () {
        if (!active) return;
        try {
          var response = await fetch('/auth/factor/qr/status', {
            credentials: 'same-origin', headers: { Accept: 'application/json' }
          });
          var body = await response.json();
          if (!active) return;
          if (!response.ok) {
            stop(); reset();
            say(status, messageFor(body.error || 'expired'));
            return;
          }
          if (body.status === 'done') {
            stop();
            if (Array.isArray(body.codes)) {
              var mode = box.getAttribute('data-qr-for');
              var form = document.getElementById(mode === 'enroll' ? 'enroll-form' : 'security-form');
              var codesRoot = document.getElementById(mode === 'enroll' ? 'enroll-codes' : 'security-codes');
              var codesList = document.getElementById(mode === 'enroll' ? 'enroll-code-list' : 'security-code-list');
              if (form) form.hidden = true;
              if (codesList) showCodes(codesList, body.codes);
              if (codesRoot) codesRoot.hidden = false;
              panel.hidden = true;
              return;
            }
            location.assign(body.redirect || '/settings#security');
            return;
          }
          if (body.status === 'expired' || body.status === 'error') {
            stop(); reset();
            say(status, body.error || 'This QR code expired. Request a new one.');
            return;
          }
        } catch (err) { /* keep polling through a short network interruption */ }
        if (active) timer = window.setTimeout(poll, 1800);
      };

      cancel.addEventListener('click', function () {
        stop(); panel.hidden = true; reset(); say(status, '');
        fetch('/auth/factor/qr/cancel', { method: 'POST', credentials: 'same-origin' }).catch(function () {});
        var returnTarget = button || (box.getAttribute('data-qr-for') === 'factor' && document.getElementById('factor-go'));
        if (returnTarget) returnTarget.focus();
      });

      var start = async function () {
        if (starting || active) return;
        starting = true;
        stop(); reset(); panel.hidden = false;
        if (button) button.disabled = true;
        say(status, 'Starting a secure passkey handoff…');
        var fields = { for: box.getAttribute('data-qr-for') || 'factor' };
        var nameField = box.getAttribute('data-qr-name-from');
        if (nameField) {
          var input = document.getElementById(nameField);
          if (input && input.value.trim()) fields.name = input.value.trim();
        }
        try {
          var started = await postJSON('/auth/factor/qr', fields);
          if (!started.qr) throw new Error(started.error || 'begin');
          panel.hidden = false;
          img.src = started.qr; img.hidden = false;
          code.textContent = started.code || '';
          say(status, 'Waiting for your phone…');
          active = true; cancel.focus();
          timer = window.setTimeout(poll, 1200);
        } catch (err) {
          panel.hidden = false;
          say(status, messageFor(err.message));
        } finally {
          starting = false;
          if (button) button.disabled = false;
        }
      };
      box.__esStartQR = start;
      if (button && wire(button)) button.addEventListener('click', start);
    });

    // The factor page's primary action also opens the phone QR when
    // the browser cannot use the native hybrid transport.
    var factorGo = document.getElementById('factor-go');
    if (factorGo) {
      factorGo.__esStartQRIfNeeded = function () {
        nativeHybridPromise.then(function (nativeHybrid) {
          if (nativeHybrid) return;
          var box = document.querySelector('[data-webauthn-qr][data-qr-for="factor"]');
          if (box && box.__esStartQR) box.__esStartQR();
        });
      };
    }

    var device = document.querySelector('[data-webauthn-qr-device]');
    var deviceButton = device && device.querySelector('[data-webauthn-qr-device-start]');
    if (deviceButton && wire(deviceButton)) {
      var token = device.getAttribute('data-token') || '';
      var codeInput = document.getElementById('webauthn-qr-code');
      var deviceStatus = document.getElementById('webauthn-qr-device-status');
      deviceButton.addEventListener('click', async function () {
        var pairingCode = (codeInput.value || '').replace(/[\s-]+/g, '');
        if (!/^\d{6}$/.test(pairingCode)) {
          say(deviceStatus, 'Enter the six-digit code shown on your computer.');
          codeInput.focus();
          return;
        }
        deviceButton.disabled = true;
        say(deviceStatus, 'Follow the passkey prompt on this phone…');
        try {
          var operation;
          do {
            var optsResponse = await fetch('/auth/factor/qr/device/options', {
              method: 'POST', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json', 'X-Evesynapse-QR-Token': token },
              body: JSON.stringify({ code: pairingCode })
            });
            var opts = {};
            try { opts = await optsResponse.json(); } catch (err) { /* handled below */ }
            if (!optsResponse.ok) throw new Error(opts.error || 'expired');
            operation = opts.operation;
            var publicKey = operation === 'register'
              ? creationOptions(opts.publicKey)
              : requestOptions(opts.publicKey);
            var credential = operation === 'register'
              ? await navigator.credentials.create({ publicKey: publicKey })
              : await navigator.credentials.get({ publicKey: publicKey });
            if (!credential) throw new Error('verify');
            var verified = await fetch('/auth/factor/qr/device/verify', {
              method: 'POST', credentials: 'same-origin',
              headers: {
                'Content-Type': 'application/json',
                'X-Evesynapse-QR-Token': token,
                'X-Evesynapse-QR-Code': pairingCode
              },
            body: JSON.stringify(serialiseCredential(credential))
            });
            var result = {};
            try { result = await verified.json(); } catch (err) { /* handled below */ }
            if (!verified.ok) throw new Error(result.error || 'verify');
            if (result.next === 'register') {
              say(deviceStatus, 'Passkey confirmed. Now save a passkey on this phone…');
              continue;
            }
            say(deviceStatus, result.message || 'Done. Return to your computer to finish.');
            codeInput.disabled = true;
            return;
          } while (true);
        } catch (err) {
          say(deviceStatus, messageFor(err.name === 'NotAllowedError' ? err.name : err.message));
          if (err.name === 'NotAllowedError') deviceButton.disabled = false;
          else deviceButton.disabled = false;
        }
      });
    }
  }

  function init() {
    // --- factor page ------------------------------------------
    var factorGo = document.getElementById('factor-go');
    if (wire(factorGo)) {
      factorGo.addEventListener('click', function () {
        if (factorGo.__esStartQRIfNeeded) factorGo.__esStartQRIfNeeded();
        var root = document.getElementById('factor-assert');
        var status = document.getElementById('factor-status');
        hideError(status);
        if (!keysAvailable()) {
          showError(status, new Error('begin'));
          return;
        }
        factorGo.disabled = true;
        runAssertion(root.getAttribute('data-begin'), root.getAttribute('data-finish-url')).then(function (redirect) {
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
        runRegistration(root.getAttribute('data-begin'), root.getAttribute('data-finish-url'), name).then(function (data) {
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
        postJSON(root.getAttribute('data-confirm-url')).then(function (data) {
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
          return runRegistration(root.getAttribute('data-begin'), root.getAttribute('data-finish-url'), name);
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
    initWebAuthnQR();
  }

  init();
  document.addEventListener('evesynapse:content-ready', init);
})();
