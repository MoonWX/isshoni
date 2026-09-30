// @vitest-environment node
// scripts/check-i18n.mjs, rule 1 (05 §16.5): a t('…') literal key missing from en.json fails the check.
import { execFile } from 'node:child_process';
import { mkdir, mkdtemp, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { checkI18n, isScanned, keyProblem, scanSource } from '../../scripts/check-i18n.mjs';

const SCRIPT = fileURLToPath(new URL('../../scripts/check-i18n.mjs', import.meta.url));
const run = promisify(execFile);

const CATALOG = {
  common: { appName: 'isshoni', nested: { deep: 'Deep' } },
  viewer: { watching_one: '{{count}} watching', watching_other: '{{count}} watching' },
};

describe('scanSource', () => {
  it('finds literal keys in t() and i18n.t() calls and i18nKey attributes', () => {
    const src = [
      "const a = t('common.appName');",
      'const b = i18n.t(`common.nested.deep`, { x: 1 });',
      "const c = <Trans i18nKey='viewer.watching' />;",
      'const d = <Trans i18nKey={"x.y"} />;',
      't(dynamicKey); t(`errors.${code}`); other("not.a.key"); // t(\'in.a.comment\')',
    ].join('\n');
    const { keys, prefixes } = scanSource('a.tsx', src);
    expect(keys.map((k) => k.key)).toEqual(['common.appName', 'common.nested.deep', 'viewer.watching', 'x.y']);
    expect(keys[0]).toMatchObject({ file: 'a.tsx', line: 1, column: 13 });
    expect(prefixes).toEqual([]);
  });

  it.each([
    ["const { t } = useTranslation(undefined, { keyPrefix: 'room' });", 1, 43],
    ["const { t } = useTranslation('ns', { keyPrefix } as const);", 1, 38],
    ["export default withTranslation('ns', { keyPrefix: 'room' })(Page);", 1, 40],
    ["t('title', { count: 1, keyPrefix: 'room' });", 1, 24],
    ["i18n.t('title', ({ keyPrefix: 'room' }));", 1, 20],
    ["const t = i18n.getFixedT(null, 'ns', 'room');", 1, 38],
    ["const t = i18next.getFixedT('en', undefined, prefix);", 1, 46],
    ["i18next.use(initReactI18next).init({ lng: 'en', react: { keyPrefix: 'room' } });", 1, 58],
    ["const i = i18next.createInstance({ react: { useSuspense: false, keyPrefix: 'room' } });", 1, 65],
    ["const a = <Translation keyPrefix='room'>{(t) => t('title')}</Translation>;", 1, 24],
    ["const a = <Trans i18nKey='room.title' tOptions={{ keyPrefix: 'x' }} />;", 1, 51],
  ])('reports the keyPrefix in %s', (src, line, column) => {
    const { prefixes } = scanSource('a.tsx', src);
    expect(prefixes).toHaveLength(1);
    expect(prefixes[0]).toMatchObject({ file: 'a.tsx', line, column });
    expect(prefixes[0]?.message).toMatch(/keyPrefix/);
  });

  it.each([
    "const storage = createStorage({ keyPrefix: 'isshoni.' });",
    "const keyPrefix = 'isshoni.'; const opts = { keyPrefix };",
    "cache.init({ keyPrefix: 'isshoni.' });",
    "i18next.init({ keyPrefix: 'room' });", // not an init option: only `react.keyPrefix` is read
    "const r = { react: { keyPrefix: 'room' } };",
    "const t = i18n.getFixedT(null, 'ns'); const u = i18n.getFixedT('en', 'ns', undefined);",
    "const a = <Store keyPrefix='isshoni.' />;",
    "const a = <Trans i18nKey='room.title' values={{ keyPrefix: 'x' }} />;",
  ])('ignores a keyPrefix that i18next does not read: %s', (src) => {
    expect(scanSource('a.tsx', src).prefixes).toEqual([]);
  });
});

describe('keyProblem', () => {
  it.each([
    ['common.appName', null],
    ['common.nested.deep', null],
    ['viewer.watching', null], // plural forms
    ['common.missing', 'missing'],
    ['nope', 'missing'],
    ['common.appName.more', 'missing'],
    ['common.nested', 'has sub-keys, not a message'],
    ['common', 'has sub-keys, not a message'],
  ])('%s → %s', (key, want) => {
    expect(keyProblem(CATALOG, key)).toBe(want);
  });
});

describe('isScanned', () => {
  it.each([
    ['app/Page.tsx', true],
    ['lib/log.ts', true],
    ['protocol/types.gen.ts', false],
    ['types/globals.d.ts', false],
    ['app/Page.test.tsx', false],
    ['i18n/check-i18n.node.test.ts', false],
    ['test/setup.ts', false],
    ['i18n/en.json', false],
  ])('%s → %s', (rel, want) => {
    expect(isScanned(rel)).toBe(want);
  });
});

describe('check-i18n.mjs', () => {
  let dir = '';
  let srcDir = '';
  let catalogPath = '';

  beforeEach(async () => {
    dir = await mkdtemp(path.join(tmpdir(), 'isshoni-check-i18n-'));
    srcDir = path.join(dir, 'src');
    catalogPath = path.join(srcDir, 'i18n', 'en.json');
    await mkdir(path.join(srcDir, 'i18n'), { recursive: true });
    await mkdir(path.join(srcDir, 'app'), { recursive: true });
    await writeFile(catalogPath, JSON.stringify(CATALOG));
    await writeFile(
      path.join(srcDir, 'app', 'Page.tsx'),
      "export const Page = () => <h1>{t('common.appName')} {t('viewer.watching', { count: 2 })}</h1>;\n",
    );
    // Tests may use missing keys; they are not scanned.
    await writeFile(path.join(srcDir, 'app', 'Page.test.tsx'), "t('test.only.key');\n");
  });

  afterEach(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  it('passes when every literal key exists', async () => {
    const result = await checkI18n({ srcDir, catalogPath });
    expect(result).toMatchObject({ problems: [], keysUsed: 2, files: 1 });
    const { stdout } = await run(process.execPath, [SCRIPT, '--src', srcDir, '--catalog', catalogPath]);
    expect(stdout).toMatch(/check:i18n: ok \(2 message keys in 1 files\)/);
  });

  it('fails with file:line:column when a t() key is missing from en.json', async () => {
    await writeFile(path.join(srcDir, 'app', 'Other.ts'), "\nexport const x = () => t('room.title');\n");
    const result = await checkI18n({ srcDir, catalogPath });
    expect(result.problems).toHaveLength(1);
    expect(result.problems[0]).toMatchObject({ line: 2, column: 26 });

    const failed = run(process.execPath, [SCRIPT, '--src', srcDir, '--catalog', catalogPath]);
    await expect(failed).rejects.toMatchObject({ code: 1 });
    const err = (await failed.catch((e: unknown) => e)) as { stderr: string };
    expect(err.stderr).toMatch(/Other\.ts:2:26: message key "room\.title" missing in /);
    expect(err.stderr).toMatch(/check:i18n: 1 problem\(s\)/);
  });

  it('fails through a symlinked path to the script (import.meta.main, not argv[1])', async (ctx) => {
    // Node resolves the main module's symlinks but not process.argv[1]; comparing the two used to skip main() and
    // exit 0 without checking anything.
    await writeFile(path.join(srcDir, 'app', 'Other.ts'), "export const x = () => t('room.title');\n");
    const link = path.join(dir, 'link', 'check-i18n.mjs');
    await mkdir(path.dirname(link));
    try {
      await symlink(SCRIPT, link, 'file');
    } catch (err) {
      // Windows without Developer Mode can't create symlinks.
      if ((err as NodeJS.ErrnoException).code === 'EPERM') ctx.skip();
      throw err;
    }
    const failed = run(process.execPath, [link, '--src', srcDir, '--catalog', catalogPath]);
    await expect(failed).rejects.toMatchObject({ code: 1 });
    const err = (await failed.catch((e: unknown) => e)) as { stderr: string };
    expect(err.stderr).toMatch(/Other\.ts:1:26: message key "room\.title" missing in /);
  });

  it('exits 2 on an unknown option', async () => {
    await expect(run(process.execPath, [SCRIPT, '--nope'])).rejects.toMatchObject({ code: 2 });
  });
});
