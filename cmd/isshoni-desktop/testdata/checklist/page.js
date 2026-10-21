// The Wails checklist page (03 §3.2; checklist_test.go). It runs in the app's real webview, behind the real asset
// handler and Guard, and reports what it saw to the Go test, which decides each row. Every check runs from page
// script, as an injected script in the SPA could.
const results = {};
const out = document.getElementById('out');

function note(key, value) {
  results[key] = value;
  out.textContent = JSON.stringify(results, null, 2);
}

async function report(step, data) {
  await fetch('/__checklist/report?step=' + encodeURIComponent(step), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
}

// Stray errors are noted, not fatal: the runtime's CancellablePromise reports the rejection of a cancelled call
// that nobody awaits as an unhandled rejection (W11 cancels one on purpose).
const stray = [];
window.addEventListener('error', (e) => stray.push('error: ' + String(e.message)));
window.addEventListener('unhandledrejection', (e) => {
  const r = e.reason;
  stray.push('unhandled rejection: ' + String((r && r.message) || r).slice(0, 120));
});

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// raw sends a /wails/runtime request as runtime.ts builds it (a JSON body), or in the transport's query form. simple
// sends it as a CORS simple request instead (text/plain, no x-wails-client-id), as another origin could without a
// preflight.
async function raw({ object, method, args, query, headers, body, simple }) {
  let url = '/wails/runtime';
  const base = simple
    ? { 'Content-Type': 'text/plain' }
    : { 'x-wails-client-id': 'checklist', 'Content-Type': 'application/json' };
  const init = { method: 'POST', headers: { ...base, ...(headers || {}) } };
  if (query) {
    const q = new URLSearchParams({ object: String(object), method: String(method) });
    if (args !== undefined) q.set('args', JSON.stringify(args));
    url += '?' + q.toString();
  } else if (body !== undefined) {
    init.body = body;
  } else {
    init.body = JSON.stringify(args === undefined ? { object, method } : { object, method, args });
  }
  try {
    const r = await fetch(url, init);
    return { status: r.status, text: (await r.text()).slice(0, 120) };
  } catch (e) {
    return { status: 0, text: String(e.message) };
  }
}

// settle runs a runtime call and says whether it resolved or how it rejected.
async function settle(fn) {
  try {
    const value = await fn();
    return { resolved: true, value: value === undefined ? null : value };
  } catch (e) {
    return { resolved: false, name: e && e.name, message: String((e && e.message) || e).slice(0, 120) };
  }
}

async function status(path, init) {
  try {
    const r = await fetch(path, init);
    return r.status;
  } catch {
    return 0;
  }
}

async function main() {
  const cfg = await (await fetch('/__checklist/config')).json();

  note('W1', { origin: location.origin });
  note('W2', { isSecureContext: window.isSecureContext === true });
  const b = window.__ISSHONI_DESKTOP__;
  note('bridge', { present: !!b, frozen: !!b && Object.isFrozen(b), v: b ? b.v : null, os: b ? b.app.os : null });
  note('W3meta', { inlineRan: window.__inlineRan === true, violations: window.__violations.slice() });

  // The npm runtime, as the SPA bundles it; Wails' own copy as the fallback.
  let rt = null;
  let from = 'npm';
  try {
    rt = await import('/__checklist/rt/index.js');
  } catch (e) {
    from = 'wails (' + e.message + ')';
    try {
      rt = await import('/wails/runtime.js');
    } catch (e2) {
      from = 'none (' + e2.message + ')';
    }
  }
  note('runtime', { from });

  // W4: the webview's own requests to /wails/* (no Origin on a same-origin GET or HEAD; the Guard then needs
  // Sec-Fetch-Site). The Go side records the headers it saw.
  note('W4', {
    runtimeJS: await status('/wails/runtime.js'),
    customJSHead: await status('/wails/custom.js', { method: 'HEAD' }),
    eventPayload: await status('/wails/eventpayload/' + '0'.repeat(32), { cache: 'no-store' }),
    stream: await status('/wails/stream/poll'),
  });

  if (rt) {
    // A bound method answers (W21's control).
    note('echo', await settle(() => rt.Call.ByName(cfg.fqn.echo, 'checklist')));
    // W12: a Go error rejects with err.Error() verbatim.
    note('W12', await settle(() => rt.Call.ByName(cfg.fqn.fail)));
    // W11: cancelling the JS call cancels the Go context.
    const pending = rt.Call.ByName(cfg.fqn.wait);
    await sleep(300);
    pending.cancel();
    note('W11', { call: await settle(() => pending), go: await (await fetch('/__checklist/wait-status')).json() });

    // A large Go event comes through /wails/eventpayload/ (more than 8 KiB).
    const big = new Promise((resolve) => {
      const off = rt.Events.On('checklist:big', (ev) => {
        off();
        resolve({ delivered: true, size: JSON.stringify(ev.data).length });
      });
      setTimeout(() => resolve({ delivered: false }), 3000);
    });
    await fetch('/__checklist/emit-big', { method: 'POST' });
    note('bigEvent', await big);

    // W21: the runtime's own objects, called through the runtime as page script would.
    const visibleBefore = document.visibilityState;
    note('W21runtime', {
      openSettings: await settle(() => rt.Browser.OpenURL('ms-settings:display')),
      openSearch: await settle(() => rt.Browser.OpenURL('search-ms:query=x')),
      clipboardText: await settle(() => rt.Clipboard.Text()),
      clipboardSetText: await settle(() => rt.Clipboard.SetText('checklist')),
      windowHide: await settle(() => rt.Window.Hide()),
      windowOpenDevTools: await settle(() => rt.Window.OpenDevTools()),
      applicationHide: await settle(() => rt.Application.Hide()),
      eventsEmit: await settle(() => rt.Events.Emit('checklist:from-page', 1)),
      systemEnvironment: await settle(() => rt.System.Environment()),
      screensGetAll: await settle(() => rt.Screens.GetAll()),
    });
    await sleep(500);
    note('visibility', { before: visibleBefore, after: document.visibilityState });
  }

  // W21: raw requests as the pinned transport reads them.
  const callArgs = { 'call-id': 'checklist-raw', methodName: cfg.fqn.echo, args: ['raw'] };
  note('W21raw', {
    callBody: await raw({ object: 0, method: 0, args: callArgs }),
    callQuery: await raw({ object: 0, method: 0, args: { ...callArgs, 'call-id': 'checklist-q' }, query: true }),
    openURLBody: await raw({ object: 9, method: 0, args: { url: 'ms-settings:display' } }),
    openURLQuery: await raw({ object: 9, method: 0, args: { url: 'search-ms:query=x' }, query: true }),
    windowHideQuery: await raw({ object: 6, method: 11, query: true }),
    chunked: await raw({
      object: 0,
      method: 0,
      args: { ...callArgs, 'call-id': 'checklist-c' },
      headers: { 'x-wails-chunk-id': 'c1', 'x-wails-chunk-index': '0', 'x-wails-chunk-total': '1' },
    }),
    oversize: await raw({ body: JSON.stringify({ object: 0, method: 0, args: { pad: 'x'.repeat(70000) } }) }),
    unparsable: await raw({ body: 'object=9&method=0' }),
    simpleCall: await raw({ object: 0, method: 0, args: { ...callArgs, 'call-id': 'checklist-s' }, simple: true }),
  });

  // W13: viewer audio plays without a click.
  try {
    const a = new Audio('/__checklist/tone.wav');
    a.volume = 0.01;
    await a.play();
    note('W13', { autoplay: true });
    a.pause();
  } catch (e) {
    note('W13', { autoplay: false, error: e.name });
  }

  note('stray', stray.slice());
  await report('main', results);
  document.getElementById('status').textContent = 'Done. Checking the header-only CSP next.';
  location.href = '/__checklist/w3';
}

main().catch((e) => void report('fatal', { message: String(e && e.message) }));
