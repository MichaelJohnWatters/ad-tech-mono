// portal-core.js — helpers shared by the three portals (advertiser /
// publisher / staff). One copy: a fix here (esc() above all) lands in every
// portal at once — the escaping helper previously existed as three template
// copies, so an XSS fix had to be applied three times to actually ship.
//
// Load order: include BEFORE the portal's inline <script> (these are plain
// globals, same as toast.js / theme.js).

const $ = (id) => document.getElementById(id);

// esc — THE HTML-escaping helper every innerHTML template string must route
// user-controlled values through.
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));

const fmtUSD = (n) => '$' + Number(n || 0).toLocaleString(undefined, {minimumFractionDigits: 2, maximumFractionDigits: 2});
const fmtDate = (s) => { try { return new Date(s).toLocaleDateString(); } catch { return s; } };
const todayStart = () => { const d = new Date(); d.setHours(0, 0, 0, 0); return d.toISOString(); };
const daysAgo = (n) => { const d = new Date(); d.setDate(d.getDate() - n); return d.toISOString(); };

async function fetchJSON(url, opts = {}) {
  const resp = await fetch(url, {headers: {'Content-Type': 'application/json'}, ...opts});
  const text = await resp.text();
  if (!resp.ok) {
    let msg = text;
    try { msg = JSON.parse(text).error || text; } catch {}
    throw new Error(`${resp.status}: ${msg}`);
  }
  return text ? JSON.parse(text) : null;
}

// ---- Section switching (hash-driven; sidebar links are #anchors) ----
// initSectionRouter returns the portal's showSection(name). Sections load
// lazily on first show via cfg.loaders; cfg.reEnter runs on every re-entry
// (e.g. restarting a jobs poll). cfg.fallback picks the landing section for
// an unknown/empty hash (default: first section). cfg.crumbs overrides the
// default Capitalised crumb per section; cfg.onShow is a per-portal hook
// (e.g. toggling a section-local header action).
function initSectionRouter(cfg) {
  const loaded = {};
  function showSection(name) {
    if (!cfg.sections.includes(name)) name = cfg.fallback ? cfg.fallback() : cfg.sections[0];
    cfg.sections.forEach(s => $('section-' + s)?.classList.toggle('hidden', s !== name));
    cfg.onShow?.(name);
    $('crumbSection').textContent = (cfg.crumbs || {})[name] || (name[0].toUpperCase() + name.slice(1));
    document.querySelectorAll('aside nav a').forEach(a => {
      const active = a.getAttribute('href') === '#' + name;
      a.classList.toggle('bg-brand/10', active);
      a.classList.toggle('text-brand', active);
      a.classList.toggle('font-medium', active);
      a.classList.toggle('text-gray-600', !active);
      a.classList.toggle('dark:text-gray-300', !active);
    });
    if (!loaded[name]) {
      loaded[name] = true;
      cfg.loaders[name]?.();
    } else {
      cfg.reEnter?.[name]?.();
    }
  }
  // Forget which sections loaded, so the next showSection re-pulls (used by
  // the publisher's site switcher to refresh whatever is on screen).
  showSection.resetLoaded = () => Object.keys(loaded).forEach(k => delete loaded[k]);
  window.addEventListener('hashchange', () => showSection(location.hash.slice(1)));
  return showSection;
}

// ---- Notifications (bell + dropdown) ----
// Poll the unread count every 30s for the badge; the full list loads when the
// panel opens. Clicking a notification marks it read; "Mark all read" clears
// the badge. All reads/mutations are tenant-scoped server-side. Portals with
// a bell call initNotifBell() once at the end of their inline script.
let notifUnread = 0; // last polled unread count — the needs-attention card reads it
function renderNotifs(list) {
  const el = $('notifList');
  if (!el) return;
  if (!list.length) {
    el.innerHTML = '<div class="px-4 py-6 text-center text-sm text-gray-400">No notifications</div>';
    return;
  }
  el.innerHTML = list.map(n => {
    const when = new Date(n.created_at).toLocaleString();
    const dot = n.read ? '' : '<span class="inline-block w-2 h-2 rounded-full bg-brand shrink-0 mt-1.5"></span>';
    const wt = n.read ? 'font-normal' : 'font-medium';
    return `<div class="flex gap-2 px-4 py-3 cursor-pointer hover:bg-gray-50 dark:hover:bg-surface-3" onclick="markNotifRead('${esc(n.id)}')">
      ${dot || '<span class="w-2 shrink-0"></span>'}
      <div class="min-w-0">
        <div class="text-sm text-gray-900 dark:text-gray-100 ${wt}">${esc(n.title)}</div>
        ${n.body ? `<div class="text-xs text-gray-500 dark:text-gray-400 mt-0.5">${esc(n.body)}</div>` : ''}
        <div class="text-[11px] text-gray-400 mt-1">${esc(when)}</div>
      </div>
    </div>`;
  }).join('');
}
function setNotifBadge(n) {
  notifUnread = n;
  const b = $('notifBadge');
  if (!b) return;
  if (n > 0) { b.textContent = n > 99 ? '99+' : n; b.classList.remove('hidden'); }
  else b.classList.add('hidden');
}
async function loadNotifs() {
  try {
    const res = await fetchJSON('/v1/api/notifications');
    renderNotifs(res?.notifications || []);
    setNotifBadge(res?.unread || 0);
  } catch { /* transient — the 30s poll retries */ }
}
async function refreshNotifBadge() {
  try {
    const res = await fetchJSON('/v1/api/notifications');
    setNotifBadge(res?.unread || 0);
  } catch { /* transient */ }
}
function toggleNotifs() {
  const p = $('notifPanel');
  if (!p) return;
  const open = !p.classList.contains('hidden');
  p.classList.toggle('hidden', open);
  if (!open) loadNotifs();
}
async function markNotifRead(id) {
  try {
    await fetchJSON('/v1/api/notifications/read', {method: 'POST', body: JSON.stringify({id})});
    await loadNotifs();
  } catch (err) { toast('Could not mark read: ' + err.message, 'error'); }
}
async function markAllNotifsRead() {
  try {
    await fetchJSON('/v1/api/notifications/read', {method: 'POST', body: JSON.stringify({all: true})});
    await loadNotifs();
  } catch (err) { toast('Could not mark all read: ' + err.message, 'error'); }
}
function initNotifBell() {
  // Close the panel when clicking outside it.
  document.addEventListener('click', (e) => {
    const wrap = $('notifBell')?.parentElement;
    if (wrap && !wrap.contains(e.target)) $('notifPanel')?.classList.add('hidden');
  });
  refreshNotifBadge();
  setInterval(refreshNotifBadge, 30000);
}
