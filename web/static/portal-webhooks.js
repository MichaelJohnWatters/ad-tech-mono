// portal-webhooks.js — the webhooks console shared by the advertiser and
// publisher portals: subscription list, create modal (event picker +
// one-time secret reveal), delete. Requires portal-core.js.
//
// WEBHOOK_EVENTS is the customer-facing event catalog — the ONE copy the
// portals render (it lived in both templates before). The dispatcher's
// routing table (cmd/webhooks eventRoutes) is the Go-side truth;
// cmd/webhooks TestPortalCatalogMatchesEventRoutes pins the two together so
// adding an event to one without the other fails the build.
const WEBHOOK_EVENTS = [
  {ev: 'budget.depleted', label: 'Campaign budget depleted', desc: 'a campaign exhausts its budget', checked: true},
  {ev: 'balance.depleted', label: 'Account balance depleted', desc: 'your prepay balance hits zero', checked: true},
  {ev: 'campaign.state_changed', label: 'Campaign state changed', desc: 'a campaign is paused / resumed / archived'},
  {ev: 'report.completed', label: 'Report completed', desc: 'a scheduled or async report finishes'},
  {ev: 'retargeting.enrolled', label: 'Retargeting enrollment', desc: 'a shopper enrols in a retargeting audience'},
];

async function loadWebhooks() {
  const tbody = $('webhookList');
  try {
    const hooks = await fetchJSON('/v1/api/webhooks') || [];
    tbody.innerHTML = hooks.length ? hooks.map(h => `
      <tr class="border-t border-gray-100 dark:border-gray-800">
        <td class="px-3 py-2 font-mono text-xs max-w-[20rem] truncate" title="${esc(h.url)}">${esc(h.url)}</td>
        <td class="px-3 py-2">${(h.events || []).map(e => `<span class="inline-block mr-1 mb-0.5 text-xs px-1.5 py-0.5 rounded bg-surface-3 text-gray-500 font-mono">${esc(e)}</span>`).join('')}</td>
        <td class="px-3 py-2"><span class="text-xs px-1.5 py-0.5 rounded ${h.status === 'active' ? 'bg-success/15 text-success' : 'bg-gray-200 dark:bg-gray-800 text-gray-500'}">${esc(h.status)}</span></td>
        <td class="px-3 py-2 text-xs text-gray-500">${fmtDate(h.created_at)}</td>
        <td class="px-3 py-2 text-right"><button onclick="deleteWebhook('${esc(h.id)}')" class="text-xs px-2 py-1 rounded border border-error/40 text-error hover:bg-error/10">Delete</button></td>
      </tr>`).join('')
      : '<tr><td colspan="5" class="px-3 py-8 text-center text-sm text-gray-400">No webhooks yet — add an endpoint to receive platform events.</td></tr>';
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="5" class="px-3 py-6 text-center text-sm text-error">load failed — ${esc(err.message)}</td></tr>`;
  }
}
function openNewWebhook() {
  $('webhookForm').reset();
  $('wh_events').innerHTML = WEBHOOK_EVENTS.map(e => `
    <label class="flex items-center gap-2">
      <input type="checkbox" class="wh_ev" value="${esc(e.ev)}" ${e.checked ? 'checked' : ''}>
      <span>${esc(e.ev)} <span class="text-xs text-gray-400">— ${esc(e.desc)}</span></span>
    </label>`).join('');
  $('webhookForm').classList.remove('hidden');
  $('webhookResult').classList.add('hidden');
  $('newWebhook').classList.remove('hidden');
}
function closeNewWebhook() { $('newWebhook').classList.add('hidden'); }
function copyWebhookSecret() {
  navigator.clipboard?.writeText($('wh_secret').textContent).then(() => toast('Secret copied', 'info'));
}
async function createWebhook(ev) {
  ev.preventDefault();
  const events = [...document.querySelectorAll('#webhookForm .wh_ev:checked')].map(b => b.value);
  if (!events.length) { toast('Pick at least one event', 'error'); return; }
  try {
    const res = await fetchJSON('/v1/api/webhooks', {method: 'POST',
      body: JSON.stringify({url: $('wh_url').value.trim(), events})});
    // Reveal the one-time secret in place (toasts fade; this must persist).
    $('wh_secret').textContent = res.secret;
    $('webhookForm').classList.add('hidden');
    $('webhookResult').classList.remove('hidden');
    toast('Webhook created', 'success');
    loadWebhooks();
  } catch (err) { toast('Create failed: ' + err.message, 'error'); }
}
async function deleteWebhook(id) {
  try {
    await fetchJSON('/v1/api/webhooks?id=' + encodeURIComponent(id), {method: 'DELETE'});
    toast('Webhook deleted — recreate it to undo (a new secret is issued)', 'success');
    loadWebhooks();
  } catch (err) { toast('Delete failed: ' + err.message, 'error'); }
}
