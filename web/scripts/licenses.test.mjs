// Tests for scripts/licenses.mjs (06 §8.3), on small npm projects built in a temporary directory. They run under
// Node's test runner, not Vitest (whose config covers src/ and build/): `node --test scripts/licenses.test.mjs`
// from web/; the CI licenses job runs them.
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { after, before, describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';

import {
  declaredLicense,
  dependencyPath,
  devProblem,
  evaluate,
  normalize,
  readOverrides,
  shippedProblem,
} from './licenses.mjs';

const SCRIPT = fileURLToPath(new URL('./licenses.mjs', import.meta.url));
const execFileAsync = promisify(execFile);

const MIT = 'MIT License\n\nCopyright (c) 2026 Fixture authors\n\nPermission is hereby granted, free of charge.\n';

/**
 * @typedef {import('./licenses.mjs').QueryNode} QueryNode
 * @typedef {{ name: string, version: string, license?: string, deps?: Record<string, string>,
 *   files?: Record<string, string> }} FixturePackage
 * @typedef {{ deps?: Record<string, string>, devDeps?: Record<string, string>, packages: FixturePackage[] }} Project
 */

/**
 * Writes an npm project with an installed node_modules (no lockfile needed for npm query).
 * @param {string} dir
 * @param {Project} spec
 */
async function makeProject(dir, spec) {
  await mkdir(dir, { recursive: true });
  const root = {
    name: 'fixture-app',
    version: '1.0.0',
    private: true,
    license: 'Apache-2.0',
    dependencies: spec.deps ?? {},
    devDependencies: spec.devDeps ?? {},
  };
  await writeFile(path.join(dir, 'package.json'), JSON.stringify(root, null, 2));
  for (const p of spec.packages) {
    const pkgDir = path.join(dir, 'node_modules', p.name);
    await mkdir(pkgDir, { recursive: true });
    /** @type {Record<string, unknown>} */
    const json = { name: p.name, version: p.version, dependencies: p.deps ?? {} };
    if (p.license !== undefined) json['license'] = p.license;
    await writeFile(path.join(pkgDir, 'package.json'), JSON.stringify(json, null, 2));
    for (const [name, text] of Object.entries(p.files ?? {})) {
      await writeFile(path.join(pkgDir, name), text);
    }
  }
}

/**
 * Runs the script; resolves with the exit status and output, never rejects on a non-zero exit.
 * @param {string[]} args
 * @returns {Promise<{ code: number, stdout: string, stderr: string }>}
 */
async function cli(args) {
  try {
    const { stdout, stderr } = await execFileAsync(process.execPath, [SCRIPT, ...args]);
    return { code: 0, stdout, stderr };
  } catch (err) {
    const e = /** @type {{ code?: number, stdout?: string, stderr?: string }} */ (err);
    return { code: e.code ?? -1, stdout: e.stdout ?? '', stderr: e.stderr ?? '' };
  }
}

describe('declaredLicense', () => {
  it('reads the license field and its old forms', () => {
    assert.equal(declaredLicense({ location: 'x', license: 'MIT' }), 'MIT');
    assert.equal(declaredLicense({ location: 'x', license: { type: 'ISC', url: 'https://x' } }), 'ISC');
    assert.equal(declaredLicense({ location: 'x', licenses: [{ type: 'MIT' }] }), 'MIT');
    assert.equal(
      declaredLicense({ location: 'x', licenses: [{ type: 'MIT' }, { type: 'Apache-2.0' }] }),
      '(MIT OR Apache-2.0)',
    );
    assert.equal(declaredLicense({ location: 'x' }), null);
    assert.equal(declaredLicense({ location: 'x', license: '  ' }), null);
  });
});

describe('shippedProblem (allowlist)', () => {
  for (const ok of ['MIT', 'BlueOak-1.0.0', '(MIT OR GPL-3.0-only)', '(Apache-2.0 AND BSD-3-Clause)', '0BSD']) {
    it(`allows ${ok}`, () => assert.equal(shippedProblem(ok), null));
  }
  for (const bad of ['GPL-3.0-only', 'MPL-2.0', '(MIT AND CC-BY-4.0)', 'LGPL-2.1-or-later', 'UNLICENSED']) {
    it(`refuses ${bad}`, () => assert.match(String(shippedProblem(bad)), /not on the allowlist|not a valid SPDX/));
  }
  it('refuses a missing license', () => assert.match(String(shippedProblem(null)), /no license field/));
  it('refuses a license stated in a file', () =>
    assert.match(String(shippedProblem('SEE LICENSE IN LICENSE.md')), /not a valid SPDX expression/));
});

describe('devProblem (denylist)', () => {
  for (const ok of ['MIT', 'MPL-2.0', 'LGPL-3.0-only', 'CC-BY-4.0', '(GPL-2.0-only OR MIT)', 'Python-2.0']) {
    it(`allows ${ok}`, () => assert.equal(devProblem(ok), null));
  }
  for (const bad of ['GPL-3.0-only', 'AGPL-3.0-or-later', 'SSPL-1.0', 'BUSL-1.1', '(GPL-2.0-only AND MIT)']) {
    it(`refuses ${bad}`, () => assert.match(String(devProblem(bad)), /can only be met with GPL/));
  }
  it('refuses an invalid expression', () => assert.match(String(devProblem('Apache 2')), /not a valid SPDX/));
});

describe('dependencyPath', () => {
  it('walks the shortest chain up to the project', () => {
    /** @type {QueryNode[]} */
    const nodes = [
      { name: 'app', version: '1.0.0', location: '', from: [] },
      { name: 'a', version: '1.0.0', location: 'node_modules/a', from: [''] },
      { name: 'b', version: '2.0.0', location: 'node_modules/b', from: ['node_modules/a'] },
      { name: 'c', version: '3.0.0', location: 'node_modules/c', from: ['node_modules/b', ''] },
      { name: 'orphan', version: '1.0.0', location: 'node_modules/orphan', from: [] },
    ];
    const byLocation = new Map(nodes.map((n) => [n.location, n]));
    /** @param {string} name */
    const pathOf = (name) => dependencyPath(/** @type {QueryNode} */ (nodes.find((n) => n.name === name)), byLocation);
    assert.deepEqual(pathOf('b'), ['b@2.0.0', 'a@1.0.0', 'app']);
    assert.deepEqual(pathOf('c'), ['c@3.0.0', 'app']);
    assert.deepEqual(pathOf('a'), ['a@1.0.0', 'app']);
    assert.deepEqual(pathOf('orphan'), ['orphan@1.0.0']);
  });
});

describe('evaluate', () => {
  it('reports a package once, with the shipped rule first, sorted by id', () => {
    /** @type {QueryNode[]} */
    const shipped = [
      { name: 'app', version: '1.0.0', location: '', license: 'Apache-2.0' },
      { name: 'z', version: '1.0.0', location: 'node_modules/z', license: 'GPL-3.0-only', from: [''] },
      { name: 'y', version: '1.0.0', location: 'node_modules/y', license: 'MIT', from: [''] },
    ];
    const all = [
      ...shipped,
      { name: 'dev', version: '1.0.0', location: 'node_modules/dev', license: 'AGPL-3.0-only', from: [''] },
    ];
    const { problems, shipped: n, all: m } = evaluate({ shipped, all }, {});
    assert.equal(n, 2);
    assert.equal(m, 3);
    assert.deepEqual(
      problems.map((p) => [p.id, p.why]),
      [
        ['dev@1.0.0', 'the license can only be met with GPL, AGPL, SSPL or BUSL'],
        ['z@1.0.0', 'shipped, and the license is not on the allowlist'],
      ],
    );
  });

  it('applies overrides', () => {
    /** @type {QueryNode[]} */
    const shipped = [{ name: 'x', version: '1.0.0', location: 'node_modules/x', license: 'SEE LICENSE IN LICENSE' }];
    assert.equal(evaluate({ shipped, all: shipped }, {}).problems.length, 1);
    const overrides = { 'x@1.0.0': { license: 'MIT', reason: 'LICENSE is the MIT text' } };
    assert.equal(evaluate({ shipped, all: shipped }, overrides).problems.length, 0);
  });
});

describe('readOverrides', () => {
  /** @type {string} */
  let dir;
  before(async () => {
    dir = await mkdtemp(path.join(tmpdir(), 'isshoni-licenses-overrides-'));
  });
  after(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  /** @param {unknown} data */
  const write = async (data) => {
    const file = path.join(dir, 'overrides.json');
    await writeFile(file, JSON.stringify(data));
    return file;
  };

  it('reads valid entries, scoped names included', async () => {
    const data = { 'a@1.0.0': { license: 'MIT', reason: 'r' }, '@s/b@2.0.0': { license: '(MIT OR ISC)', reason: 'r' } };
    assert.deepEqual(await readOverrides(await write(data)), data);
  });
  it('treats a missing file as no overrides', async () => {
    assert.deepEqual(await readOverrides(path.join(dir, 'missing.json')), {});
  });
  for (const [name, data, pattern] of /** @type {[string, unknown, RegExp][]} */ ([
    ['a key without a version', { a: { license: 'MIT', reason: 'r' } }, /not name@version/],
    ['no reason', { 'a@1': { license: 'MIT', reason: ' ' } }, /non-empty "reason"/],
    ['an invalid license', { 'a@1': { license: 'MIT or so', reason: 'r' } }, /not a valid SPDX expression/],
    ['an array', [], /want a JSON object/],
  ])) {
    it(`refuses ${name}`, async () => {
      await assert.rejects(readOverrides(await write(data)), pattern);
    });
  }
});

describe('normalize', () => {
  it('normalizes line ends, BOM and blank edges', () => {
    assert.equal(normalize('\uFEFF\r\n a \r\nb\t\r\n\r\n'), ' a\nb\n');
    assert.equal(normalize('\n\n'), '');
  });
});

describe('licenses.mjs on fixture projects', () => {
  /** @type {string} */
  let tmp;
  /** @type {string} */
  let noOverrides;
  before(async () => {
    tmp = await mkdtemp(path.join(tmpdir(), 'isshoni-licenses-'));
    noOverrides = path.join(tmp, 'no-overrides.json');
    await writeFile(noOverrides, '{}\n');
  });
  after(async () => {
    await rm(tmp, { recursive: true, force: true });
  });

  /**
   * The clean base project: an MIT prod dependency with a NOTICE file, and LGPL and MPL dev tools.
   * @returns {Required<Project>}
   */
  const base = () => ({
    deps: { good: '1.0.0' },
    devDeps: { 'lgpl-tool': '1.0.0', 'mpl-tool': '1.0.0' },
    packages: [
      { name: 'good', version: '1.0.0', license: 'MIT', files: { LICENSE: MIT, NOTICE: 'good\nCopyright 2026\n' } },
      { name: 'lgpl-tool', version: '1.0.0', license: 'LGPL-3.0-only' },
      { name: 'mpl-tool', version: '1.0.0', license: 'MPL-2.0' },
    ],
  });

  it('passes a clean project', async () => {
    const dir = path.join(tmp, 'clean');
    await makeProject(dir, base());
    const r = await cli(['check', '--dir', dir, '--overrides', noOverrides]);
    assert.equal(r.code, 0, r.stderr);
    assert.match(r.stdout, /ok \(1 shipped packages allowed, 3 installed packages checked\)/);
  });

  it('fails on a GPL-3.0 fixture dependency of a shipped package, with its dependency path', async () => {
    const dir = path.join(tmp, 'gpl');
    const spec = base();
    const good = /** @type {FixturePackage} */ (spec.packages[0]);
    good.deps = { 'gpl-fixture': '^1.0.0' };
    spec.packages.push({ name: 'gpl-fixture', version: '1.0.0', license: 'GPL-3.0-only' });
    await makeProject(dir, spec);
    const r = await cli(['check', '--dir', dir, '--overrides', noOverrides]);
    assert.equal(r.code, 1, r.stdout);
    assert.match(
      r.stderr,
      /gpl-fixture@1\.0\.0 \(GPL-3\.0-only\) ← good@1\.0\.0 ← fixture-app: shipped, and the license is not on the allowlist/,
    );
    // notices refuses to write the list for it.
    const out = path.join(tmp, 'gpl-licenses.txt');
    const n = await cli(['notices', '--dir', dir, '--overrides', noOverrides, '--out', out]);
    assert.equal(n.code, 1);
    await assert.rejects(readFile(out));
  });

  it('fails on a GPL-3.0 dev dependency', async () => {
    const dir = path.join(tmp, 'gpl-dev');
    const spec = base();
    spec.devDeps['gpl-tool'] = '1.0.0';
    spec.packages.push({ name: 'gpl-tool', version: '1.0.0', license: 'GPL-3.0-or-later' });
    await makeProject(dir, spec);
    const r = await cli(['check', '--dir', dir, '--overrides', noOverrides]);
    assert.equal(r.code, 1);
    assert.match(r.stderr, /gpl-tool@1\.0\.0 \(GPL-3\.0-or-later\) ← fixture-app: the license can only be met with/);
  });

  it('fails on a shipped package without a license field, unless overridden', async () => {
    const dir = path.join(tmp, 'nolicense');
    const spec = base();
    spec.deps['bare'] = '1.0.0';
    spec.packages.push({ name: 'bare', version: '2.0.0', files: { 'LICENSE.md': MIT } });
    await makeProject(dir, spec);
    const r = await cli(['check', '--dir', dir, '--overrides', noOverrides]);
    assert.equal(r.code, 1);
    assert.match(r.stderr, /bare@2\.0\.0 \(no license\) ← fixture-app: shipped, and has no license field/);

    const overrides = path.join(tmp, 'overrides.json');
    await writeFile(overrides, JSON.stringify({ 'bare@2.0.0': { license: 'MIT', reason: 'LICENSE.md is MIT' } }));
    const ok = await cli(['check', '--dir', dir, '--overrides', overrides]);
    assert.equal(ok.code, 0, ok.stderr);
  });

  it('writes licenses.txt with the shipped packages only, sorted, with their license and notice files', async () => {
    const dir = path.join(tmp, 'notices');
    const spec = base();
    spec.deps['@scope/alpha'] = '1.0.0';
    spec.packages.push({
      name: '@scope/alpha',
      version: '0.1.0',
      license: '(MIT OR Apache-2.0)',
      files: {
        'LICENSE-MIT': MIT,
        'licence.txt': 'Also here.\r\n',
        README: 'not a license',
        'license.js': 'export const source = "code";\n',
      },
    });
    await makeProject(dir, spec);
    const out = path.join(tmp, 'out', 'licenses.txt');
    const r = await cli(['notices', '--dir', dir, '--overrides', noOverrides, '--out', out]);
    assert.equal(r.code, 0, r.stderr);
    const text = await readFile(out, 'utf8');
    const rule = '-'.repeat(80);
    assert.ok(text.startsWith('isshoni web client: third-party packages\n'));
    assert.ok(
      text.includes(`\n${rule}\n@scope/alpha 0.1.0\nLicense: (MIT OR Apache-2.0)\n${rule}\n\n== LICENSE-MIT ==\n`),
    );
    assert.ok(text.includes('\n== licence.txt ==\n\nAlso here.\n'));
    assert.ok(text.indexOf('@scope/alpha 0.1.0') < text.indexOf('\ngood 1.0.0\n'), 'sorted by name');
    assert.ok(text.indexOf('== LICENSE ==') < text.indexOf('== NOTICE =='), 'license files before notices');
    assert.ok(!text.includes('README') && !text.includes('license.js') && !text.includes('source = "code"'));
    assert.ok(!text.includes('lgpl-tool') && !text.includes('fixture-app'), 'no dev packages, not the project');
    assert.ok(!text.includes(tmp), 'no local paths');
  });

  // npm query prints `***` for every UUID-shaped segment of a package's absolute path and realpath.
  it('writes licenses.txt when the project sits under a UUID-named directory', async () => {
    const dir = path.join(tmp, '123e4567-e89b-12d3-a456-426614174000', 'app');
    await makeProject(dir, base());
    const out = path.join(tmp, 'uuid-licenses.txt');
    const r = await cli(['notices', '--dir', dir, '--overrides', noOverrides, '--out', out]);
    assert.equal(r.code, 0, r.stderr);
    const text = await readFile(out, 'utf8');
    assert.ok(text.includes('\ngood 1.0.0\nLicense: MIT\n'));
    assert.ok(text.includes('\n== LICENSE ==\n\nMIT License\n'));
    assert.ok(text.includes('\n== NOTICE ==\n\ngood\nCopyright 2026\n'));
  });

  it('fails when node_modules is not installed', async () => {
    const dir = path.join(tmp, 'not-installed');
    await makeProject(dir, { deps: { missing: '1.0.0' }, packages: [] });
    const r = await cli(['check', '--dir', dir, '--overrides', noOverrides]);
    assert.equal(r.code, 1);
    assert.match(r.stderr, /not installed: missing; run npm ci there first/);
  });

  it('exits 2 on bad usage', async () => {
    assert.equal((await cli([])).code, 2);
    assert.equal((await cli(['audit'])).code, 2);
    assert.equal((await cli(['check', 'extra'])).code, 2);
    assert.equal((await cli(['check', '--nope'])).code, 2);
  });
});
