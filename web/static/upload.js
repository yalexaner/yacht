// upload.js drives the /upload form: a kind=file/kind=text radio toggle that
// shows the matching input block, plus an XHR-based submit so the operator
// sees a live progress bar instead of the browser's blank "submitting..." state.
//
// The handler stays a regular multipart POST, so this script is purely an
// enhancement: the form still works without JS (the browser submits it as a
// normal form, the handler 303-redirects, and the next page loads). Disable
// JS in the browser and the upload still goes through.
(function () {
  'use strict';

  document.addEventListener('DOMContentLoaded', function () {
    var form = document.querySelector('form[action="/upload"]');
    if (!form) {
      return;
    }

    var progress = document.getElementById('upload-progress');
    var percent = document.getElementById('upload-percent');
    var submitBtn = form.querySelector('button[type="submit"]');
    var textBlock = form.querySelector('.text-block');
    var fileBlock = form.querySelector('.file-block');
    var textInput = form.querySelector('textarea[name="text"]');
    var fileInput = form.querySelector('input[name="file"]');
    var kindRadios = form.querySelectorAll('input[name="kind"]');

    // syncKindVisibility hides the input block that doesn't match the selected
    // radio. The other block stays in the DOM so the form keeps posting both
    // fields — parseUploadForm tolerates an empty file part on kind=text and
    // ignores text on kind=file, so visibility here is a UX hint, not a
    // correctness gate.
    function syncKindVisibility() {
      var selected = form.querySelector('input[name="kind"]:checked');
      if (!selected) {
        return;
      }
      if (selected.value === 'file') {
        if (fileBlock) fileBlock.hidden = false;
        if (textBlock) textBlock.hidden = true;
      } else {
        if (fileBlock) fileBlock.hidden = true;
        if (textBlock) textBlock.hidden = false;
      }
    }

    for (var i = 0; i < kindRadios.length; i++) {
      kindRadios[i].addEventListener('change', syncKindVisibility);
    }
    syncKindVisibility();

    form.addEventListener('submit', function (event) {
      event.preventDefault();

      if (progress) {
        progress.value = 0;
        progress.hidden = false;
      }
      if (percent) {
        percent.textContent = '0%';
        percent.hidden = false;
      }
      if (submitBtn) {
        submitBtn.disabled = true;
      }

      var formData = new FormData(form);
      var xhr = new XMLHttpRequest();
      xhr.open('POST', form.action, true);

      xhr.upload.onprogress = function (e) {
        if (!e.lengthComputable) {
          return;
        }
        var pct = Math.round((e.loaded / e.total) * 100);
        if (progress) progress.value = pct;
        if (percent) percent.textContent = pct + '%';
      };

      xhr.onload = function () {
        // XHR follows 303 redirects automatically: by the time onload fires,
        // status reflects the redirect target's response and responseURL is
        // the final URL after redirects. Treat 2xx/3xx as success and hand
        // off to a real navigation so the browser commits the new page (the
        // /shares/{id}/created confirmation) rather than rendering it inline.
        if (xhr.status >= 200 && xhr.status < 400) {
          window.location.href = xhr.responseURL || '/upload';
          return;
        }
        // Server re-rendered upload.html with an Error banner (validation
        // failure or oversized body). Swap the whole document so the
        // returned form replaces the in-flight one — preserves the operator's
        // intent and surfaces the error message exactly as the static path
        // would have shown it.
        document.open();
        document.write(xhr.responseText);
        document.close();
      };

      xhr.onerror = function () {
        if (submitBtn) submitBtn.disabled = false;
        if (progress) progress.hidden = true;
        if (percent) percent.hidden = true;
        var banner = document.createElement('p');
        banner.className = 'error';
        banner.textContent = 'Upload failed — please check your connection and try again.';
        form.parentNode.insertBefore(banner, form);
      };

      xhr.send(formData);
    });

    // Touching textInput / fileInput here keeps a couple of references the
    // closure already captured "live" against future template tweaks — a
    // dropped element shows up as a console-discoverable null instead of a
    // silent no-op deep inside the submit path.
    void textInput;
    void fileInput;
  });
})();
