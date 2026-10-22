// @vitest-environment node
// scripts/check-i18n.mjs, rule 4 (05 §16.5, W13): a message that no code asks for is a warning at its place in the
// catalog, and the check still passes; a message that the code asks for fails the check once it is removed.
import { execFile } from 'node:child_process';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  checkI18n,
  messagePositions,
  namespacesBuiltWhole,
  READ_WHOLE,
  scanSource,
  unusedMessages,
} from '../../scripts/check-i18n.mjs';

const SCRIPT = fileURLToPath(new URL('../../scripts/check-i18n.mjs', import.meta.url));
const WEB_SRC = fileURLToPath(new URL('..', import.meta.url));
const EN_JSON = path.join(WEB_SRC, 'i18n', 'en.json');
const run = promisify(execFile);

/** What a run printed, whether it passed or failed. */
interface Output {
  code: number;
  stdout: string;
  stderr: string;
}

async function cli(args: string[]): Promise<Output> {
  try {
    const { stdout, stderr } = await run(process.execPath, [SCRIPT, ...args]);
    return { code: 0, stdout, stderr };
  } catch (err) {
    const e = err as { code: number; stdout: string; stderr: string };
    return { code: e.code, stdout: e.stdout, stderr: e.stderr };
  }
}

/** The warnings of a run as "file:line:column key", the file's path from its i18n folder on, else its name. */
function warned(stderr: string): string[] {
  return [...stderr.matchAll(/(\S+):(\d+):(\d+): warning: message "([^"]+)" is not used/g)].map((m) => {
    const file = (m[1] ?? '').replaceAll('\\', '/');
    const shown = /\/i18n\/(.+)$/.exec(file)?.[1] ?? path.posix.basename(file);
    return `${shown}:${m[2] ?? ''}:${m[3] ?? ''} ${m[4] ?? ''}`;
  });
}

describe('unusedMessages', () => {
  const unused = (catalog: object, code: string, required: string[] = []): string[] => {
    const scan = scanSource('a.tsx', code);
    return unusedMessages({
      catalog: catalog as Parameters<typeof unusedMessages>[0]['catalog'],
      keys: scan.keys.map((k) => k.key),
      strings: scan.strings,
      heads: scan.heads,
      required,
    });
  };

  it('counts literal keys, and the base of a plural form', () => {
    const catalog = { room: { title: 'Room', gone: 'Gone' }, viewer: { watching_one: '1', watching_other: 'n' } };
    expect(unused(catalog, "t('room.title'); t('viewer.watching', { count });")).toEqual(['room.gone']);
    expect(unused(catalog, "t('room.title');")).toEqual(['room.gone', 'viewer.watching_one', 'viewer.watching_other']);
  });

  it('counts a key that the code writes out anywhere, also outside t()', () => {
    const catalog = { room: { menu: { leave: 'Leave', stay: 'Stay' } } };
    expect(unused(catalog, "const items = [{ label: 'room.menu.leave' }]; items.map((i) => t(i.label));")).toEqual([
      'room.menu.stay',
    ]);
  });

  it('follows a key built at run time as far as it is written out', () => {
    const catalog = {
      doctor: { part: { firewall: { ufw: 'ufw', firewalld: 'firewalld' }, other: 'Other' } },
      viewer: { title: { window: 'W', tab: 'T' }, x: 'X' },
      common: { a: 'A' },
    };
    // A template's head, and a string that ends in a dot, cover every key they begin.
    expect(unused(catalog, "t(`doctor.part.firewall.${p}`); t('viewer.title.' + kind);")).toEqual([
      'common.a',
      'doctor.part.other',
      'viewer.x',
    ]);
    // A head needs a namespace and a dot: `${ns}.a` and `common${x}` cover nothing, nor does a string without the dot.
    expect(unused(catalog, "t(`${ns}.a`); t(`common${x}`); t('viewer' + '.x');")).toEqual([
      'common.a',
      'doctor.part.firewall.firewalld',
      'doctor.part.firewall.ufw',
      'doctor.part.other',
      'viewer.title.tab',
      'viewer.title.window',
      'viewer.x',
    ]);
  });

  it('takes a string for a head only below a namespace: a lone namespace and a dot is more often something else', () => {
    const catalog = { room: { title: 'Room', gone: 'Gone', menu: { leave: 'Leave', stay: 'Stay' } } };
    // The audit log's action groups ('room.' for room.create …) are no message keys.
    expect(unused(catalog, "const GROUPS = ['room.', 'auth.']; t('room.title');")).toEqual([
      'room.gone',
      'room.menu.leave',
      'room.menu.stay',
    ]);
    expect(unused(catalog, "t('room.title'); t('room.menu.' + item);")).toEqual(['room.gone']);
  });

  it('names the namespaces that a template head covers whole, where it can’t tell what is unused', () => {
    const catalog = { doctor: { a: 'A' }, errors: { x: 'X' }, push: { p: 'P' }, room: { title: 'Room' }, n: 'N' };
    const heads = (code: string): string[] => namespacesBuiltWhole({ catalog, heads: scanSource('a.ts', code).heads });
    expect(heads('t(`errors.${c}`); t(`doctor.${c}`); t(`doctor.${d}.x`); t(`room.menu.${m}`);')).toEqual([
      'doctor',
      'errors',
    ]);
    // Not a namespace of the catalog, a message rather than a namespace, one read whole, and a string are none.
    expect(heads("t(`isshoni.${k}`); t(`n.${k}`); t(`push.${k}`); t('room.' + k);")).toEqual([]);
    expect(unused(catalog, 't(`doctor.${c}`);')).toEqual(['errors.x', 'n', 'room.title']);
  });

  it('counts what rules 2 and 3 require, with what is below it, the namespaces read whole, and $t() nesting', () => {
    expect(READ_WHOLE).toEqual(['push', 'desktop.native']);
    const catalog = {
      errors: { internal: 'Oops', rate_limited: { wait: 'Wait' }, gone_code: 'Old' },
      fix: { firewall: { ufw: 'ufw' } },
      push: { share: { title: 'Started' } },
      common: { appName: 'isshoni', about: 'About $t(common.appName)' },
    };
    expect(
      unused(catalog, "t('common.about');", ['errors.internal', 'errors.rate_limited', 'fix.firewall.ufw']),
    ).toEqual(['errors.gone_code']);
  });
});

