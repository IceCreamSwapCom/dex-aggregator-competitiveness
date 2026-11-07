// Tooltips: any element with data-tip shows it on hover and focus.
(function () {
  var box = document.getElementById("tipbox");
  function show(e) {
    var t = e.target.closest && e.target.closest("[data-tip]");
    if (!t) return;
    box.textContent = t.getAttribute("data-tip");
    box.style.display = "block";
    var r = t.getBoundingClientRect(), w = box.offsetWidth, h = box.offsetHeight;
    var x = Math.min(Math.max(8, r.left + r.width / 2 - w / 2), window.innerWidth - w - 8);
    var y = r.top - h - 8 < 8 ? r.bottom + 8 : r.top - h - 8;
    box.style.left = x + "px"; box.style.top = y + "px";
  }
  function hide(e) { if (e.target.closest && e.target.closest("[data-tip]")) box.style.display = "none"; }
  document.addEventListener("mouseover", show); document.addEventListener("focusin", show);
  document.addEventListener("mouseout", hide); document.addEventListener("focusout", hide);
  window.addEventListener("scroll", function () { box.style.display = "none"; }, { passive: true });
})();
// Menus: a click outside an open one closes it.
document.addEventListener("click", function (e) {
  document.querySelectorAll("details.menu[open]").forEach(function (m) { if (!m.contains(e.target)) m.removeAttribute("open"); });
});
// Rows with data-href open it wherever they are clicked; the link in the row
// stays for keyboards. Ctrl, cmd or the middle button open a new tab.
function rowLink(e) {
  var tr = e.target.closest("tr[data-href]");
  if (!tr || e.target.closest("a, button, .tip") || (e.type == "auxclick" && e.button != 1) || String(getSelection())) return;
  if (e.ctrlKey || e.metaKey || e.button == 1) window.open(tr.dataset.href); else location.href = tr.dataset.href;
}
document.addEventListener("click", rowLink);
document.addEventListener("auxclick", rowLink);
// Sortable tables: a click on a th with data-sort orders the rows by their cells' data-v.
document.querySelectorAll("table[data-sortable]").forEach(function (table) {
  table.querySelectorAll("th[data-sort]").forEach(function (th) {
    th.addEventListener("click", function (e) {
      if (e.target.closest(".tip")) return;
      var i = Array.prototype.indexOf.call(th.parentNode.children, th);
      var asc = th.getAttribute("aria-sort") === "descending" ? true : th.getAttribute("aria-sort") === "ascending" ? false : th.dataset.sort === "asc";
      table.querySelectorAll("th[aria-sort]").forEach(function (o) { o.removeAttribute("aria-sort"); });
      th.setAttribute("aria-sort", asc ? "ascending" : "descending");
      var body = table.tBodies[0], rows = Array.prototype.slice.call(body.rows);
      rows.sort(function (a, b) {
        var x = parseFloat(a.cells[i].dataset.v), y = parseFloat(b.cells[i].dataset.v);
        if (isNaN(x)) return 1; if (isNaN(y)) return -1;
        return asc ? x - y : y - x;
      });
      rows.forEach(function (r) { body.appendChild(r); });
    });
  });
});
