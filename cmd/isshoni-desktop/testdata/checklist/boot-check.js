// The checklist's boot-check.js: a classic script that runs before the inline one, so it sees that one's CSP
// violation (W3).
window.__violations = [];
document.addEventListener('securitypolicyviolation', (e) => {
  window.__violations.push(e.effectiveDirective || e.violatedDirective);
});
