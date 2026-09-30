// @vitest-environment node
// The build plugins (05 §17.1, §17.3) and scripts/check-size.mjs, on a small fixture app built with the real Vite.
import { access, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import zlib from 'node:zlib';

import { build } from 'vite';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { BUDGETS, checkSize } from '../scripts/check-size.mjs';
import { cleanOutDir, cleanPlugin } from './clean-plugin.ts';
import { COMPRESS_MIN_BYTES, compressPlugin, shouldCompress } from './compress-plugin.ts';
import { type BuildReport, reportPlugin } from './report-plugin.ts';
import { resolveShell } from './shell.ts';
import { DEFAULT_VERSION, PROTOCOL_VERSION, resolveBuildVersion, versionPlugin } from './version-plugin.ts';

const exists = (p: string) =>
  access(p).then(
    () => true,
    () => false,
  );

describe('resolveBuildVersion', () => {
  it.each([
    [{}, DEFAULT_VERSION],
    [{ ISSHONI_VERSION: '' }, DEFAULT_VERSION],
    [{ ISSHONI_VERSION: '  ' }, DEFAULT_VERSION],
    [{ ISSHONI_VERSION: '0.1.0' }, '0.1.0'],
    [{ ISSHONI_VERSION: '0.0.0-ci.42' }, '0.0.0-ci.42'],
    [{ ISSHONI_VERSION: '0.1.0-rc.1' }, '0.1.0-rc.1'],
    [{ ISSHONI_VERSION: '0.0.0-e2e.local' }, '0.0.0-e2e.local'],
    [{ ISSHONI_VERSION: '0.0.0-dev+1a2b3c4d5e6f-dirty' }, '0.0.0-dev+1a2b3c4d5e6f-dirty'],
  ])('%o → %s', (env, want) => {
    expect(resolveBuildVersion(env)).toBe(want);
  });

  it.each(['v0.1.0', '0.1', '01.2.3', '0.1.0-', 'latest', '0.1.0-rc.01'])('rejects %s', (v) => {
    expect(() => resolveBuildVersion({ ISSHONI_VERSION: v })).toThrow(/not a SemVer version/);
  });
});

describe('shouldCompress', () => {
  it.each([
    ['assets/a.js', COMPRESS_MIN_BYTES, true],
    ['assets/a.js', COMPRESS_MIN_BYTES - 1, false],
    ['index.html', 5000, true],
    ['assets/a.CSS', 5000, true],
    ['icons/icon.svg', 5000, true],
    ['version.json', 5000, true],
    ['manifest.webmanifest', 5000, true],
    ['licenses.txt', 5000, false],
    ['icons/icon-192.png', 5000, false],
    ['assets/a.js.br', 5000, false],
    ['assets/a.js.gz', 5000, false],
  ])('%s (%i bytes) → %s', (file, size, want) => {
    expect(shouldCompress(file, size)).toBe(want);
  });
});

describe('cleanOutDir', () => {
  it('keeps .gitkeep and removes everything else; a missing dir is fine', async () => {
    const dir = await mkdtemp(path.join(tmpdir(), 'isshoni-clean-'));
    try {
      await mkdir(path.join(dir, 'dist', 'assets'), { recursive: true });
      await writeFile(path.join(dir, 'dist', '.gitkeep'), '');
      await writeFile(path.join(dir, 'dist', 'index.html'), 'x');
      await writeFile(path.join(dir, 'dist', 'assets', 'a.js'), 'x');
      await cleanOutDir(path.join(dir, 'dist'));
      expect(await exists(path.join(dir, 'dist', '.gitkeep'))).toBe(true);
      expect(await exists(path.join(dir, 'dist', 'index.html'))).toBe(false);
      expect(await exists(path.join(dir, 'dist', 'assets'))).toBe(false);
      await cleanOutDir(path.join(dir, 'missing'));
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
});

describe('checkSize', () => {
  const entry = (file: string, gzip: number) => ({ file, bytes: gzip * 3, gzip });
  const report = (initial: number, lazy: number[], css: number): BuildReport => ({
    format: 1,
    initial: { files: [entry('assets/index.js', initial)], gzip: initial },
    lazy: lazy.map((g, i) => entry(`assets/lazy${String(i)}.js`, g)),
    css: { files: [entry('assets/index.css', css)], gzip: css },
  });

  it('passes at the budgets', () => {
    const { errors, lines } = checkSize(report(BUDGETS.initialJs, [BUDGETS.lazyChunk], BUDGETS.css));
    expect(errors).toEqual([]);
    expect(lines).toHaveLength(3);
  });

  it('fails each budget one byte over', () => {
    const { errors } = checkSize(report(BUDGETS.initialJs + 1, [10, BUDGETS.lazyChunk + 1], BUDGETS.css + 1));
    expect(errors).toHaveLength(3);
    expect(errors[0]).toMatch(/^initial JS is 200\.0 KB, over the 200\.0 KB budget: assets\/index\.js/);
    expect(errors[1]).toMatch(/^lazy chunk assets\/lazy1\.js is 120\.0 KB/);
    expect(errors[2]).toMatch(/^CSS is 30\.0 KB/);
  });

  it('rejects an unknown report format', () => {
    expect(checkSize({ ...report(1, [], 1), format: 2 }).errors[0]).toMatch(/format 2/);
  });
});

describe('a fixture build with every plugin', () => {
  let root = '';
  let dist = '';
  const bigText = 'isshoni '.repeat(400); // > 1 KiB, so the entry chunk gets .br/.gz siblings

  const buildFixture = async (version: string) => {
    await build({
      root,
      configFile: false,
      logLevel: 'silent',
      plugins: [cleanPlugin(), versionPlugin({ version }), compressPlugin(), reportPlugin()],
      build: { outDir: 'dist', emptyOutDir: false },
    });
  };

  beforeAll(async () => {
    root = await mkdtemp(path.join(tmpdir(), 'isshoni-build-'));
    dist = path.join(root, 'dist');
    await mkdir(path.join(root, 'src'), { recursive: true });
    await mkdir(path.join(root, 'public'), { recursive: true });
    await mkdir(path.join(dist, 'assets'), { recursive: true });
    await writeFile(path.join(dist, '.gitkeep'), '');
    await writeFile(path.join(dist, 'assets', 'stale-1234.js'), 'old build');
    await writeFile(
      path.join(root, 'index.html'),
      '<!doctype html><html><head><title>t</title></head><body><div id="root"></div>' +
        '<script src="/boot-check.js"></script><script type="module" src="/src/main.js"></script></body></html>\n',
    );
    await writeFile(path.join(root, 'src', 'main.css'), 'body { color: red; }\n');
    await writeFile(
      path.join(root, 'src', 'main.js'),
      `import './main.css';\nimport { shared } from './shared.js';\n` +
        `document.body.dataset.text = ${JSON.stringify(bigText)} + shared;\n` +
        `document.body.onclick = () => import('./lazy.js').then((m) => m.run());\n`,
    );
    await writeFile(path.join(root, 'src', 'shared.js'), `export const shared = 'shared';\n`);
    await writeFile(path.join(root, 'src', 'lazy.js'), `export function run() { return 'lazy'; }\n`);
    await writeFile(path.join(root, 'public', 'boot-check.js'), '/* v1 */\n');
    await buildFixture('1.2.3-ci.7');
  });

  afterAll(async () => {
    await rm(root, { recursive: true, force: true });
  });

  it('keeps dist/.gitkeep and removes the previous build', async () => {
    expect(await exists(path.join(dist, '.gitkeep'))).toBe(true);
    expect(await exists(path.join(dist, 'assets', 'stale-1234.js'))).toBe(false);
    expect(await exists(path.join(dist, 'index.html'))).toBe(true);
  });

  it('writes version.json with the version, the protocol and the shell hash', async () => {
    const file = JSON.parse(await readFile(path.join(dist, 'version.json'), 'utf8')) as Record<string, unknown>;
    expect(Object.keys(file)).toEqual(['version', 'protocol', 'shell']);
    expect(file['version']).toBe('1.2.3-ci.7');
    expect(file['protocol']).toBe(PROTOCOL_VERSION);
    expect(file['shell']).toMatch(/^[0-9a-f]{12}$/);
  });

  it('writes build-report.json with initial, lazy and CSS sizes', async () => {
    const report = JSON.parse(await readFile(path.join(root, 'build-report.json'), 'utf8')) as BuildReport;
    expect(report.format).toBe(1);
    expect(report.initial.files.length).toBeGreaterThanOrEqual(1);
    expect(report.initial.files.every((f) => f.file.startsWith('assets/') && f.gzip > 0)).toBe(true);
    expect(report.lazy.map((f) => f.file)).toEqual([expect.stringMatching(/^assets\/lazy-.*\.js$/)]);
    expect(report.css.files.map((f) => f.file)).toEqual([expect.stringMatching(/^assets\/.*\.css$/)]);
    expect(report.initial.gzip).toBe(report.initial.files.reduce((s, f) => s + f.gzip, 0));
    expect(checkSize(report).errors).toEqual([]);
  });

  it('writes .br and .gz siblings that decompress to the original, only for text files of 1 KiB or more', async () => {
    const report = JSON.parse(await readFile(path.join(root, 'build-report.json'), 'utf8')) as BuildReport;
    const main = report.initial.files.find((f) => f.bytes >= COMPRESS_MIN_BYTES);
    if (!main) throw new Error('no entry chunk of 1 KiB or more in the fixture build');
    const file = path.join(dist, main.file);
    const original = await readFile(file);
    expect(zlib.brotliDecompressSync(await readFile(file + '.br'))).toEqual(original);
    expect(zlib.gunzipSync(await readFile(file + '.gz'))).toEqual(original);

    const lazy = report.lazy[0];
    if (!lazy) throw new Error('no lazy chunk in the fixture build');
    expect(await exists(path.join(dist, lazy.file + '.gz'))).toBe(false); // under 1 KiB
    expect(await exists(path.join(dist, 'boot-check.js.gz'))).toBe(false); // under 1 KiB
    expect(await exists(path.join(dist, 'version.json.br'))).toBe(false); // under 1 KiB
  });

  it('versions the shell by content: a changed public shell file changes the hash', async () => {
    const before = JSON.parse(await readFile(path.join(dist, 'version.json'), 'utf8')) as { shell: string };
    const report = JSON.parse(await readFile(path.join(root, 'build-report.json'), 'utf8')) as BuildReport;
    const urls = ['/', '/boot-check.js', ...report.initial.files.map((f) => '/' + f.file)];
    urls.push(...report.css.files.map((f) => '/' + f.file), '/manifest.webmanifest');
    const shell = await resolveShell(dist, urls);
    expect(shell.version).toBe(before.shell);
    expect(shell.urls).not.toContain('/manifest.webmanifest'); // missing public files are skipped
    expect(shell.urls[0]).toBe('/');

    await writeFile(path.join(root, 'public', 'boot-check.js'), '/* v2 */\n');
    await buildFixture('1.2.3-ci.8');
    const after = JSON.parse(await readFile(path.join(dist, 'version.json'), 'utf8')) as { shell: string };
    expect(after.shell).not.toBe(before.shell);
  });
});
