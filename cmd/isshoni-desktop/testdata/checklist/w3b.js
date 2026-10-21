// W3, part 2: report whether the inline script ran under the header-only CSP. The violation event is dispatched
// asynchronously, so wait a moment for it.
setTimeout(() => {
  void fetch('/__checklist/report?step=w3', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ inlineRan: window.__inlineRan === true, violations: window.__violations.slice() }),
  });
}, 300);
