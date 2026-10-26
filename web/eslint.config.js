// ESLint flat config (05 §2). TypeScript files get typescript-eslint's strict type-checked rules (through the
// project service and the tsconfig.*.json projects); every TS file under src/ gets React hooks, TSX files jsx-a11y
// (strict). JavaScript files (this file, scripts/, public/boot-check.js) use ESLint's own parser.
import js from '@eslint/js';
import { defineConfig, globalIgnores } from 'eslint/config';
import i18next from 'eslint-plugin-i18next';
import jsxA11y from 'eslint-plugin-jsx-a11y';
import reactHooks from 'eslint-plugin-react-hooks';
import globals from 'globals';
import tseslint from 'typescript-eslint';

// Markup that must never appear (no-restricted-syntax entries).
const noRawMarkup = [
  { selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']", message: 'No raw HTML (CSP + XSS).' },
  // No markup may trigger a desktop runtime call by itself (docs/m2/03 §5.6).
  {
    selector: 'JSXAttribute[name.name=/^(data-)?wml-/]',
    message: 'No Wails WML attributes (docs/m2/03 §5.6).',
  },
];

// The SPA never navigates away (docs/m2/03 §5.5): outside platform/, nothing calls window.open, location.assign or
// location.replace, or sets location or location.href. no-restricted-properties can't match window.location.assign
// or an assignment, hence selectors.
const navigationMessage = 'Navigation and new windows go through the Platform (docs/m2/03 §5.5).';
const noNavigation = [
  "MemberExpression[property.name='open'][object.name=/^(window|globalThis|self)$/]",
  "MemberExpression[property.name=/^(assign|replace)$/][object.name='location']",
  "MemberExpression[property.name=/^(assign|replace)$/][object.property.name='location']",
  "AssignmentExpression > MemberExpression.left[property.name='href'][object.name='location']",
  "AssignmentExpression > MemberExpression.left[property.name='href'][object.property.name='location']",
  "AssignmentExpression > Identifier.left[name='location']",
  "AssignmentExpression > MemberExpression.left[property.name='location']",
].map((selector) => ({ selector, message: navigationMessage }));

export default defineConfig(
  // First entry: generated and build-output files are never linted.
  // The desktop bridge's codes (tygo) and its Wails bindings (`task gen:bindings`) are generated too (docs/m2/03 §6).
  globalIgnores([
    'dist/',
    'src/protocol/*.gen.ts',
    'src/platform/desktop/*.gen.ts',
    'src/platform/desktop/bindings/',
    'playwright-report/',
    'test-results/',
  ]),

  js.configs.recommended,

  {
    files: ['**/*.{ts,tsx,mts,cts}'],
    extends: [tseslint.configs.strictTypeChecked],
    languageOptions: {
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
  },
  // Hooks rules on every app source file: custom hooks live in .ts files too (auth/useMe.ts, viewer/useVisibility.ts).
  {
    files: ['src/**/*.{ts,tsx}'],
    extends: [reactHooks.configs.flat.recommended],
  },
  {
    files: ['**/*.tsx'],
    extends: [jsxA11y.flatConfigs.strict],
  },

  {
    plugins: { i18next },
    rules: {
      // Every user-visible string comes from en.json (05 §16.5).
      'i18next/no-literal-string': [
        'error',
        {
          mode: 'jsx-only',
          'jsx-attributes': { include: ['aria-label', 'aria-description', 'title', 'alt', 'placeholder', 'label'] },
        },
      ],
      'no-restricted-syntax': ['error', ...noRawMarkup],
      // Use platform.storage (a try/catch wrapper that falls back to memory).
      'no-restricted-globals': ['error', 'localStorage', 'sessionStorage'],
    },
  },
  // The app outside platform/: no navigation either (docs/m2/03 §5.5; the platform files are exempt).
  {
    files: ['src/**/*.{ts,tsx}'],
    ignores: ['src/platform/**'],
    rules: { 'no-restricted-syntax': ['error', ...noRawMarkup, ...noNavigation] },
  },
  // No UI strings: tests and the shared test support (src/test/, src/**/testing/: the Vitest setup file, fakes, MSW
  // handlers), tooling and the service worker.
  {
    files: [
      '**/*.test.*',
      'src/test/**',
      'src/**/testing/**',
      'e2e/**',
      'scripts/**',
      'build/**',
      'src/sw/**',
      'public/boot-check.js',
    ],
    rules: { 'i18next/no-literal-string': 'off' },
  },
  // Web Storage directly: the platform wrapper itself, tests and the shared test support.
  {
    files: ['src/platform/browser/storage.ts', '**/*.test.*', 'src/test/**', 'src/**/testing/**', 'e2e/**'],
    rules: { 'no-restricted-globals': 'off' },
  },

  // JavaScript: Node tooling, and the one classic browser script.
  {
    files: ['**/*.{js,mjs,cjs}'],
    languageOptions: { globals: globals.node },
  },
  {
    // Runs before the module bundle in browsers too old for it (05 §4), so it must stay ES5.
    files: ['public/boot-check.js'],
    languageOptions: { ecmaVersion: 5, sourceType: 'script', globals: globals.browser },
  },
);
