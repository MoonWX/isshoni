// Vitest setup file (vite.config.ts `test.setupFiles`): runs before every test file, in the jsdom and the node
// environment alike.
// - jest-dom matchers (toBeInTheDocument, toHaveTextContent, …) on Vitest's expect;
// - Testing Library cleanup after each test (Vitest globals are off, so it doesn't register itself);
// - the i18n catalog, so components render real English strings;
// - (S27) MSW for REST: the server in test/msw.ts listens for the whole file, errors on requests no handler covers,
//   and drops a test's own handlers after it; api() is bound to a test platform (test/platform.ts) before each test;
// - (S27) jsdom's page counts as a secure context (a localhost page is one in browsers; jsdom leaves the flag unset);
// - (S27) FakeRTCPeerConnection.instances is emptied after each test.
import '@testing-library/jest-dom/vitest';

import { cleanup } from '@testing-library/react';
import { afterAll, afterEach, beforeAll, beforeEach } from 'vitest';

import { initI18n } from '../i18n';
import { configureApi } from '../protocol/rest';
import { FakeRTCPeerConnection } from './FakeRTCPeerConnection';
import { server } from './msw';
import { createTestPlatform } from './platform';

initI18n();

if (typeof window !== 'undefined' && !('isSecureContext' in window && window.isSecureContext)) {
  Object.defineProperty(window, 'isSecureContext', { configurable: true, value: true });
}

beforeAll(() => {
  server.listen({ onUnhandledFrame: 'error' });
});

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
