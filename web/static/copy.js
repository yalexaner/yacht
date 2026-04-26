// copy.js drives the "Copy" button on /shares/{id}/created: one click puts
// the share URL on the user's clipboard and flips the button label to the
// localized success string for 1.5s as feedback. Pure progressive
// enhancement — without JS, the URL still sits in a readonly <input> the
// operator can select and copy by hand.
//
// Localized button labels arrive from the template via data-* attributes
// (data-copy-success-text, data-copy-failed-text) so this script stays
// language-agnostic. If either attribute is missing we leave the button
// label unchanged rather than introduce an English literal — the clipboard
// op still ran, and templates always set both attributes in practice.
//
// We try navigator.clipboard.writeText first (the modern, async API). It
// can reject for two practical reasons: the page is served over plain HTTP
// outside localhost (Clipboard API requires a secure context) or the user
// previously denied clipboard-write permission. In either case we fall
// back to the legacy execCommand('copy') path: stash the text in an
// off-screen textarea, select it, ask the document to copy the selection,
// then yank the textarea back out. execCommand is deprecated but still
// implemented in every browser we care about and works in non-secure
// contexts where the modern API doesn't.
(function () {
  'use strict';

  document.addEventListener('DOMContentLoaded', function () {
    var buttons = document.querySelectorAll('[data-copy-text]');
    for (var i = 0; i < buttons.length; i++) {
      bind(buttons[i]);
    }
  });

  function bind(btn) {
    btn.addEventListener('click', function () {
      var text = btn.dataset.copyText || '';
      if (!text) {
        return;
      }
      copy(text).then(function () {
        flash(btn);
      }, function () {
        // Both paths failed (clipboard rejected AND execCommand returned
        // false). Surface the failure so the operator knows to copy by
        // hand instead of silently swallowing it — the readonly input
        // next to the button still holds the URL.
        var prev = btn.textContent;
        btn.textContent = btn.dataset.copyFailedText || prev;
        setTimeout(function () { btn.textContent = prev; }, 1500);
      });
    });
  }

  // copy returns a Promise so the caller can branch on success/failure
  // uniformly across the modern and legacy paths.
  function copy(text) {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
      return navigator.clipboard.writeText(text).catch(function () {
        return execCopy(text);
      });
    }
    return execCopy(text);
  }

  function execCopy(text) {
    return new Promise(function (resolve, reject) {
      var ta = document.createElement('textarea');
      ta.value = text;
      // Off-screen but still focusable: position fixed at (-9999px, 0) so
      // the page doesn't scroll and the textarea isn't visible. readonly
      // keeps mobile keyboards from popping up during the brief select().
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.top = '0';
      ta.style.left = '-9999px';
      document.body.appendChild(ta);
      ta.select();
      var ok = false;
      try {
        ok = document.execCommand('copy');
      } catch (e) {
        ok = false;
      }
      document.body.removeChild(ta);
      if (ok) {
        resolve();
      } else {
        reject(new Error('execCommand copy failed'));
      }
    });
  }

  function flash(btn) {
    var prev = btn.textContent;
    btn.textContent = btn.dataset.copySuccessText || prev;
    btn.classList.add('copied');
    setTimeout(function () {
      btn.textContent = prev;
      btn.classList.remove('copied');
    }, 1500);
  }
})();
