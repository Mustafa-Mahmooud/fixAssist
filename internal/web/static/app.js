// Copy buttons: <button data-copy="#element-id">Copy</button>
document.addEventListener("click", function (e) {
  var btn = e.target.closest("[data-copy]");
  if (!btn) return;
  var el = document.querySelector(btn.getAttribute("data-copy"));
  if (!el) return;
  navigator.clipboard.writeText(el.textContent.trim()).then(function () {
    var old = btn.textContent;
    btn.textContent = "Copied";
    setTimeout(function () { btn.textContent = old; }, 1500);
  });
});
