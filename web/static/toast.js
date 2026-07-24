// Shared toast notification — works on any page that includes the toast
// partial (which renders the #toastContainer slot). Three styles:
//
//   toast('Saved', 'success')   // green
//   toast('Failed: ...', 'error') // red
//   toast('Heads up', 'info')   // brand blue (default)
//
// Toasts auto-dismiss after 3s with a slide+fade transition. Container
// position is set in partials/toast.html (fixed bottom-right) so this
// JS stays purely about the message lifecycle.
//
// History: this used to be inlined in config/manager.html. Promoted
// to shared during Phase 2a of the UI design audit so the simulator
// and other surfaces can stop using window.confirm() (which violates
// the saved "no confirmation dialogs" preference) and reach for
// toast + undo instead.
function toast(message, type) {
    var container = document.getElementById('toastContainer');
    if (!container) {
        // Page didn't include the toast partial — log to console so
        // dev notices, but don't blow up calling code.
        console.warn('toast() called but #toastContainer missing; add {{ template "toast" . }} to the page');
        return;
    }
    var t = document.createElement('div');
    // Inline styles so the toast renders consistently on Tailwind pages
    // (manager.html) AND CSS-var pages (simulator/minimal.html). The
    // bg- Tailwind classes used to be here but they no-op on pages
    // without the Tailwind config loaded.
    var bg = {
        success: '#4ade80',
        error:   '#f87171',
        info:    '#4361ee',
    }[type] || '#4361ee';
    t.style.cssText =
        'background:' + bg + ';' +
        'color:white;font-size:13px;padding:8px 16px;border-radius:8px;' +
        'box-shadow:0 4px 12px rgba(0,0,0,0.3);transition:all 0.2s ease;' +
        'transform:translateY(8px);opacity:0;font-weight:500;';
    t.textContent = message;
    container.appendChild(t);
    requestAnimationFrame(function() {
        t.style.transform = 'translateY(0)';
        t.style.opacity = '1';
    });
    setTimeout(function() {
        t.style.transform = 'translateY(8px)';
        t.style.opacity = '0';
        setTimeout(function() { t.remove(); }, 300);
    }, 3000);
}

// toastUndo — the "no confirmation dialogs" pattern: apply the change
// immediately, then offer a 6s window to reverse it. onUndo runs only if
// the user clicks Undo before the toast dismisses.
function toastUndo(message, onUndo) {
    var container = document.getElementById('toastContainer');
    if (!container) {
        console.warn('toastUndo() called but #toastContainer missing');
        return;
    }
    var t = document.createElement('div');
    t.style.cssText =
        'background:#4ade80;color:white;font-size:13px;padding:8px 16px;' +
        'border-radius:8px;box-shadow:0 4px 12px rgba(0,0,0,0.3);' +
        'transition:all 0.2s ease;transform:translateY(8px);opacity:0;' +
        'font-weight:500;display:flex;align-items:center;gap:12px;';
    var span = document.createElement('span');
    span.textContent = message;
    var btn = document.createElement('button');
    btn.textContent = 'Undo';
    btn.style.cssText =
        'background:rgba(255,255,255,0.25);border:none;color:white;' +
        'font-size:12px;font-weight:600;padding:2px 10px;border-radius:6px;cursor:pointer;';
    var dismiss = function() {
        t.style.transform = 'translateY(8px)';
        t.style.opacity = '0';
        setTimeout(function() { t.remove(); }, 300);
    };
    btn.onclick = function() { dismiss(); onUndo(); };
    t.appendChild(span);
    t.appendChild(btn);
    container.appendChild(t);
    requestAnimationFrame(function() {
        t.style.transform = 'translateY(0)';
        t.style.opacity = '1';
    });
    setTimeout(dismiss, 6000);
}
