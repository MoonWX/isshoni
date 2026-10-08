// @vitest-environment node
// What a production build puts where (05 §5, §17.3), on the app itself: built in memory by the real Vite with the
// build options of vite.config.ts (without the plugins that write files), then read chunk by chunk.
//
// "Initial" is check:size's word: the entry chunk and everything it imports statically, which is what the first
// page load downloads. The budget only says how big that may be; these tests say what must not be in it, so that
// a module that is meant to load on demand can't move back unnoticed:
// - the pages of the page folders, the room's among them, and the room's media code (05 §5). The router's module has
//   a branch for Vitest that loads rooms/ with it (ROOMS_IN_TESTS); a build must drop that one too;
// - the share publisher: BrowserSharing, PublisherPC and BrowserShare (platform/browser/displayMedia.ts loads it),
//   which must not ride along with the room's media chunk either: it waits for the first pick;
// - the lazy namespaces of the catalog, which come with the folders that use them (05 §16.5). In Vitest the i18n
//   module loads them all up front; a build must drop that branch (i18n/index.ts).
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import react from '@vitejs/plugin-react';
import { build, type Rolldown } from 'vite';
import { beforeAll, describe, expect, it, vi } from 'vitest';

import config from '../../vite.config.ts';

const WEB_ROOT = fileURLToPath(new URL('../..', import.meta.url));
const SRC = path.join(WEB_ROOT, 'src');

/** The chunks of the build, and the source modules of each (paths under src/, with forward slashes). */
let chunks: Rolldown.OutputChunk[] = [];
const modulesOf = (chunk: Rolldown.OutputChunk): string[] =>
  Object.keys(chunk.modules)
    .filter((id) => id.startsWith(SRC + path.sep))
    .map((id) => path.relative(SRC, id).split(path.sep).join('/'));

const byFile = (file: string): Rolldown.OutputChunk => {
  const chunk = chunks.find((c) => c.fileName === file);
  if (!chunk) throw new Error(`no chunk ${file}`);
  return chunk;
};

/** A chunk and everything it imports statically, as file names. */
function withStaticImports(start: Rolldown.OutputChunk): Set<string> {
  const seen = new Set<string>();
  const visit = (chunk: Rolldown.OutputChunk): void => {
    if (seen.has(chunk.fileName)) return;
    seen.add(chunk.fileName);
    for (const dep of chunk.imports) visit(byFile(dep));
  };
  visit(start);
  return seen;
}

/** The chunks that hold a source module (a module is in one chunk; none when it was dropped from the build). */
const chunksWith = (module: string): string[] =>
  chunks.filter((c) => modulesOf(c).includes(module)).map((c) => c.fileName);

/** What the first page load downloads. */
let initial = new Set<string>();
const initialModules = (): string[] => chunks.filter((c) => initial.has(c.fileName)).flatMap(modulesOf);

/** The chunk that an import() of a source module gets: the one made for that module (a path under src/). */
function chunkOf(module: string): Rolldown.OutputChunk {
  const own = chunks.find((c) => c.isDynamicEntry && c.facadeModuleId === path.join(SRC, ...module.split('/')));
  if (!own) throw new Error(`no chunk for ${module}`);
  return own;
}

/** What the router's import of a page folder downloads: the folder's own chunk and its static imports. */
const folderChunks = (folder: string): Set<string> => withStaticImports(chunkOf(`${folder}/index.ts`));

/** How often the code of the initial chunks has an import() of a chunk. */
function initialImportsOf(chunk: Rolldown.OutputChunk): number {
  const name = path.posix.basename(chunk.fileName).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const call = new RegExp(`\\bimport\\(\\s*[\`'"]\\./${name}[\`'"]\\s*\\)`, 'g');
  return chunks
    .filter((c) => initial.has(c.fileName))
    .reduce((count, c) => count + (c.code.match(call)?.length ?? 0), 0);
}

