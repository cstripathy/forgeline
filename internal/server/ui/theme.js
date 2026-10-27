// Runs before the stylesheet paints, so the page never flashes the wrong theme.
(function () {
  var saved = null;
  try { saved = localStorage.getItem('forgeline-theme'); } catch (e) { /* storage may be blocked */ }
  var dark = saved ? saved === 'dark' : window.matchMedia('(prefers-color-scheme: dark)').matches;
  document.documentElement.classList.toggle('dark', dark);
})();
