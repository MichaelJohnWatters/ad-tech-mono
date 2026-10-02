// trace-render.js — the shared trace/flow-timeline renderer.
//
// One function, renderTrace(el, data), draws the .flow / .node / .service /
// .panels markup that the Trace Explorer, Publisher Simulator, and the tenant
// portals all use. The CSS classes live in theme.css (.flow, .node,
// .service.<name>, .time, .msg, .detail, .panel, .metric). Keeping the render
// in one place means the dev tools and the portal drawers stay visually
// identical and a change lands everywhere at once.
//
// data = {
//   traceId:   "<32-hex>"            // optional; shown in the header + Jaeger link
//   note:      "human status line"   // optional; appended after the trace id
//   jaegerBase:"http://host:16686"   // optional; enables the [Jaeger →] link (staff only)
//   steps:  [ { time, service, cls, msg, detail } ]   // the timeline rows
//   panels: [ { title, value, label, cls } ]          // optional summary cards
// }
//
// steps[].msg / .detail may contain HTML (the callers build controlled markup);
// callers that render UNTRUSTED text must escape it before passing it in.
(function () {
  function cap(s) {
    s = s || "";
    return s.charAt(0).toUpperCase() + s.slice(1);
  }

  function jaegerLink(base, tid) {
    if (!base || !tid || tid.length !== 32) return "";
    return ` <a href="${base}/trace/${tid}" target="_blank" rel="noopener" class="jaeger-link">[Jaeger →]</a>`;
  }

  function stepHTML(s) {
    const svc = s.service || "";
    const detail = s.detail ? ` <span class="detail">${s.detail}</span>` : "";
    return `<div class="node ${s.cls || ""}">
      <span class="time">${s.time || ""}</span>
      <span class="service ${svc}">[${cap(svc)}]</span>
      <span class="msg">${s.msg || ""}${detail}</span>
    </div>`;
  }

  function panelHTML(p) {
    return `<div class="panel"><h3>${p.title || ""}</h3><div class="metric ${p.cls || "blue"}">${p.value}</div><div class="label">${p.label || ""}</div></div>`;
  }

  // Renders a trace timeline into el (a DOM node). Returns nothing.
  window.renderTrace = function renderTrace(el, data) {
    if (!el) return;
    data = data || {};
    let html = "";

    if (data.traceId || data.note) {
      const tid = data.traceId
        ? `Trace ID: <code>${data.traceId}</code>${jaegerLink(data.jaegerBase, data.traceId)}`
        : "";
      const sep = tid && data.note ? " &mdash; " : "";
      html += `<div class="demo-note">${tid}${sep}${data.note || ""}</div>`;
    }

    html += '<div class="flow">';
    (data.steps || []).forEach(function (s) { html += stepHTML(s); });
    html += "</div>";

    if (data.panels && data.panels.length) {
      html += '<div class="panels">';
      data.panels.forEach(function (p) { html += panelHTML(p); });
      html += "</div>";
    }

    el.innerHTML = html;
  };

  // Flow-mode CSS is injected once (keeps renderFlow self-contained — works on any
  // page that loads this file, no theme.css dependency).
  function ensureFlowCSS() {
    if (document.getElementById("flow-css")) return;
    const st = document.createElement("style");
    st.id = "flow-css";
    st.textContent =
      ".flow-diagram{display:flex;flex-wrap:wrap;align-items:stretch;margin:14px 0;}" +
      ".flow-svc{flex:0 1 auto;min-width:150px;max-width:240px;border:1px solid var(--border,#30363d);border-radius:8px;background:var(--bg-surface-2,#161b22);overflow:hidden;}" +
      ".flow-svc-head{padding:7px 10px;font-weight:700;font-size:13px;border-bottom:1px solid var(--border,#30363d);background:var(--bg-surface-3,#1c2333);}" +
      ".flow-svc-steps{padding:7px 9px;display:flex;flex-direction:column;gap:5px;}" +
      ".flow-step{font-size:11px;color:var(--text-secondary,#8b949e);line-height:1.35;}" +
      ".flow-step .t{color:var(--text-muted,#6e7681);font-variant-numeric:tabular-nums;margin-left:5px;}" +
      ".flow-step.error{color:#f87171;}.flow-step.warn{color:#fbbf24;}" +
      ".flow-arrow{display:flex;align-items:center;padding:0 7px;color:var(--brand,#6366f1);font-size:22px;font-weight:700;}" +
      "@media(max-width:720px){.flow-diagram{flex-direction:column;align-items:stretch;}.flow-arrow{transform:rotate(90deg);padding:4px 0;justify-content:center;}.flow-svc{max-width:none;}}";
    document.head.appendChild(st);
  }

  // Renders the SAME trace data as a left-to-right SERVICE FLOW (one box per
  // service in call order, each listing its events + timings, arrows between) —
  // "flow mode". Services are grouped in first-seen order, which is exactly the
  // call sequence (SSP → Exchange → DSP → Ad Server → Tracker → …).
  window.renderFlow = function renderFlow(el, data) {
    if (!el) return;
    data = data || {};
    ensureFlowCSS();
    const order = [], byService = {};
    (data.steps || []).forEach(function (s) {
      const svc = s.service || "unknown";
      if (!byService[svc]) { byService[svc] = []; order.push(svc); }
      byService[svc].push(s);
    });
    let html = "";
    if (data.traceId || data.note) {
      const tid = data.traceId ? `Trace ID: <code>${data.traceId}</code>${jaegerLink(data.jaegerBase, data.traceId)}` : "";
      const sep = tid && data.note ? " &mdash; " : "";
      html += `<div class="demo-note">${tid}${sep}${data.note || ""}</div>`;
    }
    if (!order.length) { html += '<div class="demo-note">No steps to flow.</div>'; el.innerHTML = html; return; }
    html += '<div class="flow-diagram">';
    order.forEach(function (svc, i) {
      if (i > 0) html += '<div class="flow-arrow">&rarr;</div>';
      html += '<div class="flow-svc"><div class="flow-svc-head"><span class="service ' + svc + '">' + cap(svc) + "</span></div><div class=\"flow-svc-steps\">";
      byService[svc].forEach(function (s) {
        html += '<div class="flow-step ' + (s.cls || "") + '">' + (s.msg || "") + (s.time ? '<span class="t">' + s.time + "</span>" : "") + "</div>";
      });
      html += "</div></div>";
    });
    html += "</div>";
    if (data.panels && data.panels.length) {
      html += '<div class="panels">';
      data.panels.forEach(function (p) { html += panelHTML(p); });
      html += "</div>";
    }
    el.innerHTML = html;
  };
})();
