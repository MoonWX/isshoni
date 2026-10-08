// @vitest-environment node
// scripts/check-i18n.mjs and the lazy namespaces (05 §16.5): the catalog is en.json plus i18n/lazy/<ns>.en.json, a
// missing key fails the check whichever file it belongs in, and rule 5: a lazy namespace is loaded before a key of
// it is asked for, and it is not in the main bundle.
import { execFile } from 'node:child_process';
import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  checkI18n,
  checkLazyNamespaces,
  ENTRY_MODULE,
  importTargets,
  lazyModule,
  readCatalog,
  scanSource,
  scanTree,
} from '../../scripts/check-i18n.mjs';

const SCRIPT = fileURLToPath(new URL('../../scripts/check-i18n.mjs', import.meta.url));
const WEB_SRC = fileURLToPath(new URL('..', import.meta.url));
const EN_JSON = path.join(WEB_SRC, 'i18n', 'en.json');
const run = promisify(execFile);

type Imports = Parameters<typeof checkLazyNamespaces>[0]['imports'];
type Uses = Parameters<typeof checkLazyNamespaces>[0]['uses'];

/** A src tree in memory: its files' imports and literal keys, as scanTree gives them. */
function scanFiles(files: Record<string, string>): { imports: Imports; uses: Uses } {
  const imports = new Map<string, ReturnType<typeof scanSource>['imports']>();
  const uses: Uses[number][] = [];
  for (const [module, text] of Object.entries(files)) {
    const scan = scanSource(module, text);
    imports.set(module, scan.imports);
    for (const use of scan.keys) uses.push({ ...use, module });
  }
  return { imports, uses };
}

const ADMIN = { name: 'admin', file: 'i18n/lazy/admin.en.json' };

/**
 * An app with both kinds of page folder: a room page that is imported eagerly (as this app had it before the room
 * became a lazy folder too), and a lazy admin folder that loads its namespace in its entry module.
 */
const APP = {
  'main.tsx': "import { startApp } from './app/boot';\nstartApp();\n",
  'app/boot.tsx':
    "import { routes } from './router';\nimport { initI18n } from '../i18n';\nexport const startApp = () => [initI18n(), routes];\n",
  'app/router.tsx': [
    "const lazy = import.meta.glob(['../admin/index.{ts,tsx}', '../account/index.{ts,tsx}']);",
    "const eager = import.meta.glob('../rooms/index.{ts,tsx}', { eager: true });",
    'export const routes = { lazy, eager };',
    '',
  ].join('\n'),
  'i18n/index.ts': 'export const initI18n = () => undefined;\nexport const addMessages = (m: unknown) => m;\n',
  'i18n/lazy/admin.ts':
    "import { addMessages } from '../index';\nimport messages from './admin.en.json';\naddMessages(messages);\n",
  'rooms/index.ts': "export { RoomPage } from './RoomPage';\n",
  'rooms/RoomPage.tsx': "export const RoomPage = () => t('room.title');\n",
  'admin/index.ts': "import '../i18n/lazy/admin';\n\nexport { UsersPage } from './UsersPage';\n",
  'admin/UsersPage.tsx':
    "import { When } from './When';\nexport const UsersPage = () => [t('admin.users.title'), When];\n",
  'admin/When.tsx': "export const When = () => t('admin.never');\n",
};

