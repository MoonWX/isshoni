/// <reference types="vitest/config" />
// Vite, build and Vitest config (05 §17.3).
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

import { cleanPlugin } from './build/clean-plugin.ts';
import { compressPlugin } from './build/compress-plugin.ts';
import { reportPlugin } from './build/report-plugin.ts';
import { swPlugin } from './build/sw-plugin.ts';
import { resolveBuildVersion, versionPlugin } from './build/version-plugin.ts';

// One version per build (06 §7.2): env ISSHONI_VERSION, else 0.0.0-dev.
const version = resolveBuildVersion();

// `task dev:server` listens here (06 §7.3); the dev proxy sends /api, /ws and /healthz to it.
const DEV_SERVER = process.env['ISSHONI_DEV_SERVER'] ?? 'http://127.0.0.1:8080';

export default defineConfig({
  plugins: [react(), cleanPlugin(), swPlugin(), versionPlugin({ version }), compressPlugin(), reportPlugin()],
  define: { __ISSHONI_VERSION__: JSON.stringify(version) },
  build: {
    outDir: 'dist',
    emptyOutDir: false, // keeps dist/.gitkeep; cleanPlugin deletes the old build output
    sourcemap: false,
    assetsInlineLimit: 0, // no data: URIs; every asset is a file under /assets/ (04's CSP)
    target: ['es2022', 'chrome111', 'edge111', 'firefox115', 'safari15.4'],
  },
  server: {
    port: 5173,
    strictPort: true,
    // changeOrigin: false keeps Host and Origin as the browser sent them, so the server's Origin, CSRF and Host
    // checks see the public URL (http://localhost:5173 in dev).
    proxy: {
      '/api': { target: DEV_SERVER, changeOrigin: false },
      '/ws': { target: DEV_SERVER, changeOrigin: false, ws: true },
      '/healthz': { target: DEV_SERVER, changeOrigin: false },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}', 'build/**/*.test.ts'],
    // Room for the waits of src/test/setup.ts (4 s each) on a busy machine; the default is 5 s.
    testTimeout: 15_000,
  },
});
