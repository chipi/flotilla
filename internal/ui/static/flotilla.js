// flotilla UI: the few things a page does in the browser. Every page works without this file; it adds
// the Decision box's five-second hold with Undo and Send now, the copy buttons, confirm buttons that
// wait for a reason, and selects that apply themselves.
(function () {
  "use strict";

  // An answer waits five seconds, so a thumb slip can be undone (DESIGN.md, the Decision Box).
  document.querySelectorAll("form[data-hold]").forEach(function (form) {
    var box = form.closest("[data-decision]");
    var pending = box && box.querySelector("[data-pending]");
    if (!pending) return;
    var text = pending.querySelector("[data-pending-text]");
    var timer = null, left = 0;
    function stop() { clearInterval(timer); timer = null; }
    function send() { stop(); form.submit(); }
    function tick() {
      text.textContent = form.dataset.hold + " in " + left + " s.";
      if (left <= 0) { send(); return; }
      left -= 1;
    }
    form.addEventListener("submit", function (e) {
      if (!form.checkValidity()) return;
      e.preventDefault();
      if (timer) return;
      left = 5;
      pending.hidden = false;
      tick();
      timer = setInterval(tick, 1000);
      pending.querySelector("[data-undo]").onclick = function () { stop(); pending.hidden = true; };
      pending.querySelector("[data-send]").onclick = send;
    });
  });

  // Copy buttons are hidden without JavaScript; with it they copy their value.
  document.querySelectorAll("button[data-copy]").forEach(function (b) {
    if (!navigator.clipboard) return;
    b.hidden = false;
    var label = b.textContent;
    b.addEventListener("click", function () {
      navigator.clipboard.writeText(b.dataset.copy).then(function () {
        b.textContent = "Copied";
        setTimeout(function () { b.textContent = label; }, 2000);
      });
    });
  });

  // A confirm that needs a reason stays disabled until the reason has text.
  document.querySelectorAll("[data-needs]").forEach(function (field) {
    var form = field.form;
    if (!form) return;
    var submit = form.querySelector("button[type=submit]");
    if (!submit) return;
    function sync() { submit.disabled = field.value.trim() === ""; }
    field.addEventListener("input", sync);
    sync();
  });

  // Cancel closes the details a confirm sits in.
  document.querySelectorAll("button[data-close]").forEach(function (b) {
    var d = b.closest("details");
    if (!d) return;
    b.hidden = false;
    b.addEventListener("click", function () { d.open = false; });
  });

  document.querySelectorAll("select[data-autosubmit]").forEach(function (s) {
    s.addEventListener("change", function () { s.form.submit(); });
  });
})();
