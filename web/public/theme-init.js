// Runs before first paint (an external file because the CSP forbids inline scripts).
// Keep the storage key and colours in sync with src/theme.tsx.
(function () {
  var t = "dark";
  try {
    if (localStorage.getItem("vp-theme") === "light") t = "light";
  } catch (e) {}
  var d = document.documentElement;
  d.setAttribute("data-theme", t);
  d.style.colorScheme = t;
  var m = document.querySelector('meta[name="theme-color"]');
  if (m) m.setAttribute("content", t === "light" ? "#F8F8F8" : "#2C3942");
})();