describe('scanSource: imports', () => {
  it('finds what a file loads, and whether before its own code or later', () => {
    const src = [
      "import a from './a';",
      "import { type B } from '../b/index';",
      "import type { C } from './c';",
      "import './d.css';",
      "export { e } from './e';",
      "export * from './f';",
      "export type { G } from './g';",
      "const h = () => import('./h');",
      "type I = typeof import('./i');",
      "const j = import.meta.glob(['../j/index.{ts,tsx}', '!../j/skip.ts']);",
      "const k = import.meta.glob<Mod>('./k/*.ts', { import: 'default', eager: true });",
      'const l = import(dynamicName);',
      "import react from 'react';",
    ].join('\n');
    expect(scanSource('x/file.ts', src).imports).toEqual([
      { spec: './a', eager: true, glob: false },
      // Its specifiers are types, but the statement stays (verbatimModuleSyntax): the module is loaded.
      { spec: '../b/index', eager: true, glob: false },
      { spec: './d.css', eager: true, glob: false },
      { spec: './e', eager: true, glob: false },
      { spec: './f', eager: true, glob: false },
      { spec: './h', eager: false, glob: false },
      { spec: '../j/index.{ts,tsx}', eager: false, glob: true },
      { spec: './k/*.ts', eager: true, glob: true },
      { spec: 'react', eager: true, glob: false },
    ]);
  });
});

describe('importTargets', () => {
  const files = new Set([
    'main.tsx',
    'app/router.tsx',
    'app/screens/index.tsx',
    'admin/index.ts',
    'admin/UsersPage.tsx',
    'rooms/index.tsx',
    'i18n/index.ts',
    'i18n/lazy/admin.ts',
    'i18n/lazy/setup.ts',
  ]);
  const targets = (from: string, spec: string, glob = false) => importTargets(from, { spec, eager: true, glob }, files);

  it.each([
    ['app/router.tsx', '../admin/UsersPage', ['admin/UsersPage.tsx']],
    ['app/router.tsx', '../admin', ['admin/index.ts']],
    ['app/router.tsx', '../rooms/index', ['rooms/index.tsx']],
    ['app/router.tsx', './screens', ['app/screens/index.tsx']],
    ['admin/index.ts', '../i18n/lazy/admin', ['i18n/lazy/admin.ts']],
    ['i18n/lazy/admin.ts', '../index', ['i18n/index.ts']],
    ['main.tsx', './app/router.tsx', ['app/router.tsx']],
    ['main.tsx', './app/router?worker', ['app/router.tsx']],
    // Not source files of this tree: a package, a stylesheet, a catalog, a path outside the src dir.
    ['main.tsx', 'react', []],
    ['main.tsx', './ui/global.css', []],
    ['i18n/lazy/admin.ts', './admin.en.json', []],
    ['main.tsx', '../scripts/check-i18n.mjs', []],
  ])('%s imports %s → %j', (from, spec, want) => {
    expect(targets(from, spec)).toEqual(want);
  });

  it.each([
    ['app/router.tsx', '../admin/index.{ts,tsx}', ['admin/index.ts']],
    ['app/router.tsx', '../rooms/index.{ts,tsx}', ['rooms/index.tsx']],
    ['app/router.tsx', '../nope/index.{ts,tsx}', []],
    ['i18n/index.ts', './lazy/*.ts', ['i18n/lazy/admin.ts', 'i18n/lazy/setup.ts']],
    ['i18n/index.ts', './lazy/*.en.json', []],
    ['main.tsx', './*/index.ts', ['admin/index.ts', 'i18n/index.ts']],
    ['main.tsx', './**/index.ts?', ['app/screens/index.tsx', 'rooms/index.tsx']],
    ['main.tsx', './**/a*.ts', ['i18n/lazy/admin.ts']],
  ])('%s globs %s → %j', (from, pattern, want) => {
    expect(targets(from, pattern, true).sort()).toEqual(want);
  });
});

