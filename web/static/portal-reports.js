// portal-reports.js — the saved-query + report-jobs console shared by the
// advertiser and publisher portals. Requires portal-core.js. Portal-specific
// pieces stay in the templates: reportQuery (scoping filters), runAsJob /
// jobFromSaved (publisher injects publisher_id; advertiser adds a delivery
// picker), loadRecentImpressions (different columns).

const savedTbody = () => document.querySelector('#savedWrap tbody');
let savedCache = [];

async function loadSaved() {
  try {
    savedCache = await fetchJSON('/v1/api/reports/saved') || [];
  } catch (err) {
    savedTbody().innerHTML = `<tr><td colspan="5" class="px-3 py-6 text-center text-sm text-error">load failed — ${esc(err.message)}</td></tr>`;
    return;
  }
  savedTbody().innerHTML = savedCache.length ? savedCache.map(s => {
    const q = s.query_config || {};
    const desc = `${q.table || '?'} · ${(q.metrics || []).join(',')}${q.dimensions?.length ? ' by ' + q.dimensions.join(',') : ''}`;
    return `<tr class="border-t border-gray-100 dark:border-gray-800">
      <td class="px-3 py-2 font-medium">${esc(s.name)}</td>
      <td class="px-3 py-2 font-mono text-xs text-gray-500">${esc(desc)}</td>
      <td class="px-3 py-2 font-mono text-xs">${s.schedule ? esc(s.schedule) : '<span class="text-gray-400">manual</span>'}</td>
      <td class="px-3 py-2 text-xs">${s.delivery && s.delivery !== 'none' ? esc(s.delivery) : '<span class="text-gray-400">—</span>'}</td>
      <td class="px-3 py-2 text-right whitespace-nowrap">
        <button onclick="runSaved('${esc(s.id)}')" class="text-xs px-2 py-1 rounded border border-gray-200 dark:border-gray-700 hover:bg-gray-100 dark:hover:bg-gray-800">Run</button>
        <button onclick="jobFromSaved('${esc(s.id)}')" title="Run async and download the result" class="text-xs px-2 py-1 rounded border border-gray-200 dark:border-gray-700 hover:bg-gray-100 dark:hover:bg-gray-800">Run as job</button>
        <button onclick="deleteSaved('${esc(s.id)}')" class="text-xs px-2 py-1 rounded border border-gray-200 dark:border-gray-700 hover:bg-gray-100 dark:hover:bg-gray-800">Delete</button>
      </td>
    </tr>`;
  }).join('')
    : '<tr><td colspan="5" class="px-3 py-8 text-center text-sm text-gray-400">Nothing saved yet — run a query and save it.</td></tr>';
}

// The free-text cron input only shows for "Custom cron…".
function toggleCronInput() {
  $('svCronWrap').classList.toggle('hidden', $('svSchedule').value !== 'custom');
}

function currentQueryConfig() {
  const split = (v) => v.split(',').map(s => s.trim()).filter(Boolean);
  return {
    table: $('repTable').value,
    metrics: split($('repMetrics').value),
    dimensions: split($('repDims').value),
    range_days: parseInt($('repRange').value, 10),
  };
}

async function saveQuery(ev) {
  ev.preventDefault();
  // Schedule: interval keyword, a 5-field cron for "custom", or "" for manual —
  // the API validates the expression (reportrunner.ValidSchedule) on save. An
  // empty cron under "custom" would silently save as manual, so guard it here.
  let schedule = $('svSchedule').value;
  if (schedule === 'custom') {
    schedule = $('svCron').value.trim();
    if (!schedule) { toast('Enter a 5-field cron expression', 'error'); return; }
  }
  const body = {
    name: $('svName').value.trim(),
    query_config: currentQueryConfig(),
    schedule,
    delivery: $('svDelivery').value,
  };
  try {
    await fetchJSON('/v1/api/reports/saved', {method: 'POST', body: JSON.stringify(body)});
    toast(schedule ? 'Query saved — runs ' + schedule : 'Query saved', 'success');
    $('svName').value = '';
    loadSaved();
  } catch (err) { toast('Save failed: ' + err.message, 'error'); }
}

function runSaved(id) {
  const s = savedCache.find(x => x.id === id);
  if (!s) return;
  const q = s.query_config || {};
  $('repTable').value = q.table || 'impressions';
  $('repMetrics').value = (q.metrics || []).join(',');
  $('repDims').value = (q.dimensions || []).join(',');
  if (q.range_days) $('repRange').value = String(q.range_days);
  runReport(new Event('submit'));
}

