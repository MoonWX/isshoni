// @vitest-environment node
// The service-worker build (05 §16.2) on a small fixture app, built with the real Vite, the real src/sw/sw.ts and the
// real public/ directory; then the built dist/sw.js is run in a fake service-worker scope. Also the manifest and the
// icons of public/ (05 §16.1).
import { access, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';
import zlib from 'node:zlib';

import { build } from 'vite';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { cleanPlugin } from './clean-plugin.ts';
import { compressPlugin } from './compress-plugin.ts';
import { readPushStrings, SW_FILE, swBuildConfig, swDefine, swPlugin } from './sw-plugin.ts';
import { versionPlugin } from './version-plugin.ts';

const WEB_ROOT = fileURLToPath(new URL('..', import.meta.url));
const SW_ENTRY = path.join(WEB_ROOT, 'src', 'sw', 'sw.ts');
const PUBLIC_DIR = path.join(WEB_ROOT, 'public');
const CATALOG = path.join(WEB_ROOT, 'src', 'i18n', 'en.json');
const ORIGIN = 'https://isshoni.test';

const exists = (p: string) =>
  access(p).then(
    () => true,
    () => false,
  );

interface FakeNotification {
  title: string;
  options: { body?: string; icon?: string; data?: unknown } | undefined;
}

/** Runs a built sw.js as a classic script in a fresh context with a fake `self`, and returns what it did. */
function runWorker(code: string) {
  const handlers = new Map<string, (event: unknown) => void>();
  const precached = new Map<string, string[]>();
  const notifications: FakeNotification[] = [];
  const self = {
    location: { origin: ORIGIN },
    addEventListener: (type: string, handler: (event: unknown) => void) => {
      handlers.set(type, handler);
    },
    caches: {
      open: (name: string) =>
        Promise.resolve({
          addAll: (requests: Request[]) => {
            precached.set(
              name,
              requests.map((r) => new URL(r.url).pathname),
            );
            return Promise.resolve();
          },
        }),
      keys: () => Promise.resolve([...precached.keys()]),
      delete: (name: string) => Promise.resolve(precached.delete(name)),
    },
    clients: { claim: () => Promise.resolve() },
    registration: {
      showNotification: (title: string, options?: FakeNotification['options']) => {
        notifications.push({ title, options });
        return Promise.resolve();
      },
    },
    fetch: () => Promise.reject(new Error('the fake scope has no network')),
    skipWaiting: () => Promise.resolve(),
  };
  // A classic script: `import` or `export` at the top level would be a SyntaxError here.
  new vm.Script(code, { filename: SW_FILE }).runInNewContext({
    self,
    URL,
    Request,
    Response,
    setTimeout,
    clearTimeout,
  });

  const fire = async (type: string, event: object = {}): Promise<void> => {
    const handler = handlers.get(type);
    if (!handler) throw new Error(`sw.js registered no ${type} handler`);
    const tasks: Promise<unknown>[] = [];
    handler({
      ...event,
      waitUntil: (task: Promise<unknown>) => {
        tasks.push(task);
      },
    });
    await Promise.all(tasks);
  };
  return { handlers, precached, notifications, fire };
}

describe('readPushStrings', () => {
  let dir = '';
  const catalog = async (content: string): Promise<string> => {
    const file = path.join(dir, 'catalog.json');
    await writeFile(file, content);
    return file;
  };

  beforeAll(async () => {
    dir = await mkdtemp(path.join(tmpdir(), 'isshoni-sw-catalog-'));
  });
  afterAll(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  it('returns the push section, nested groups included', async () => {
    const file = await catalog(JSON.stringify({ common: { reload: 'Reload' }, push: { a: 'A', b: { c: 'C' } } }));
    await expect(readPushStrings(file)).resolves.toEqual({ a: 'A', b: { c: 'C' } });
  });

  it('returns {} for a catalog without a push section', async () => {
    await expect(readPushStrings(await catalog(JSON.stringify({ common: {} })))).resolves.toEqual({});
  });

  it.each([
    ['a number', { push: { count: 3 } }],
    ['an array', { push: { list: ['a'] } }],
    ['null', { push: { nothing: null } }],
    ['a push section that is a string', { push: 'text' }],
  ])('fails the build for %s in the push section', async (_name, content) => {
    await expect(readPushStrings(await catalog(JSON.stringify(content)))).rejects.toThrow(/"push" section/);
  });

  it('fails the build for a catalog that is not an object, or is missing', async () => {
    await expect(readPushStrings(await catalog('[]'))).rejects.toThrow(/not a JSON object/);
    await expect(readPushStrings(path.join(dir, 'missing.json'))).rejects.toThrow(/ENOENT/);
  });

  it('reads the generic notification texts from the real catalog', async () => {
    const push = await readPushStrings(CATALOG);
    expect(push['generic']).toEqual({ title: expect.any(String) as string, body: expect.any(String) as string });
  });
});

describe('swDefine and swBuildConfig', () => {
  it('defines the three constants as JSON text', () => {
    const define = swDefine({ urls: ['/', '/assets/a.js'], version: '0123456789ab' }, { generic: { title: 'T' } });
    expect(define).toEqual({
      __SHELL__: '["/","/assets/a.js"]',
      __SHELL_VERSION__: '"0123456789ab"',
      __PUSH_STRINGS__: '{"generic":{"title":"T"}}',
    });
  });

  it('builds one IIFE named sw.js into dist/ without emptying it or copying public/ again', () => {
    const config = swBuildConfig({
      root: '/app',
      entry: '/app/src/sw/sw.ts',
      outDir: '/app/dist',
      define: {},
      target: ['es2022'],
      minify: true,
      mode: 'production',
      logLevel: 'silent',
    });
    expect(SW_FILE).toBe('sw.js');
    expect(config.configFile).toBe(false);
    expect(config.publicDir).toBe(false);
    expect(config.build).toMatchObject({
      outDir: '/app/dist',
      emptyOutDir: false,
      copyPublicDir: false,
      sourcemap: false,
      target: ['es2022'],
      rolldownOptions: {
        input: '/app/src/sw/sw.ts',
        output: { format: 'iife', entryFileNames: 'sw.js', codeSplitting: false },
      },
    });
  });
});

describe('a fixture build with the service-worker plugin', () => {
  let root = '';
  let dist = '';

  const buildFixture = async (version: string) => {
    await build({
      root,
      configFile: false,
      logLevel: 'silent',
      publicDir: PUBLIC_DIR,
      plugins: [
        cleanPlugin(),
        swPlugin({ entry: SW_ENTRY, catalog: path.join(root, 'catalog.json') }),
        versionPlugin({ version }),
        compressPlugin(),
      ],
      build: { outDir: 'dist', emptyOutDir: false },
    });
  };
  const readVersion = async () =>
    JSON.parse(await readFile(path.join(dist, 'version.json'), 'utf8')) as { version: string; shell: string };
  const readWorker = () => readFile(path.join(dist, SW_FILE), 'utf8');

  beforeAll(async () => {
    root = await mkdtemp(path.join(tmpdir(), 'isshoni-sw-build-'));
    dist = path.join(root, 'dist');
    await mkdir(path.join(root, 'src'), { recursive: true });
    await mkdir(dist, { recursive: true });
    await writeFile(path.join(dist, '.gitkeep'), '');
    await writeFile(path.join(dist, SW_FILE), '/* the previous build */');
    await writeFile(
      path.join(root, 'index.html'),
      '<!doctype html><html><head><title>t</title><link rel="manifest" href="/manifest.webmanifest"></head>' +
        '<body><div id="root"></div><script src="/boot-check.js"></script>' +
        '<script type="module" src="/src/main.js"></script></body></html>\n',
    );
    await writeFile(path.join(root, 'src', 'main.css'), 'body { color: red; }\n');
    await writeFile(
      path.join(root, 'src', 'main.js'),
      `import './main.css';\nimport { shared } from './shared.js';\ndocument.body.dataset.text = shared;\n` +
        `document.body.onclick = () => import('./lazy.js').then((m) => m.run());\n`,
    );
    await writeFile(path.join(root, 'src', 'shared.js'), `export const shared = 'shared v1';\n`);
    await writeFile(path.join(root, 'src', 'lazy.js'), `export function run() { return 'lazy'; }\n`);
    await writeFile(
      path.join(root, 'catalog.json'),
      JSON.stringify({ push: { generic: { title: 'Fixture title', body: 'Fixture body' } } }),
    );
    await buildFixture('1.2.3-ci.7');
  });

  afterAll(async () => {
    await rm(root, { recursive: true, force: true });
  });

  it('emits sw.js and manifest.webmanifest at the root of dist/, next to the rest of the build', async () => {
    expect(await readWorker()).not.toContain('the previous build');
    expect(await exists(path.join(dist, 'manifest.webmanifest'))).toBe(true);
    expect(await exists(path.join(dist, 'index.html'))).toBe(true);
    expect(await exists(path.join(dist, 'version.json'))).toBe(true);
    expect(await exists(path.join(dist, 'boot-check.js'))).toBe(true);
    expect(await exists(path.join(dist, 'icons', 'icon-192.png'))).toBe(true);
    expect(await exists(path.join(dist, '.gitkeep'))).toBe(true);
    // Only sw.js: no second copy under assets/, no source map.
    expect(await exists(path.join(dist, `${SW_FILE}.map`))).toBe(false);
  });

  it('sw.js is one classic script that registers the worker’s handlers', async () => {
    const code = await readWorker();
    expect(code).not.toMatch(/\bimport\s*\(|\bimport\.meta\b|__SHELL__|__SHELL_VERSION__|__PUSH_STRINGS__/);
    const { handlers } = runWorker(code);
    expect([...handlers.keys()].sort()).toEqual([
      'activate',
      'fetch',
      'install',
      'message',
      'notificationclick',
      'push',
      'pushsubscriptionchange',
    ]);
  });

  it('precaches the versioned shell list: the entry with its static imports and CSS, and the public shell files', async () => {
    const { shell } = await readVersion();
    expect(shell).toMatch(/^[0-9a-f]{12}$/);
    const worker = runWorker(await readWorker());
    await worker.fire('install');

    // One cache, named after the same hash that version.json publishes.
    expect([...worker.precached.keys()]).toEqual([`isshoni-shell-${shell}`]);
    const urls = worker.precached.get(`isshoni-shell-${shell}`) ?? [];
    expect(urls[0]).toBe('/');
    expect(urls).toEqual(expect.arrayContaining(['/boot-check.js', '/manifest.webmanifest', '/icons/icon-192.png']));
    const assets = urls.filter((u) => u.startsWith('/assets/'));
    expect(assets.filter((u) => u.endsWith('.js'))).toHaveLength(1); // the entry (shared.js is bundled into it)
    expect(assets.filter((u) => u.endsWith('.css'))).toHaveLength(1);
    expect(urls).toHaveLength(6);
    // Lazy chunks are cached on first use, not at install.
    expect(urls.some((u) => u.includes('lazy'))).toBe(false);
    // Every precached URL is a file of this build.
    for (const url of urls) {
      expect(await exists(path.join(dist, url === '/' ? 'index.html' : url.slice(1))), url).toBe(true);
    }
  });

  it('activate keeps this version’s cache and deletes older shell caches', async () => {
    const { shell } = await readVersion();
    const worker = runWorker(await readWorker());
    await worker.fire('install');
    worker.precached.set('isshoni-shell-000000000000', ['/']);
    worker.precached.set('not-ours', ['/']);
    await worker.fire('activate');
    expect([...worker.precached.keys()].sort()).toEqual([`isshoni-shell-${shell}`, 'not-ours'].sort());
  });

  it('renders notifications from the catalog’s push section', async () => {
    const worker = runWorker(await readWorker());
    await worker.fire('push', { data: null });
    expect(worker.notifications).toHaveLength(1);
    expect(worker.notifications[0]?.title).toBe('Fixture title');
    expect(worker.notifications[0]?.options).toMatchObject({ body: 'Fixture body', icon: '/icons/icon-192.png' });
  });

  it('gets its .br and .gz siblings: the compress plugin runs after the worker is built', async () => {
    const original = await readFile(path.join(dist, SW_FILE));
    expect(original.length).toBeGreaterThanOrEqual(1024);
    expect(zlib.brotliDecompressSync(await readFile(path.join(dist, `${SW_FILE}.br`)))).toEqual(original);
    expect(zlib.gunzipSync(await readFile(path.join(dist, `${SW_FILE}.gz`)))).toEqual(original);
  });

  it('a change in a shell file gives a new shell version and a different sw.js; no change gives the same bytes', async () => {
    const before = { version: await readVersion(), worker: await readWorker() };

    await buildFixture('1.2.3-ci.7');
    expect(await readWorker()).toBe(before.worker);

    await writeFile(path.join(root, 'src', 'shared.js'), `export const shared = 'shared v2';\n`);
    await buildFixture('1.2.3-ci.8');
    const after = { version: await readVersion(), worker: await readWorker() };
    expect(after.version.shell).not.toBe(before.version.shell);
    expect(after.worker).not.toBe(before.worker);
    const worker = runWorker(after.worker);
    await worker.fire('install');
    expect([...worker.precached.keys()]).toEqual([`isshoni-shell-${after.version.shell}`]);
  });
});

describe('the manifest and the icons (05 §16.1)', () => {
  interface ManifestIcon {
    src: string;
    sizes: string;
    type: string;
    purpose?: string;
  }

  /** Width, height and colour type of a PNG, from its IHDR chunk. */
  const pngHeader = async (file: string) => {
    const data = await readFile(file);
    expect(data.subarray(0, 8)).toEqual(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]));
    expect(data.subarray(12, 16).toString('ascii')).toBe('IHDR');
    return { width: data.readUInt32BE(16), height: data.readUInt32BE(20), colourType: data.readUInt8(25) };
  };
  const RGB = 2;
  const RGBA = 6;

  it('the manifest is the one 05 §16.1 gives', async () => {
    const manifest = JSON.parse(await readFile(path.join(PUBLIC_DIR, 'manifest.webmanifest'), 'utf8')) as unknown;
    expect(manifest).toEqual({
      id: '/',
      name: 'isshoni',
      short_name: 'isshoni',
      description: "Watch your friends' screens together",
      start_url: '/',
      scope: '/',
      display: 'standalone',
      orientation: 'any',
      background_color: '#101014',
      theme_color: '#101014',
      icons: [
        { src: '/icons/icon-192.png', sizes: '192x192', type: 'image/png' },
        { src: '/icons/icon-512.png', sizes: '512x512', type: 'image/png' },
        { src: '/icons/maskable-512.png', sizes: '512x512', type: 'image/png', purpose: 'maskable' },
        { src: '/icons/icon.svg', sizes: 'any', type: 'image/svg+xml' },
      ],
    });
  });

  it('every PNG icon of the manifest exists with the size it states', async () => {
    const manifest = JSON.parse(await readFile(path.join(PUBLIC_DIR, 'manifest.webmanifest'), 'utf8')) as {
      icons: ManifestIcon[];
    };
    for (const icon of manifest.icons.filter((i) => i.type === 'image/png')) {
      const { width, height } = await pngHeader(path.join(PUBLIC_DIR, icon.src));
      expect(`${String(width)}x${String(height)}`, icon.src).toBe(icon.sizes);
    }
  });

  it('the maskable and the Apple touch icon have no alpha channel; the badge has one', async () => {
    // iOS paints transparency black and Android masks the maskable icon: both must be full-bleed.
    expect(await pngHeader(path.join(PUBLIC_DIR, 'icons', 'maskable-512.png'))).toEqual({
      width: 512,
      height: 512,
      colourType: RGB,
    });
    expect(await pngHeader(path.join(PUBLIC_DIR, 'icons', 'apple-touch-icon.png'))).toEqual({
      width: 180,
      height: 180,
      colourType: RGB,
    });
    // Android draws only the alpha channel of a notification badge.
    expect(await pngHeader(path.join(PUBLIC_DIR, 'icons', 'badge-72.png'))).toEqual({
      width: 72,
      height: 72,
      colourType: RGBA,
    });
  });

  it('icon.svg is a plain drawing: no styles, scripts or outside references (04’s CSP for files)', async () => {
    const svg = await readFile(path.join(PUBLIC_DIR, 'icons', 'icon.svg'), 'utf8');
    const drawing = svg.replace(/<!--[\s\S]*?-->/g, '');
    expect(drawing).toMatch(/^\s*<svg xmlns="http:\/\/www\.w3\.org\/2000\/svg" viewBox="0 0 512 512">/);
    expect(drawing).not.toMatch(/<style|<script|\sstyle=|href=|url\(|<image|<foreignObject|\son\w+=/i);
  });

  it('index.html links the manifest and the icons that exist', async () => {
    const html = await readFile(path.join(WEB_ROOT, 'index.html'), 'utf8');
    const hrefs = [...html.matchAll(/<link rel="(?:manifest|icon|apple-touch-icon)" href="([^"]+)"/g)].map((m) => m[1]);
    expect(hrefs).toEqual(['/manifest.webmanifest', '/icons/icon.svg', '/icons/apple-touch-icon.png']);
    for (const href of hrefs) {
      expect(await exists(path.join(PUBLIC_DIR, href ?? 'missing')), href).toBe(true);
    }
  });
});
