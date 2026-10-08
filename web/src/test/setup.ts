// Vitest setup file (vite.config.ts `test.setupFiles`): runs before every test file, in the jsdom and the node
// environment alike.
// - jest-dom matchers (toBeInTheDocument, toHaveTextContent, …) on Vitest's expect;
// - Testing Library cleanup after each test (Vitest globals are off, so it doesn't register itself);
// - the i18n catalog, so components render real English strings;
// - (S27) MSW for REST: the server in test/msw.ts listens for the whole file, errors on requests no handler covers,
//   and drops a test's own handlers after it; api() is bound to a test platform (test/platform.ts) before each test;
//   globalThis.WebSocket, which MSW patches read-only, stays assignable for fake signaling servers;
// - (S27) jsdom's page counts as a secure context (a localhost page is one in browsers; jsdom leaves the flag unset);
// - (S27) FakeRTCPeerConnection.instances is emptied after each test;
// - Testing Library's findBy… and waitFor wait up to 4 s instead of 1 s, and a test may take 15 s
//   (vite.config.ts): the first test of a file that renders a lazy page loads and transforms that page's folder,
//   which takes more than a second on a busy machine (several suites side by side, CI). A wait that succeeds is
//   as fast as before; only a failing one takes longer to say so.
import '@testing-library/jest-dom/vitest';

import { cleanup, configure } from '@testing-library/react';
import { afterAll, afterEach, beforeAll, beforeEach } from 'vitest';

import { initI18n } from '../i18n';
import { configureApi } from '../protocol/rest';
import { FakeRTCPeerConnection } from './FakeRTCPeerConnection';
import { server } from './msw';
import { createTestPlatform } from './platform';

initI18n();

configure({ asyncUtilTimeout: 4000 });

if (typeof window !== 'undefined' && !('isSecureContext' in window && window.isSecureContext)) {
  Object.defineProperty(window, 'isSecureContext', { configurable: true, value: true });
}

beforeAll(() => {
  server.listen({ onUnhandledFrame: 'error' });
  keepGlobalWritable('WebSocket');
});

/**
 * MSW's node server also intercepts WebSocket: it replaces globalThis.WebSocket with a configurable but read-only
 * property, so a plain assignment (protocol/testing's FakeSignalServer.install()) would throw. Making the patched
 * property writable keeps such fakes working; server.close() still restores the original, since the patch stays
 * configurable.
 */
function keepGlobalWritable(name: 'WebSocket'): void {
  const d = Object.getOwnPropertyDescriptor(globalThis, name);
  if (d && 'value' in d && d.writable !== true && d.configurable === true) {
    Object.defineProperty(globalThis, name, { ...d, writable: true });
  }
}

beforeEach(() => {
  configureApi(createTestPlatform());
});

afterEach(() => {
  cleanup();
  server.resetHandlers();
  FakeRTCPeerConnection.reset();
});

afterAll(() => {
  server.close();
});