async function deleteSaved(id) {
  try {
    await fetchJSON('/v1/api/reports/saved?id=' + encodeURIComponent(id), {method: 'DELETE'});
    toast('Saved query deleted — save it again to undo', 'success');
    loadSaved();
  } catch (err) { toast('Delete failed: ' + err.message, 'error'); }
}

// ---- Report jobs (async) ----
const jobsTbody = () => document.querySelector('#jobsWrap tbody');
let jobsTimer = null;

function toggleColdHint() {
  $('coldHint')?.classList.toggle('hidden', parseInt($('repRange').value, 10) <= 7);
}

function jobStatusBadge(j) {
  const cls = {queued: 'bg-gray-200/60 text-gray-600 dark:bg-gray-700 dark:text-gray-300',
               running: 'bg-brand/15 text-brand', done: 'bg-success/15 text-success',
               failed: 'bg-error/15 text-error'}[j.status] || 'bg-gray-100 text-gray-500';
  const title = j.status === 'failed' ? ` title="${esc(j.error || '')}"` : '';
  return `<span${title} class="text-xs px-1.5 py-0.5 rounded ${cls}">${esc(j.status)}</span>`;
}

async function loadJobs() {
  clearTimeout(jobsTimer);
  let jobs = [];
  try {
    jobs = await fetchJSON('/v1/api/reports/jobs') || [];
  } catch (err) {
    jobsTbody().innerHTML = `<tr><td colspan="7" class="px-3 py-6 text-center text-sm text-error">load failed — ${esc(err.message)}</td></tr>`;
    return;
  }
  jobsTbody().innerHTML = jobs.length ? jobs.map(j => `
    <tr class="border-t border-gray-100 dark:border-gray-800">
      <td class="px-3 py-2 font-medium">${esc(j.name)}</td>
      <td class="px-3 py-2">${jobStatusBadge(j)}</td>
      <td class="px-3 py-2 font-mono text-xs">${esc(j.format)}</td>
      <td class="px-3 py-2 font-mono text-xs">${esc((j.created_at || '').slice(0, 19))}</td>
      <td class="px-3 py-2 font-mono text-xs text-right">${j.status === 'done' ? (j.row_count ?? 0) : '—'}</td>
      <td class="px-3 py-2 text-right whitespace-nowrap">${j.status === 'done'
        ? `<a href="/v1/api/reports/jobs/${esc(j.id)}/download" class="text-xs px-2 py-1 rounded border border-gray-200 dark:border-gray-700 hover:bg-gray-100 dark:hover:bg-gray-800">Download</a>`
        : ''}</td>
    </tr>`).join('')
    : '<tr><td colspan="6" class="px-3 py-8 text-center text-sm text-gray-400">No jobs yet — use “Run as job” above.</td></tr>';
  // Poll while anything is in flight and the section is visible.
  if (jobs.some(j => j.status === 'queued' || j.status === 'running')
      && !$('section-reports').classList.contains('hidden')) {
    jobsTimer = setTimeout(loadJobs, 3000);
  }
}

async function submitReportJob(body, label) {
  try {
    await fetchJSON('/v1/api/reports/jobs', {method: 'POST', body: JSON.stringify(body)});
    toast(`Job queued — ${label}`, 'success');
    loadJobs();
  } catch (err) { toast('Job failed: ' + err.message, 'error'); }
}

// ---- Reports console (sync run into the results pane) ----
async function runReport(ev) {
  ev.preventDefault();
  const split = (v) => v.split(',').map(s => s.trim()).filter(Boolean);
  const body = {
    table: $('repTable').value,
    metrics: split($('repMetrics').value),
    dimensions: split($('repDims').value),
    time_from: daysAgo(parseInt($('repRange').value, 10)),
  };
  const out = $('reportOut');
  try {
    const res = await reportQuery(body);
    if (!res.rows?.length) {
      out.innerHTML = '<div class="py-6 text-center text-sm text-gray-400">Query ran — zero rows in range.</div>';
      return;
    }
    out.innerHTML = `<table class="w-full text-sm">
      <thead><tr>${res.columns.map(c => `<th class="px-3 py-2 text-left text-xs font-medium text-gray-500 uppercase tracking-wide">${esc(c)}</th>`).join('')}</tr></thead>
      <tbody>${res.rows.map(r => `<tr class="border-t border-gray-100 dark:border-gray-800">${
        r.map(v => `<td class="px-3 py-2 font-mono text-xs">${typeof v === 'number' && !Number.isInteger(v) ? v.toFixed(4) : esc(v)}</td>`).join('')
      }</tr>`).join('')}</tbody></table>`;
  } catch (err) { toast('Query failed: ' + err.message, 'error'); }
}
