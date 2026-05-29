// Theme toggle - persists choice to localStorage, defaults to system preference
(function() {
  const saved = localStorage.getItem('theme');
  if (saved) {
    document.documentElement.className = saved;
  }
})();

function toggleTheme() {
  const html = document.documentElement;
  const current = html.classList.contains('light') ? 'light' : 'dark';
  const next = current === 'dark' ? 'light' : 'dark';
  html.className = next;
  localStorage.setItem('theme', next);
  // Update toggle button icon
  const btn = document.querySelector('.theme-toggle');
  if (btn) btn.textContent = next === 'dark' ? '☀️' : '🌙';
}