describe('checkLazyNamespaces (rule 5)', () => {
  const check = (files: Record<string, string>, lazy = [ADMIN]) =>
    checkLazyNamespaces({ lazy, ...scanFiles(files) }).map(
      (p) => `${p.file}:${String(p.line)}:${String(p.column)}: ${p.message}`,
    );

  it('passes when the folder that uses a lazy namespace loads it in its entry module', () => {
    expect(check(APP)).toEqual([]);
  });

  it('has nothing to check without lazy namespaces, entry module or not', () => {
    expect(check(APP, [])).toEqual([]);
    expect(check({ 'app/Page.tsx': "t('admin.title');\n" }, [])).toEqual([]);
  });

  it("fails for every key of the namespace when the folder's entry module doesn't load it", () => {
    const got = check({ ...APP, 'admin/index.ts': "export { UsersPage } from './UsersPage';\n" });
    expect(got).toEqual([
      'admin/UsersPage.tsx:2:35: message key "admin.users.title" is in the lazy namespace "admin", which nothing ' +
        'loads on the way main.tsx → app/boot.tsx → app/router.tsx → admin/index.ts → admin/UsersPage.tsx: ' +
        'the entry module of the folder that uses it must import i18n/lazy/admin.ts',
      'admin/When.tsx:1:29: message key "admin.never" is in the lazy namespace "admin", which nothing loads on ' +
        'the way main.tsx → app/boot.tsx → app/router.tsx → admin/index.ts → admin/UsersPage.tsx → admin/When.tsx: ' +
        'the entry module of the folder that uses it must import i18n/lazy/admin.ts',
    ]);
  });

  it('fails when code of the main bundle uses a key of a lazy namespace', () => {
    const got = check({ ...APP, 'rooms/RoomPage.tsx': "export const RoomPage = () => t('admin.nav.users');\n" });
    expect(got).toHaveLength(1);
    expect(got[0]).toMatch(
      /^rooms\/RoomPage\.tsx:1:33: message key "admin\.nav\.users" is in the lazy namespace "admin", which nothing loads on the way main\.tsx → app\/boot\.tsx → app\/router\.tsx → rooms\/index\.ts → rooms\/RoomPage\.tsx: /,
    );
  });

  it('fails when a file of the folder is loaded past the entry module that loads the namespace', () => {
    // The room page opens one of the admin folder's files by itself: admin/index.ts never runs on that way.
    const got = check({
      ...APP,
      'rooms/RoomPage.tsx': "export const RoomPage = () => [t('room.title'), import('../admin/When')];\n",
    });
    expect(got).toHaveLength(1);
    expect(got[0]).toMatch(
      /^admin\/When\.tsx:1:29: message key "admin\.never" .* on the way main\.tsx → app\/boot\.tsx → app\/router\.tsx → rooms\/index\.ts → rooms\/RoomPage\.tsx → admin\/When\.tsx: /,
    );
  });

  it('passes when that file loads the namespace itself, or through what it imports', () => {
    const viaRoom = "export const RoomPage = () => [t('room.title'), import('../admin/When')];\n";
    expect(
      check({
        ...APP,
        'rooms/RoomPage.tsx': viaRoom,
        'admin/When.tsx': "import '../i18n/lazy/admin';\nexport const When = () => t('admin.never');\n",
      }),
    ).toEqual([]);
    expect(
      check({
        ...APP,
        'rooms/RoomPage.tsx': viaRoom,
        'admin/When.tsx': "import { texts } from './texts';\nexport const When = () => [t('admin.never'), texts];\n",
        'admin/texts.ts': "export * from '../i18n/lazy/admin';\nexport const texts = 1;\n",
      }),
    ).toEqual([]);
  });

  it('does not count an import() of the loading module: it runs after the code that asked for it', () => {
    const got = check({
      ...APP,
      'admin/index.ts': "void import('../i18n/lazy/admin');\n\nexport { UsersPage } from './UsersPage';\n",
    });
    expect(got).toHaveLength(2);
  });

  it('fails when the namespace is in the main bundle after all', () => {
    const got = check({
      ...APP,
      'rooms/index.ts': "import '../i18n/lazy/admin';\nexport { RoomPage } from './RoomPage';\n",
    });
    expect(got).toEqual([
      'main.tsx:1:1: the lazy namespace "admin" is in the main bundle: ' +
        'main.tsx → app/boot.tsx → app/router.tsx → rooms/index.ts → i18n/lazy/admin.ts',
    ]);
  });

  it('fails for a lazy file that no module adds to the catalog', () => {
    const files: Record<string, string> = { ...APP, 'admin/index.ts': "export { UsersPage } from './UsersPage';\n" };
    delete files['i18n/lazy/admin.ts'];
    expect(check(files)).toEqual([
      'i18n/lazy/admin.en.json:1:1: no i18n/lazy/admin.ts adds the lazy namespace "admin" to the catalog ' +
        '(see i18n/index.ts)',
    ]);
  });

  it('fails without the entry module: nothing can be followed', () => {
    const files: Record<string, string> = { ...APP };
    delete files['main.tsx'];
    expect(check(files)).toEqual([`main.tsx:1:1: no main.tsx: can't tell what runs before admin is loaded`]);
    expect(ENTRY_MODULE).toBe('main.tsx');
  });

  it('checks each namespace by itself, and leaves the keys of the main bundle alone', () => {
    const account = { name: 'account', file: 'i18n/lazy/account.en.json' };
    const files = {
      ...APP,
      'i18n/lazy/account.ts': "import { addMessages } from '../index';\naddMessages({});\n",
      // The account folder uses its own namespace, and one of admin's that it doesn't load.
      'account/index.ts': "import '../i18n/lazy/account';\nexport { AccountPage } from './AccountPage';\n",
      'account/AccountPage.tsx':
        "export const AccountPage = () => [t('account.title'), t('admin.never'), t('auth.x')];\n",
    };
    const got = check(files, [account, ADMIN]);
    expect(got).toHaveLength(1);
    expect(got[0]).toMatch(
      /^account\/AccountPage\.tsx:1:57: message key "admin\.never" is in the lazy namespace "admin"/,
    );
    expect(lazyModule('account')).toBe('i18n/lazy/account.ts');
  });
});