beforeAll(async () => {
  // Vitest runs with NODE_ENV=test; a production build is what is being checked.
  vi.stubEnv('NODE_ENV', 'production');
  try {
    const out = await build({
      root: WEB_ROOT,
      configFile: false,
      mode: 'production',
      logLevel: 'silent',
      publicDir: false,
      plugins: [react()],
      define: config.define,
      build: { ...config.build, write: false },
    });
    const outputs = Array.isArray(out) ? out : [out];
    chunks = outputs.flatMap((o) => ('output' in o ? o.output : [])).filter((o) => o.type === 'chunk');
  } finally {
    vi.unstubAllEnvs();
  }
  initial = new Set(chunks.filter((c) => c.isEntry).flatMap((c) => [...withStaticImports(c)]));
}, 120_000);

describe('the production build', () => {
  it('has the app shell, the layout and the main catalog in the initial chunks', () => {
    const modules = initialModules();
    expect(modules).toEqual(
      expect.arrayContaining([
        'main.tsx',
        'app/boot.tsx',
        'app/router.tsx',
        'app/layouts/RootLayout.tsx',
        'platform/browser/displayMedia.ts',
        'i18n/index.ts',
        'i18n/en.json',
      ]),
    );
  });

  it.each([
    // The room's folder (rooms/index.ts): the page, the bar of the other pages, the session and its connection.
    'rooms/RoomPage.tsx',
    'rooms/RootRedirect.tsx',
    'rooms/InRoomBar.tsx',
    'rooms/RoomSession.ts',
    'rooms/runtime.ts',
    'protocol/signal-client.ts',
    'auth/LoginPage.tsx',
    'setup/SetupPage.tsx',
    'account/AccountPage.tsx',
    'admin/UsersPage.tsx',
    'download/DownloadPage.tsx',
    'conntest/ConnTestPanel.tsx',
    // The room's media chunk (rooms/media.ts).
    'viewer/SubscriberPC.ts',
    'viewer/ViewerLayout.tsx',
    'share/ShareSheet.tsx',
    'share/SharePanel.tsx',
  ])('loads %s on demand', (module) => {
    const holders = chunksWith(module);
    expect(holders).toHaveLength(1);
    expect(holders.filter((file) => initial.has(file))).toEqual([]);
  });

  it('has nothing of rooms/, viewer/, share/ or conntest/ in the initial chunks but what the picker needs', () => {
    // The whole room, not only the modules named above: a file of these folders that the entry chunk starts to
    // import shows up here. share/presets.ts is the content hint that the picker's provider puts on a picked track
    // (platform/browser/displayMedia.ts); add another file only if the shell needs it before a room is opened.
    const inShell = initialModules().filter((m) => /^(rooms|viewer|share|conntest)\//.test(m));
    expect(inShell.sort()).toEqual(['share/presets.ts']);
  });

  it('the router loads the room by import(), in one place: the branch that Vitest has is not in a build', () => {
    const rooms = chunkOf('rooms/index.ts');
    const [router] = chunksWith('app/router.tsx');
    expect(router).toBeDefined();
    if (router === undefined) return;
    expect(initial.has(router)).toBe(true);
    expect(byFile(router).dynamicImports).toContain(rooms.fileName);
    // The page-folder loader. A second import() of the room would be router.tsx's `await import('../rooms/index')`,
    // which a build must drop: it would fetch the room on every page, and hold the app's start until it is there.
    expect(initialImportsOf(rooms)).toBe(1);
    // A folder that never had such a branch gives the same count: the count finds a loader.
    expect(initialImportsOf(chunkOf('auth/index.ts'))).toBe(1);
  });

  it('the room’s folder comes with one import: the page, the redirect, the bar and the session', () => {
    const loaded = folderChunks('rooms');
    for (const module of [
      'rooms/RoomPage.tsx',
      'rooms/RootRedirect.tsx',
      'rooms/InRoomBar.tsx',
      'rooms/RoomSession.ts',
      'protocol/signal-client.ts',
    ]) {
      expect(chunksWith(module).every((file) => loaded.has(file))).toBe(true);
    }
    // And not the media chunk: the page asks for that as it renders (rooms/loadMedia.ts).
    expect(loaded.has(chunkOf('rooms/media.ts').fileName)).toBe(false);
  });

  it.each(['share/BrowserSharing.ts', 'share/PublisherPC.ts', 'share/BrowserShare.ts'])(
    'keeps the share publisher out of the initial chunks: %s',
    (module) => {
      const holders = chunksWith(module);
      expect(holders).toHaveLength(1);
      expect(holders.filter((file) => initial.has(file))).toEqual([]);
    },
  );

  it('loads the publisher from the provider in the main chunk, by import()', () => {
    const [provider] = chunksWith('platform/browser/displayMedia.ts');
    const [publisher] = chunksWith('share/BrowserSharing.ts');
    expect(provider).toBeDefined();
    expect(publisher).toBeDefined();
    if (provider === undefined || publisher === undefined) return;
    expect(initial.has(provider)).toBe(true);
    expect(byFile(provider).dynamicImports).toContain(publisher);
    // And everything the publisher needs comes with that one import.
    const loaded = withStaticImports(byFile(publisher));
    for (const module of ['share/PublisherPC.ts', 'share/BrowserShare.ts']) {
      expect(chunksWith(module).every((file) => loaded.has(file))).toBe(true);
    }
  });

  it.each(['share/BrowserSharing.ts', 'share/PublisherPC.ts'])(
    'keeps the publisher out of what the room page loads, the folder and the media chunk: %s waits for the first pick',
    (module) => {
      // The media chunk may import BrowserShare (shareUi.ts and SharePanel.tsx read its error classes), but not
      // these two: through share/'s barrel, say, they would load with the stage on every visit to a room.
      const withRoom = new Set([...folderChunks('rooms'), ...withStaticImports(chunkOf('rooms/media.ts'))]);
      const holders = chunksWith(module);
      expect(holders).toHaveLength(1);
      expect(holders.filter((file) => withRoom.has(file))).toEqual([]);
    },
  );

  it.each([
    ['admin', ['admin']],
    ['account', ['account']],
    ['setup', ['setup']],
  ])('keeps the lazy namespace %s out of the initial chunks', (ns) => {
    for (const module of [`i18n/lazy/${ns}.en.json`, `i18n/lazy/${ns}.ts`]) {
      const holders = chunksWith(module);
      expect(holders).toHaveLength(1);
      expect(holders.filter((file) => initial.has(file))).toEqual([]);
    }
  });

  it.each([
    ['admin', 'admin'],
    ['account', 'account'],
    ['download', 'account'],
    ['setup', 'setup'],
  ])("the %s folder's chunk brings the %s namespace with it", (folder, ns) => {
    const loaded = folderChunks(folder);
    for (const module of [`i18n/lazy/${ns}.en.json`, `i18n/lazy/${ns}.ts`]) {
      const holders = chunksWith(module);
      expect(holders).toHaveLength(1);
      expect(holders.every((file) => loaded.has(file))).toBe(true);
    }
  });

  it('gives a page folder only the lazy namespaces it uses', () => {
    const lazyIn = (folder: string): string[] => {
      const loaded = folderChunks(folder);
      return chunks
        .filter((c) => loaded.has(c.fileName))
        .flatMap(modulesOf)
        .filter((m) => /^i18n\/lazy\/\w+\.en\.json$/.test(m))
        .sort();
    };
    expect(lazyIn('auth')).toEqual([]);
    expect(lazyIn('download')).toEqual(['i18n/lazy/account.en.json']);
    expect(lazyIn('admin')).toEqual(['i18n/lazy/admin.en.json']);
  });
});
