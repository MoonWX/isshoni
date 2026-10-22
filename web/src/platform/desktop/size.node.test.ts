// @vitest-environment node
// DesktopPlatform's weight in the entry chunk (docs/m2/03 §7.2): it stays under 4 KB gzip (1 KB = 1000 bytes, as in
// check:size) and pulls in neither the Wails runtime nor a transport (wails/, http/, bindings/), which load on demand.
//
// Built in memory by the real Vite with the app's build target: the entry is what detect.ts takes from
// platform/desktop/ (DesktopPlatform and readDesktopConfig); every module outside platform/desktop/ is external,
// because BrowserPlatform's entry carries those already (codecs, rest, the browser storage, the logger).
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { gzipSync } from 'node:zlib';

import { build, type Rolldown } from 'vite';
import { beforeAll, describe, expect, it, vi } from 'vitest';

import config from '../../../vite.config.ts';

/** The budget for platform/desktop/ in the entry chunk, gzip bytes. */
const BUDGET = 4000;

const DIR = path.dirname(fileURLToPath(import.meta.url));
const ENTRY = '\0desktop-entry';

let entry: Rolldown.OutputChunk | undefined;
let chunks: Rolldown.OutputChunk[] = [];

/** The source modules of a chunk, relative to platform/desktop/ (forward slashes). */
const modulesOf = (chunk: Rolldown.OutputChunk): string[] =>
  Object.keys(chunk.modules)
    .filter((id) => id.startsWith(DIR + path.sep))
    .map((id) => path.relative(DIR, id).split(path.sep).join('/'));

beforeAll(async () => {
  vi.stubEnv('NODE_ENV', 'production');
  try {
    const out = await build({
      configFile: false,
      mode: 'production',
      logLevel: 'silent',
      publicDir: false,
      define: config.define,
      plugins: [
        {
          name: 'desktop-entry',
          enforce: 'pre',
          resolveId: (id) => {
            if (id === ENTRY) return ENTRY;
            // The build's own helpers (Vite's preload helper, the transformer's runtime for private class members)
            // are shared by the whole entry chunk of the app: not DesktopPlatform's weight.
            if (id.includes('preload-helper') || id.includes('@oxc-project')) return { id, external: true };
            return null;
          },
          load: (id) =>
            id === ENTRY
              ? `export { DesktopPlatform } from ${JSON.stringify(path.join(DIR, 'DesktopPlatform.ts'))};\n` +
                `export { readDesktopConfig } from ${JSON.stringify(path.join(DIR, 'config.ts'))};\n`
              : null,
        },
      ],
      build: {
        write: false,
        minify: true,
        target: config.build?.target,
        rollupOptions: {
          input: ENTRY,
          preserveEntrySignatures: 'strict',
          external: (id, importer) => {
            if (id === ENTRY || importer === undefined) return false;
            const resolved = path.isAbsolute(id) ? id : path.resolve(path.dirname(importer), id);
            return !resolved.startsWith(DIR + path.sep);
          },
        },
      },
    });
    const outputs = Array.isArray(out) ? out : [out];
    chunks = outputs.flatMap((o) => ('output' in o ? o.output : [])).filter((o) => o.type === 'chunk');
    entry = chunks.find((c) => c.isEntry);
  } finally {
    vi.unstubAllEnvs();
  }
}, 60_000);

describe("DesktopPlatform's share of the entry chunk", () => {
  it(`is at most ${String(BUDGET)} bytes gzip`, () => {
    expect(entry).toBeDefined();
    const code = entry?.code ?? '';
    const gzip = gzipSync(code, { level: 9 }).length;
    console.info(`platform/desktop in the entry chunk: ${String(code.length)} bytes, ${String(gzip)} gzip`);
    expect(gzip).toBeLessThanOrEqual(BUDGET);
  });

  it('holds DesktopPlatform and its helpers, but no transport', () => {
    const modules = modulesOf(entry ?? ({ modules: {} } as Rolldown.OutputChunk));
    expect(modules).toEqual(
      expect.arrayContaining(['DesktopPlatform.ts', 'config.ts', 'tokens.ts', 'links.ts', 'lazyBridge.ts']),
    );
    for (const m of modules) expect(m).not.toMatch(/^(wails|http|bindings)\/|^actions\.ts$/);
    expect(entry?.imports.some((i) => i.includes('wailsio'))).toBe(false);
  });

  it('loads each transport, and the rarely used actions, from chunks of their own', () => {
    const lazy = chunks.filter((c) => !c.isEntry).flatMap(modulesOf);
    expect(lazy).toEqual(expect.arrayContaining(['http/HttpBridge.ts', 'wails/WailsBridge.ts', 'actions.ts']));
    expect(entry?.dynamicImports).toHaveLength(3);
  });
});