describe('check-i18n.mjs with lazy namespace files', () => {
  let dir = '';
  let srcDir = '';
  let catalogPath = '';

  const write = async (rel: string, content: string | object) => {
    const file = path.join(srcDir, rel);
    await mkdir(path.dirname(file), { recursive: true });
    await writeFile(file, typeof content === 'string' ? content : JSON.stringify(content));
  };
  const cli = () => run(process.execPath, [SCRIPT, '--src', srcDir, '--catalog', catalogPath]);
  const failure = async () => {
    const failed = cli();
    await expect(failed).rejects.toMatchObject({ code: 1 });
    return ((await failed.catch((e: unknown) => e)) as { stderr: string }).stderr;
  };

  beforeEach(async () => {
    dir = await mkdtemp(path.join(tmpdir(), 'isshoni-check-i18n-lazy-'));
    srcDir = path.join(dir, 'src');
    catalogPath = path.join(srcDir, 'i18n', 'en.json');
    for (const [rel, text] of Object.entries(APP)) await write(rel, text);
    await write('i18n/en.json', { room: { title: 'Room' } });
    await write('i18n/lazy/admin.en.json', { admin: { never: 'Never', users: { title: 'Users' } } });
  });

  afterEach(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  it('reads the main file and the lazy files as one catalog', async () => {
    const { catalog, lazy, fileOf, problems } = await readCatalog({ srcDir, catalogPath });
    expect(problems).toEqual([]);
    expect(catalog).toEqual({ room: { title: 'Room' }, admin: { never: 'Never', users: { title: 'Users' } } });
    expect(lazy.map((ns) => ns.name)).toEqual(['admin']);
    expect(fileOf('room.title')).toMatch(/i18n[\\/]en\.json$/);
    expect(fileOf('admin.users.title')).toMatch(/i18n[\\/]lazy[\\/]admin\.en\.json$/);
    expect(fileOf('admin')).toMatch(/admin\.en\.json$/);
    // A namespace whose name merely starts like a lazy one is the main file's.
    expect(fileOf('administrator.title')).toMatch(/i18n[\\/]en\.json$/);
  });

  it('passes, and says which namespaces are lazy', async () => {
    const result = await checkI18n({ srcDir, catalogPath });
    expect(result).toMatchObject({ problems: [], keysUsed: 3, lazyNamespaces: ['admin'] });
    const { stdout } = await cli();
    expect(stdout).toMatch(/^check:i18n: lazy namespaces, each loaded before its keys are used: admin\n/);
    expect(stdout).toMatch(/check:i18n: ok \(3 message keys in 10 files\)/);
  });

  it('fails for a key that is missing from a lazy file, and names that file', async () => {
    await write('i18n/lazy/admin.en.json', { admin: { users: { title: 'Users' } } });
    const stderr = await failure();
    expect(stderr).toMatch(
      /When\.tsx:1:29: message key "admin\.never" missing in \S*i18n[\\/]lazy[\\/]admin\.en\.json\n/,
    );
    expect(stderr).toMatch(/check:i18n: 1 problem\(s\)/);
  });

  it('fails for a key that is missing from the main file, and names that file', async () => {
    await write('i18n/en.json', { room: {} });
    const stderr = await failure();
    expect(stderr).toMatch(/RoomPage\.tsx:1:33: message key "room\.title" missing in \S*i18n[\\/]en\.json\n/);
    expect(stderr).toMatch(/check:i18n: 1 problem\(s\)/);
  });

  it('fails when a folder uses a lazy namespace that its entry module does not load', async () => {
    await write('admin/index.ts', "export { UsersPage } from './UsersPage';\n");
    const stderr = await failure();
    expect(stderr).toMatch(/UsersPage\.tsx:2:35: message key "admin\.users\.title" is in the lazy namespace "admin"/);
    expect(stderr).toMatch(/When\.tsx:1:29: message key "admin\.never" is in the lazy namespace "admin"/);
    expect(stderr).toMatch(/check:i18n: 2 problem\(s\)/);
  });

  it.each([
    ['two namespaces', { admin: { never: 'Never', users: { title: 'Users' } }, more: {} }],
    ['another name', { administration: { never: 'Never' } }],
    ['no namespace', {}],
    ['not an object', ['admin']],
  ])('fails for a lazy file that is not just its namespace: %s', async (_name, content) => {
    await write('i18n/lazy/admin.en.json', content);
    const stderr = await failure();
    expect(stderr).toMatch(
      /i18n[\\/]lazy[\\/]admin\.en\.json:1:1: a lazy namespace file holds its own namespace and nothing else: \{"admin": \{…\}\}\n/,
    );
    // Its keys are then missing, each reported where it is used.
    expect(stderr).toMatch(/message key "admin\.never" missing in /);
  });

  it('fails when the main file has a lazy namespace too', async () => {
    await write('i18n/en.json', { room: { title: 'Room' }, admin: { never: 'Never', users: { title: 'Users' } } });
    const stderr = await failure();
    expect(stderr).toMatch(/admin\.en\.json:1:1: the namespace "admin" is in \S*i18n[\\/]en\.json too: keep it in one/);
    expect(stderr).toMatch(/check:i18n: 1 problem\(s\)/);
  });

  it('reads only the lazy files of the main file’s language', async () => {
    await write('i18n/lazy/admin.de.json', { nope: 1 });
    await write('i18n/lazy/README.md', 'not a catalog');
    expect((await checkI18n({ srcDir, catalogPath })).problems).toEqual([]);
  });
});

describe('check-i18n.mjs on this package', () => {
  it('passes, with the lazy namespaces of i18n/lazy/', async () => {
    const res = await checkI18n({ srcDir: WEB_SRC, catalogPath: EN_JSON });
    expect(res.problems).toEqual([]);
    expect(res.lazyNamespaces).toEqual(['account', 'admin', 'setup']);
  });

  it('every lazy namespace is used, and only by folders that load it', async () => {
    const [{ lazy }, tree] = await Promise.all([
      readCatalog({ srcDir: WEB_SRC, catalogPath: EN_JSON }),
      scanTree(WEB_SRC),
    ]);
    const folders = (ns: string) =>
      [...new Set(tree.uses.filter((u) => u.key.startsWith(`${ns}.`)).map((u) => u.module.split('/')[0]))].sort();
    expect(Object.fromEntries(lazy.map((ns) => [ns.name, folders(ns.name)]))).toEqual({
      account: ['account', 'download'],
      admin: ['admin'],
      setup: ['setup'],
    });
    expect(checkLazyNamespaces({ lazy, imports: tree.imports, uses: tree.uses })).toEqual([]);
  });

  it.each([
    ['admin/index.ts', 'admin'],
    ['account/index.ts', 'account'],
    ['download/index.ts', 'account'],
    ['setup/index.ts', 'setup'],
  ])('fails when %s no longer loads the %s namespace', async (entry, ns) => {
    const [{ lazy }, tree] = await Promise.all([
      readCatalog({ srcDir: WEB_SRC, catalogPath: EN_JSON }),
      scanTree(WEB_SRC),
    ]);
    const imports = new Map(tree.imports);
    const own = imports.get(entry) ?? [];
    const kept = own.filter((i) => !i.spec.endsWith(`/i18n/lazy/${ns}`));
    expect(kept).toHaveLength(own.length - 1);
    imports.set(entry, kept);
    const problems = checkLazyNamespaces({ lazy, imports, uses: tree.uses });
    expect(problems.length).toBeGreaterThan(0);
    // Only that folder's own files: another folder that uses the namespace still loads it.
    const folder = path.dirname(tree.shown(entry));
    for (const p of problems) {
      expect(path.dirname(p.file)).toBe(folder);
      expect(p.message).toContain(`is in the lazy namespace "${ns}", which nothing loads on the way main.tsx → `);
    }
  });

  it('fails when a key is removed from any of the catalog files, in a copy of the sources', async () => {
    const dir = await mkdtemp(path.join(tmpdir(), 'isshoni-check-i18n-files-'));
    try {
      const srcDir = path.join(dir, 'src');
      await cp(WEB_SRC, srcDir, { recursive: true });
      /** Removes the message at a key from a catalog file of the copy. */
      const remove = async (rel: string, key: string) => {
        const file = path.join(srcDir, rel);
        const text = await readFile(file, 'utf8');
        let found = false;
        // JSON.parse visits every member, innermost first, with the path of none: walk down to the key instead.
        const without = (node: unknown, parts: string[]): unknown => {
          if (typeof node !== 'object' || node === null) return node;
          const [head, ...rest] = parts;
          return Object.fromEntries(
            Object.entries(node).flatMap(([k, v]): [string, unknown][] => {
              if (k !== head) return [[k, v]];
              if (rest.length > 0) return [[k, without(v, rest)]];
              found = typeof v === 'string';
              return [];
            }),
          );
        };
        await writeFile(file, JSON.stringify(without(JSON.parse(text), key.split('.'))));
        expect(found).toBe(true);
      };
      await remove('i18n/en.json', 'common.appName');
      await remove('i18n/lazy/admin.en.json', 'admin.title');
      await remove('i18n/lazy/account.en.json', 'account.back');
      await remove('i18n/lazy/setup.en.json', 'setup.steps.label');

      const failed = run(process.execPath, [
        SCRIPT,
        '--src',
        srcDir,
        '--catalog',
        path.join(srcDir, 'i18n', 'en.json'),
      ]);
      await expect(failed).rejects.toMatchObject({ code: 1 });
      const { stderr } = (await failed.catch((e: unknown) => e)) as { stderr: string };
      const missing = [...stderr.matchAll(/message key "([\w.]+)" missing in \S*?i18n[\\/]([\w./\\]+)\n/g)].map(
        (m) => `${m[1] ?? ''} in ${(m[2] ?? '').replaceAll('\\', '/')}`,
      );
      expect([...new Set(missing)].sort()).toEqual([
        'account.back in lazy/account.en.json',
        'admin.title in lazy/admin.en.json',
        'common.appName in en.json',
        'setup.steps.label in lazy/setup.en.json',
      ]);
      // Nothing but those keys, each once per use.
      expect(stderr).toMatch(new RegExp(`check:i18n: ${String(missing.length)} problem\\(s\\)`));
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
});
