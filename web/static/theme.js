// Theme toggle — single source for every page. Convention: any element
// with class `.theme-toggle` (or legacy ids `#themeIcon` / `#ti` that
// older pages haven't migrated yet) is treated as the toggle button
// and gets its icon flipped on each toggle.
//
// FOUC prevention happens inline in the head-meta partial; this file
// only handles the click-driven flip. Keep it idempotent so calling
// toggleTheme() from any nav button works the same way.
function toggleTheme() {
    var html = document.documentElement;
    var next = html.classList.contains('dark') ? 'light' : 'dark';
    html.className = next;
    localStorage.setItem('theme', next);
    var icon = next === 'dark' ? '☀️' : '🌙';
    var sel = '.theme-toggle, #themeIcon, #ti';
    document.querySelectorAll(sel).forEach(function(el) {
        // Only update inner text on plain buttons; if the button has
        // child SVGs (future redesign) leave them alone.
        if (!el.querySelector('svg')) el.textContent = icon;
    });
}
