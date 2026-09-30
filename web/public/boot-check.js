/*
 * isshoni boot check (05 §4). A classic script that index.html loads before the module entry, so it also runs in
 * browsers too old for ES modules. It checks the minimum the SPA needs; if something is missing it writes a static
 * English message into #root and sets window.__ISSHONI_UNSUPPORTED__, and main.tsx then does nothing. Without it,
 * old iOS versions would show a blank page.
 *
 * ES5 only (the lint config parses this file as ES5), no innerHTML (04's CSP and 05 §20), no i18n (the catalog is
 * part of the module bundle). The service worker caches this file with the shell (05 §16.2).
 */
(function () {
  'use strict';

  var supported =
    'RTCPeerConnection' in window &&
    'RTCRtpTransceiver' in window &&
    typeof Array.prototype.at === 'function' &&
    typeof window.structuredClone === 'function';
  if (supported) return;

  window.__ISSHONI_UNSUPPORTED__ = true;

  var root = document.getElementById('root');
  if (!root) return;
  while (root.firstChild) root.removeChild(root.firstChild);

  var box = document.createElement('div');
  box.className = 'boot-message';
  box.setAttribute('role', 'alert');

  var title = document.createElement('h1');
  title.appendChild(document.createTextNode('This browser is too old for isshoni'));
  box.appendChild(title);

  var hint = document.createElement('p');
  hint.appendChild(
    document.createTextNode(
      'Update it, or open this page in a current version of Chrome, Edge, Firefox or Safari. ' +
        'On an iPhone or iPad, update iOS.'
    )
  );
  box.appendChild(hint);

  root.appendChild(box);
})();
