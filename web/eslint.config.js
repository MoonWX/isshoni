// ESLint flat config (05 §2). TypeScript files get typescript-eslint's strict type-checked rules (through the
// project service and the tsconfig.*.json projects), React hooks and jsx-a11y (strict). JavaScript files (this file,
// scripts/, public/boot-check.js) use ESLint's own parser.
import js from '@eslint/js';
import { defineConfig, globalIgnores } from 'eslint/config';
import i18next from 'eslint-plugin-i18next';
import jsxA11y from 'eslint-plugin-jsx-a11y';
import reactHooks from 'eslint-plugin-react-hooks';
import globals from 'globals';
import tseslint from 'typescript-eslint';

export default defineConfig(
  // First entry: generated and build-output files are never linted.
  globalIgnores(['dist/', 'src/protocol/*.gen.ts', 'playwright-report/', 'test-results/']),

  js.configs.recommended,

  {
    files: ['**/*.{ts,tsx,mts,cts}'],
    extends: [tseslint.configs.strictTypeChecked],
    languageOptions: {
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
  },
  {
    files: ['**/*.tsx'],
    extends: [reactHooks.configs.flat.recommended, jsxA11y.flatConfigs.strict],
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
      'no-restricted-syntax': [
        'error',
        { selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']", message: 'No raw HTML (CSP + XSS).' },
      ],
      // Use platform.storage (a try/catch wrapper that falls back to memory).
      'no-restricted-globals': ['error', 'localStorage', 'sessionStorage'],
    },
  },
  {
    files: ['**/*.test.*', 'e2e/**', 'scripts/**', 'build/**', 'src/sw/**', 'public/boot-check.js'],
    rules: { 'i18next/no-literal-string': 'off' },
  },
  {
    files: ['src/platform/browser/storage.ts', '**/*.test.*', 'e2e/**'],
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
