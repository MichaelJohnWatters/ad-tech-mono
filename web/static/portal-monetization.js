// portal-monetization.js — segment data-monetization console shared by the
// advertiser and publisher portals (ADR 0009): per-segment data-fee earnings,
// the fee editor (toast + undo), and the IAB Audience Taxonomy picker. Was a
// byte-identical copy in both templates; the segment table itself stays
// portal-side (it renders into each portal's Audiences section and calls
// openTaxonomyPicker/loadDataEarnings from here).
// ---- Data earnings (ADR 0009) ----
const fmtMicros = (m) => '$' + (m / 1e6).toFixed(4);
async function loadDataEarnings() {
  const tbody = document.getElementById('dataEarnings');
  if (!tbody) return;
  try {
    const rows = await fetchJSON('/v1/api/audiences/earnings') || [];
    tbody.innerHTML = rows.length ? rows.map(r => `
      <tr class="border-t border-gray-100 dark:border-gray-800">
        <td class="px-3 py-2 font-medium">${esc(r.segment_name)}</td>
        <td class="px-3 py-2 font-mono">${fmtInt(r.impressions)}</td>
        <td class="px-3 py-2 font-mono text-xs">${fmtMicros(r.fee_micros)}</td>
        <td class="px-3 py-2 font-mono text-xs text-gray-500">${fmtMicros(r.margin_micros)}</td>
        <td class="px-3 py-2 font-mono text-xs text-success">${fmtMicros(r.owner_net_micros)}</td>
        <td class="px-3 py-2 text-xs text-gray-500">${fmtDate(r.last_earned_at)}</td>
      </tr>`).join('')
      : '<tr><td colspan="6" class="px-3 py-8 text-center text-sm text-gray-400">Nothing earned yet — set a fee on a public, IAB-labelled segment and earnings accrue when external buyers win on it.</td></tr>';
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="6" class="px-3 py-6 text-center text-sm text-error">load failed — ${esc(err.message)}</td></tr>`;
  }
}

// ---- Data-fee editor ----
let feeTarget = null; // { segmentId, prevMicros } — prevMicros powers toast-undo
function openFeeEditor(segmentId, currentMicros) {
  feeTarget = { segmentId, prevMicros: currentMicros ?? null };
  $('fee_input').value = currentMicros != null ? (currentMicros / 1e6).toFixed(2) : '';
  $('feeEditor').classList.remove('hidden');
  $('fee_input').focus();
}
async function saveFee(clear) {
  const t = feeTarget;
  if (!t) return;
  let micros = null;
  if (!clear) {
    const dollars = parseFloat($('fee_input').value);
    if (isNaN(dollars) || dollars < 0) { toast('Enter a fee of $0.00 or more', 'error'); return; }
    micros = Math.round(dollars * 1e6);
  }
  $('feeEditor').classList.add('hidden');
  try {
    await putFee(t.segmentId, micros);
    loadAudiences();
    toastUndo(clear ? 'Data fee cleared' : 'Data fee updated', async () => {
      try { await putFee(t.segmentId, t.prevMicros); loadAudiences(); }
      catch (err) { toast('Undo failed: ' + err.message, 'error'); }
    });
  } catch (err) { toast('Fee update failed: ' + err.message, 'error'); }
}
async function putFee(segmentId, micros) {
  const resp = await fetch('/v1/api/audiences/fee', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ segment_id: segmentId, data_fee_micros: micros }),
  });
  if (!resp.ok) {
    // Surface the server's reason (e.g. the 422 "must be public and
    // taxonomy-labelled" precondition) instead of a bare status code.
    let msg = 'HTTP ' + resp.status;
    try { const body = await resp.json(); if (body && body.error) msg = body.error; } catch {}
    throw new Error(msg);
  }
}

// ---- IAB Audience Taxonomy picker ----
// Labelling a segment with a standard taxonomy node is what makes it
// expressible OUTSIDE the platform: public labelled segments ride bid
// requests to external buyers as OpenRTB user.data (ext.segtax=4). The
// reference list is global and small, so it loads once and filters
// client-side — fuzzy by default, "quoted" for strict substring.
let taxNodes = null;   // global reference list, fetched once per page life
let taxTarget = null;  // { segmentId, prevId } — prevId powers toast-undo
async function openTaxonomyPicker(segmentId, currentId) {
  taxTarget = { segmentId, prevId: currentId ?? null };
  if (!taxNodes) {
    try { taxNodes = await fetchJSON('/v1/api/taxonomy') || []; }
    catch (err) { toast('Taxonomy load failed: ' + err.message, 'error'); return; }
  }
  $('tax_search').value = '';
  renderTaxonomyList();
  $('taxonomyPicker').classList.remove('hidden');
  $('tax_search').focus();
}
function taxFuzzyScore(q, s) {
  if (!q) return 0;
  s = s.toLowerCase();
  if (q.startsWith('"')) return s.includes(q.replaceAll('"', '')) ? 0 : -1; // strict mode
  let qi = 0, score = 0, streak = 0;
  for (let i = 0; i < s.length && qi < q.length; i++) {
    if (s[i] === q[qi]) { qi++; streak++; score += streak; }
    else streak = 0;
  }
  return qi === q.length ? score : -1;
}
function renderTaxonomyList() {
  const q = $('tax_search').value.trim().toLowerCase();
  const rows = taxNodes
    .map(n => ({ n, score: taxFuzzyScore(q, n.path) }))
    .filter(r => r.score >= 0)
    .sort((a, b) => b.score - a.score)
    .slice(0, 60);
  $('tax_list').innerHTML = rows.length ? rows.map(r => `
    <button type="button" onclick="setTaxonomy(${r.n.id})" class="block w-full text-left px-3 py-2 text-sm hover:bg-gray-100 dark:hover:bg-surface-3 ${taxTarget && taxTarget.prevId === r.n.id ? 'bg-info/10' : ''}">
      ${esc(r.n.path)} <span class="text-[10px] text-gray-400 font-mono">#${r.n.id}</span>
    </button>`).join('')
    : '<div class="px-3 py-6 text-center text-sm text-gray-400">no matches</div>';
}
async function setTaxonomy(taxonomyId) { // null clears the label
  const t = taxTarget;
  if (!t) return;
  $('taxonomyPicker').classList.add('hidden');
  try {
    await putTaxonomy(t.segmentId, taxonomyId);
    loadAudiences();
    toastUndo(taxonomyId == null ? 'Taxonomy label cleared' : 'Taxonomy label updated', async () => {
      try { await putTaxonomy(t.segmentId, t.prevId); loadAudiences(); }
      catch (err) { toast('Undo failed: ' + err.message, 'error'); }
    });
  } catch (err) { toast('Label update failed: ' + err.message, 'error'); }
}
async function putTaxonomy(segmentId, taxonomyId) {
  const resp = await fetch('/v1/api/audiences/taxonomy', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ segment_id: segmentId, taxonomy_id: taxonomyId }),
  });
  if (!resp.ok) throw new Error('HTTP ' + resp.status);
}
