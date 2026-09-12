(function () {
  "use strict";

  function banner() {
    return document.getElementById("error-banner");
  }

  function show(message) {
    var el = banner();
    if (!el) return;
    el.textContent = message;
    el.hidden = false;
  }

  function hide() {
    var el = banner();
    if (el) el.hidden = true;
  }

  document.addEventListener("htmx:beforeRequest", hide);

  document.addEventListener("htmx:responseError", function (evt) {
    var xhr = evt.detail.xhr;
    var message = "Request failed (" + xhr.status + ").";
    var contentType = xhr.getResponseHeader("Content-Type") || "";
    try {
      if (contentType.indexOf("text/plain") === 0 && xhr.responseText) {
        message = xhr.responseText.replace(/^validation:\s*/i, "");
      } else if (contentType.indexOf("text/html") === 0 && xhr.responseText) {
        var doc = new DOMParser().parseFromString(xhr.responseText, "text/html");
        var detail = doc.querySelector(".error-card p");
        if (detail && detail.textContent) {
          message = detail.textContent;
        }
      }
    } catch (err) {
      /* keep the generic message */
    }
    show(message);
  });

  document.addEventListener("htmx:sendError", function () {
    show("Network error: the server did not respond.");
  });
})();
