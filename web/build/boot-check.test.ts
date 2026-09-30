// @vitest-environment jsdom
// public/boot-check.js (05 §4): runs before the module entry and flags browsers the SPA can't run in.
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// A path, not a URL object: in the jsdom environment the global URL is jsdom's, which node:fs rejects.
const source = readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), '../public/boot-check.js'), 'utf8');

type BootWindow = Window & { __ISSHONI_UNSUPPORTED__?: boolean };

function runBootCheck(): void {
  // The file is a classic script; run it the way a <script> tag would, in the page's global scope.
  // eslint-disable-next-line @typescript-eslint/no-implied-eval -- executing our own static file under test
  const script = new Function(source) as () => void;
  script();
}

describe('boot-check.js', () => {
  let root: HTMLElement;

  beforeEach(() => {
    document.body.innerHTML = '<div id="root"><p>placeholder</p></div>';
    root = document.getElementById('root') as HTMLElement;
    delete (window as BootWindow).__ISSHONI_UNSUPPORTED__;
    // jsdom has no WebRTC: stand in for a current browser.
    vi.stubGlobal('RTCPeerConnection', vi.fn());
    vi.stubGlobal('RTCRtpTransceiver', vi.fn());
    if (typeof window.structuredClone !== 'function') vi.stubGlobal('structuredClone', globalThis.structuredClone);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = '';
  });

  it('does nothing in a browser that has everything', () => {
    runBootCheck();
    expect((window as BootWindow).__ISSHONI_UNSUPPORTED__).toBeUndefined();
    expect(root.textContent).toBe('placeholder');
  });

  it.each(['RTCPeerConnection', 'RTCRtpTransceiver', 'structuredClone'])(
    'without %s: flags the browser and shows the static message',
    (name) => {
      vi.stubGlobal(name, undefined);
      Reflect.deleteProperty(window, name);
      runBootCheck();
      expect((window as BootWindow).__ISSHONI_UNSUPPORTED__).toBe(true);
      const alert = root.querySelector('[role="alert"]');
      expect(alert).not.toBeNull();
      expect(alert?.querySelector('h1')?.textContent).toBe('This browser is too old for isshoni');
      expect(root.textContent).not.toContain('placeholder');
    },
  );

  it('without Array.prototype.at: flags the browser', () => {
    const at = Object.getOwnPropertyDescriptor(Array.prototype, 'at');
    Reflect.deleteProperty(Array.prototype, 'at');
    try {
      runBootCheck();
    } finally {
      if (at) Object.defineProperty(Array.prototype, 'at', at);
    }
    expect((window as BootWindow).__ISSHONI_UNSUPPORTED__).toBe(true);
  });
});
