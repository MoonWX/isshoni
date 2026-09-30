// Vitest setup file (vite.config.ts `test.setupFiles`): runs before every test file, in the jsdom and the node
// environment alike.
// - jest-dom matchers (toBeInTheDocument, toHaveTextContent, …) on Vitest's expect;
// - Testing Library cleanup after each test (Vitest globals are off, so it doesn't register itself);
// - the i18n catalog, so components render real English strings.
// Later slices add their shared fixtures here (S27: MSW server lifecycle).
import '@testing-library/jest-dom/vitest';

import { cleanup } from '@testing-library/react';
import { afterEach } from 'vitest';

import { initI18n } from '../i18n';

initI18n();

afterEach(() => {
  cleanup();
});
