// W3, part 1: listen for the inline script's CSP violation before it happens.
window.__violations = [];
document.addEventListener('securitypolicyviolation', (e) => {
  window.__violations.push(e.effectiveDirective || e.violatedDirective);
});
