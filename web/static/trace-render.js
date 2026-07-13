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
})();