describe('messagePositions', () => {
  it('gives the line and column of every message, through nested objects', () => {
    const text = '{\n  "room": {\n    "title": "Room",\n    "menu": { "leave": "Leave" }\n  },\n  "x": "X"\n}\n';
    const at = messagePositions('en.json', text);
    expect(Object.fromEntries([...at].map(([k, p]) => [k, `${p.file}:${String(p.line)}:${String(p.column)}`]))).toEqual(
      {
        'room.title': 'en.json:3:5',
        'room.menu.leave': 'en.json:4:15',
        x: 'en.json:6:3',
      },
    );
  });
});

describe('check-i18n.mjs with unused messages', () => {
  let dir = '';
  let srcDir = '';
  let catalogPath = '';

  const write = async (rel: string, content: string | object): Promise<void> => {
    const file = path.join(srcDir, rel);
    await mkdir(path.dirname(file), { recursive: true });
    await writeFile(file, typeof content === 'string' ? content : JSON.stringify(content, null, 2));
  };

  beforeEach(async () => {
    dir = await mkdtemp(path.join(tmpdir(), 'isshoni-check-i18n-unused-'));
    srcDir = path.join(dir, 'src');
    catalogPath = path.join(srcDir, 'i18n', 'en.json');
    await write('i18n/en.json', { room: { title: 'Room', old: 'Old' }, viewer: { onlyInTests: 'T' } });
    await write('i18n/lazy/admin.en.json', { admin: { title: 'Admin', never: 'Never' } });
    await write('i18n/lazy/admin.ts', "import m from './admin.en.json';\naddMessages(m);\n");
    await write('main.tsx', "import('./admin/index');\nexport const A = () => t('room.title');\n");
    await write('admin/index.ts', "import '../i18n/lazy/admin';\nexport const B = () => t('admin.title');\n");
    // A test may use a key that nothing else uses: it is still unused.
    await write('admin/index.test.ts', "t('viewer.onlyInTests');\n");
  });

  afterEach(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  it('warns about each unused message at its place, in the file it is in, and passes', async () => {
    const result = await checkI18n({ srcDir, catalogPath });
    expect(result.problems).toEqual([]);
    expect(result.warnings.map((w) => w.message)).toEqual([
      'message "room.old" is not used: no code asks for it',
      'message "viewer.onlyInTests" is not used: no code asks for it',
      'message "admin.never" is not used: no code asks for it',
    ]);

    const out = await cli(['--src', srcDir, '--catalog', catalogPath]);
    expect(out.code).toBe(0);
    expect(warned(out.stderr)).toEqual([
      'en.json:4:5 room.old',
      'en.json:7:5 viewer.onlyInTests',
      'lazy/admin.en.json:4:5 admin.never',
    ]);
    expect(out.stderr).toMatch(/check:i18n: 3 unused message\(s\): warnings, not problems\n/);
    // The summary is the last line, as before rule 4.
    expect(out.stdout).toMatch(/check:i18n: ok \(2 message keys in 3 files\)\n$/);
  });

  it('notes the namespaces that a template head covers whole', async () => {
    await write('main.tsx', "import('./admin/index');\nexport const A = (c: string) => t(`room.${c}`);\n");
    const result = await checkI18n({ srcDir, catalogPath });
    expect(result.builtWhole).toEqual(['room']);
    expect(result.warnings.map((w) => w.message)).not.toContain('message "room.old" is not used: no code asks for it');

    const out = await cli(['--src', srcDir, '--catalog', catalogPath]);
    expect(out.code).toBe(0);
    expect(out.stdout).toContain(
      'check:i18n: note: unused messages are not checked in room.*: code builds their keys from the namespace alone (`room.${…}`)\n',
    );
    expect(out.stdout).toMatch(/check:i18n: ok \(1 message keys in 3 files\)\n$/);
  });

  it('fails when a message that the code asks for is gone; the warnings stay warnings', async () => {
    await write('i18n/en.json', { room: { old: 'Old' }, viewer: { onlyInTests: 'T' } });
    const out = await cli(['--src', srcDir, '--catalog', catalogPath]);
    expect(out.code).toBe(1);
    expect(out.stderr).toMatch(/main\.tsx:2:26: message key "room\.title" missing in \S*en\.json/);
    expect(out.stderr).toMatch(/check:i18n: 1 problem\(s\)/);
    expect(warned(out.stderr)).toHaveLength(3);
  });
});

describe('check-i18n.mjs on this package', () => {
  let dir = '';

  beforeEach(async () => {
    dir = await mkdtemp(path.join(tmpdir(), 'isshoni-check-i18n-package-'));
  });

  afterEach(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  it('passes; each warning is a message of a catalog file, at its line', async () => {
    const result = await checkI18n({ srcDir: WEB_SRC, catalogPath: EN_JSON });
    expect(result.problems).toEqual([]);
    for (const w of result.warnings) {
      const key = /message "([^"]+)"/.exec(w.message)?.[1] ?? '';
      const text = await readFile(path.resolve(w.file), 'utf8');
      const last = key.split('.').at(-1) ?? '';
      expect(text.split('\n')[w.line - 1]?.slice(w.column - 1)).toMatch(new RegExp(`^"${last}":`));
    }
  });

  it('fails when a message that this package uses is removed from a copy of en.json, and warns about one no code uses', async () => {
    const en = JSON.parse(await readFile(EN_JSON, 'utf8')) as Record<string, Record<string, unknown>>;
    const drop = (key: string): void => {
      const parts = key.split('.');
      const last = parts.pop() ?? '';
      let node: Record<string, unknown> = en;
      for (const p of parts) node = node[p] as Record<string, unknown>;
      expect(typeof node[last]).toBe('string');
      Reflect.deleteProperty(node, last);
    };
    // The debug overlay's title, the sharer's "You're live", and the VersionMismatch screen's title.
    drop('viewer.debug.title');
    drop('share.announce.live');
    drop('common.screens.versionMismatch.title');
    (en['viewer'] as Record<string, unknown>)['notUsedAnywhere'] = 'Nobody asks for me';
    const copy = path.join(dir, 'en.json');
    const text = `${JSON.stringify(en, null, 2)}\n`;
    await writeFile(copy, text);

    const out = await cli(['--src', WEB_SRC, '--catalog', copy]);
    expect(out.code).toBe(1);
    const missing = [...out.stderr.matchAll(/message key "([\w.]+)" missing in /g)].map((m) => m[1]);
    expect([...new Set(missing)].sort()).toEqual([
      'common.screens.versionMismatch.title',
      'share.announce.live',
      'viewer.debug.title',
    ]);
    const line = text.split('\n').findIndex((l) => l.includes('"notUsedAnywhere"')) + 1;
    expect(warned(out.stderr)).toContain(`${path.basename(copy)}:${String(line)}:5 viewer.notUsedAnywhere`);
  });
});
