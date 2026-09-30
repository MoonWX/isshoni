#!/usr/bin/env node
// The npm half of the license gate, and the web client's license list (docs/m1/06-deploy-and-ci.md §8.3).
//
//   node web/scripts/licenses.mjs check   [--dir web|docs]   (task licenses)
//   node web/scripts/licenses.mjs notices [--dir web]        (npm run build, after vite build)
//
// check: every package the SPA ships (`npm query .prod`) must be under a license of ALLOWLIST: an SPDX `OR`
//   passes when any branch passes, `AND` needs all of them, and a missing `license` fails. No installed package,
//   dev ones included (`npm query *`), may be under a license that can only be met with GPL, AGPL, SSPL or BUSL
//   (LGPL dev tools pass: they are not shipped). A license that is not a valid SPDX expression fails too: add an
//   override after reading the package's license.
// notices: runs the shipped-package check, then writes web/dist/licenses.txt: each shipped package with its name,
//   version, SPDX license and the text of its LICENSE*, LICENCE*, COPYING* and NOTICE* files. The SPA links it from
//   /about, and tools/notices copies it into THIRD_PARTY_NOTICES.
//
// Paths: the repository root is found from this file (web/scripts/ → ../..), and --dir is relative to it, so the
// script works the same from the root (Task) and from web/ (npm run build). The tests point --overrides (default
// web/licenses.overrides.json) and --out (default web/dist/licenses.txt) elsewhere.
//
// Overrides, for packages that state their license only in a file:
//   {"name@version": {"license": "<SPDX expression>", "reason": "<where the license is stated>"}}
//
// Exit 0 when the check passes, 1 when it fails (each offending package is listed as
// `name@version (license) ← dependency path`), 2 on bad usage.
import { execFile } from 'node:child_process';
import { mkdir, readdir, readFile, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs, promisify } from 'node:util';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

// Both SPDX packages are CommonJS without type declarations: load them with JSDoc types instead of an untyped import.
const load = createRequire(import.meta.url);
/** @type {(expression: string) => SpdxNode} throws on an invalid expression */
const parseSpdx = load('spdx-expression-parse');
/** @type {(expression: string, allowed: string[]) => boolean} throws on an invalid expression */
const satisfies = load('spdx-satisfies');

/** The licenses a shipped package may have (06 §8.3); tools/notices applies the same list to Go modules. */
export const ALLOWLIST = Object.freeze([
  'MIT',
  'ISC',
  'BSD-2-Clause',
  'BSD-3-Clause',
  'Apache-2.0',
  '0BSD',
  'Zlib',
  'CC0-1.0',
  'Unlicense',
  'BlueOak-1.0.0',
]);

/** Licenses no installed package may be limited to, dev packages included. LGPL does not match. */
export const DENIED = /^(GPL|AGPL|SSPL|BUSL)-/;

/**
 * A node of `npm query` output: the package's package.json fields, plus npm's own. Only `location`, relative to the
 * project, is used to find a package: npm redacts UUID-shaped path segments to `***` in the absolute `path` and
 * `realpath`, so they do not exist on disk when the checkout sits under such a directory.
 * @typedef {{ name?: string, version?: string, license?: unknown, licenses?: unknown, location: string,
 *   from?: string[], dependencies?: Record<string, string>, devDependencies?: Record<string, string> }} QueryNode
 * @typedef {Record<string, { license: string, reason: string }>} Overrides
 * @typedef {{ id: string, license: string | null, why: string, path: string[] }} Problem
 * @typedef {{ license: string } | { conjunction: 'and' | 'or', left: SpdxNode, right: SpdxNode }} SpdxNode
 */

/** @param {QueryNode} node */
const idOf = (node) => `${node.name ?? '(unnamed)'}@${node.version ?? '(no version)'}`;

/** @param {string} a @param {string} b */
const byString = (a, b) => (a < b ? -1 : a > b ? 1 : 0);

/**
 * The license a package declares: `license` (an SPDX expression, or the old `{type}` object) or the old
 * `licenses` array, whose entries are alternatives.
 * @param {QueryNode} node
 * @returns {string | null}
 */
export function declaredLicense(node) {
  /** @param {unknown} v */
  const typeOf = (v) =>
    typeof v === 'string' ? v : v && typeof v === 'object' && 'type' in v && typeof v.type === 'string' ? v.type : '';
  const license = typeOf(node.license).trim();
  if (license) return license;
  if (Array.isArray(node.licenses)) {
    const types = node.licenses.map((l) => typeOf(l).trim()).filter(Boolean);
    if (types.length === 1) return types[0] ?? null;
    if (types.length > 1) return `(${types.join(' OR ')})`;
  }
  return null;
}

/**
 * @param {QueryNode} node
 * @param {Overrides} overrides
 */
export function licenseOf(node, overrides) {
  return overrides[idOf(node)]?.license ?? declaredLicense(node);
}

/**
 * Why a shipped package fails the allowlist, or null when it passes.
 * @param {string | null} expression
 */
export function shippedProblem(expression) {
  if (expression === null) return 'shipped, and has no license field';
  try {
    parseSpdx(expression);
    if (satisfies(expression, [...ALLOWLIST])) return null;
  } catch {
    return 'shipped, and the license is not a valid SPDX expression: add an override';
  }
  return 'shipped, and the license is not on the allowlist';
}

/**
 * Why an installed package (dev included) fails the denylist, or null when it passes.
 * @param {string | null} expression
 */
export function devProblem(expression) {
  if (expression === null) return 'has no license field: add an override';
  /** @type {SpdxNode} */
  let tree;
  try {
    tree = parseSpdx(expression);
  } catch {
    return 'the license is not a valid SPDX expression: add an override';
  }
  /** @param {SpdxNode} n @returns {boolean} */
  const avoidable = (n) => {
    if ('conjunction' in n) {
      return n.conjunction === 'or' ? avoidable(n.left) || avoidable(n.right) : avoidable(n.left) && avoidable(n.right);
    }
    return !DENIED.test(n.license);
  };
  return avoidable(tree) ? null : 'the license can only be met with GPL, AGPL, SSPL or BUSL';
}

/**
 * The shortest dependency chain from a package up to the project: [the package, its dependent, …, the project].
 * @param {QueryNode} node
 * @param {Map<string, QueryNode>} byLocation
 * @returns {string[]}
 */
export function dependencyPath(node, byLocation) {
  /** @type {Map<string, string | null>} the dependency through which each location was reached */
  const via = new Map([[node.location, null]]);
  const queue = [node.location];
  while (queue.length > 0 && !via.has('')) {
    const location = /** @type {string} */ (queue.shift());
    for (const from of byLocation.get(location)?.from ?? []) {
      if (!via.has(from)) {
        via.set(from, location);
        queue.push(from);
      }
    }
  }
  if (!via.has('')) return [idOf(node)]; // not reachable from the project (extraneous)
  const chain = [];
  for (let location = /** @type {string | null} */ (''); location !== null; location = via.get(location) ?? null) {
    const n = byLocation.get(location);
    chain.push(location === '' ? (n?.name ?? 'package.json') : n ? idOf(n) : location);
  }
  return chain.reverse();
}

/**
 * Applies both rules to the query results.
 * @param {{ shipped: QueryNode[], all: QueryNode[] }} packages
 * @param {Overrides} overrides
 * @returns {{ shipped: number, all: number, problems: Problem[] }}
 */
export function evaluate({ shipped, all }, overrides) {
  const byLocation = new Map([...shipped, ...all].map((n) => [n.location, n]));
  /** @type {Map<string, Problem>} */
  const problems = new Map();
  /** @param {QueryNode[]} nodes @param {(license: string | null) => string | null} rule */
  const apply = (nodes, rule) => {
    const ids = new Set();
    for (const node of nodes) {
      if (node.location === '') continue; // the project itself
      const id = idOf(node);
      ids.add(id);
      if (problems.has(id)) continue;
      const license = licenseOf(node, overrides);
      const why = rule(license);
      if (why) problems.set(id, { id, license, why, path: dependencyPath(node, byLocation) });
    }
    return ids.size;
  };
  const shippedCount = apply(shipped, shippedProblem);
  const allCount = apply(all, devProblem);
  return {
    shipped: shippedCount,
    all: allCount,
    problems: [...problems.values()].sort((a, b) => byString(a.id, b.id)),
  };
}

/**
 * Formats the problems for the exit-1 message.
 * @param {Problem[]} problems
 */
export function formatProblems(problems) {
  return problems.map(
    (p) => `  ${[`${p.id} (${p.license ?? 'no license'})`, ...p.path.slice(1)].join(' ← ')}: ${p.why}`,
  );
}

/**
 * Reads and checks the overrides file. A missing file means no overrides.
 * @param {string} file
 * @returns {Promise<Overrides>}
 */
export async function readOverrides(file) {
  let text;
  try {
    text = await readFile(file, 'utf8');
  } catch (err) {
    if (err instanceof Error && 'code' in err && err.code === 'ENOENT') return {};
    throw err;
  }
  /** @type {unknown} */
  const data = JSON.parse(text);
  if (!data || typeof data !== 'object' || Array.isArray(data)) throw new Error(`${file}: want a JSON object`);
  /** @type {Overrides} */
  const overrides = {};
  for (const [key, value] of Object.entries(data)) {
    const license = value && typeof value === 'object' && 'license' in value ? value.license : undefined;
    const reason = value && typeof value === 'object' && 'reason' in value ? value.reason : undefined;
    if (!/^(@[^/@]+\/)?[^/@]+@[^@]+$/.test(key)) throw new Error(`${file}: "${key}" is not name@version`);
    if (typeof license !== 'string' || typeof reason !== 'string' || !reason.trim()) {
      throw new Error(`${file}: "${key}" needs a string "license" and a non-empty "reason"`);
    }
    try {
      parseSpdx(license);
    } catch {
      throw new Error(`${file}: "${key}": "${license}" is not a valid SPDX expression`);
    }
    overrides[key] = { license, reason };
  }
  return overrides;
}

const run = promisify(execFile);

/**
 * Runs `npm query <selector>` in dir. Under `npm run`, the same npm runs it (npm_execpath).
 * @param {string} dir
 * @param {string} selector
 * @returns {Promise<QueryNode[]>}
 */
export async function npmQuery(dir, selector) {
  const npmCli = process.env['npm_execpath'];
  const viaNode = npmCli !== undefined && /npm-cli\.c?js$/.test(npmCli);
  const [file, args] = viaNode ? [process.execPath, [npmCli, 'query', selector]] : ['npm', ['query', selector]];
  let stdout;
  try {
    ({ stdout } = await run(file, args, {
      cwd: dir,
      maxBuffer: 512 * 1024 * 1024,
      shell: !viaNode && process.platform === 'win32', // npm is npm.cmd there
    }));
  } catch (err) {
    throw new Error(`npm query ${selector} failed in ${dir}: ${err instanceof Error ? err.message : String(err)}`);
  }
  /** @type {unknown} */
  const nodes = JSON.parse(stdout);
  if (!Array.isArray(nodes)) throw new Error(`npm query ${selector} in ${dir} did not print an array`);
  return nodes;
}

/**
 * Fails when a direct dependency of the project is not installed: an empty node_modules would pass every rule.
 * @param {QueryNode[]} nodes a query result that contains the project itself
 * @param {string} dir
 * @param {boolean} dev whether nodes should contain the dev dependencies too (`npm query *`)
 */
export function assertInstalled(nodes, dir, dev) {
  const root = nodes.find((n) => n.location === '');
  const installed = new Set(nodes.map((n) => n.location));
  const direct = { ...root?.dependencies, ...(dev ? root?.devDependencies : {}) };
  const missing = Object.keys(direct).filter((name) => !installed.has(`node_modules/${name}`));
  if (!root || missing.length > 0) {
    throw new Error(`${dir}: not installed: ${missing.join(', ') || 'package.json'}; run npm ci there first`);
  }
}

/**
 * Normalizes a license text: LF line ends, no BOM, no trailing blanks, no leading or trailing empty lines.
 * @param {string} text
 */
export function normalize(text) {
  const lines = text
    .replace(/^\uFEFF/, '')
    .replace(/\r\n?/g, '\n')
    .split('\n')
    .map((l) => l.trimEnd());
  while (lines.length > 0 && lines[0] === '') lines.shift();
  while (lines.length > 0 && lines.at(-1) === '') lines.pop();
  return lines.length > 0 ? `${lines.join('\n')}\n` : '';
}

// The files reproduced for a package, as tools/notices does for Go modules: LICENSE*, LICENCE*, COPYING*,
// UNLICENSE* and NOTICE*, except source files such as license.js.
const LICENSE_FILE = /^((un)?licen[cs]e|copying|notice)/i;
const CODE_EXT = /\.([cm]?js|[cm]?ts|[jt]sx|go|json|ya?ml|toml|sh|py|rb|rs|java|[ch]|cc|cpp|s|css|html?)$/i;
const RULE = '-'.repeat(80);

/**
 * Renders licenses.txt for the shipped packages: one entry per name@version, sorted.
 * @param {QueryNode[]} shipped
 * @param {Overrides} overrides
 * @param {string} projectDir the directory npm query ran in; each package is at its `location` below it (a linked
 *   package's location is node_modules/<name>, and reading it follows the link)
 */
export async function renderNotices(shipped, overrides, projectDir) {
  /** @type {Map<string, QueryNode>} */
  const unique = new Map();
  for (const node of shipped) {
    if (node.location !== '' && !unique.has(idOf(node))) unique.set(idOf(node), node);
  }
  const nodes = [...unique.values()].sort(
    (a, b) => byString(a.name ?? '', b.name ?? '') || byString(a.version ?? '', b.version ?? ''),
  );
  const parts = [
    'isshoni web client: third-party packages\n',
    '\nThe isshoni web client bundles the npm packages listed below. Each entry names the package, its version and\n' +
      'license (an SPDX expression), and reproduces the license and notice files that come with it.\n',
  ];
  for (const node of nodes) {
    const dir = path.join(projectDir, node.location);
    const names = (await readdir(dir, { withFileTypes: true }))
      .filter((e) => e.isFile() && LICENSE_FILE.test(e.name) && !CODE_EXT.test(e.name))
      .map((e) => e.name)
      .sort((a, b) => Number(/^notice/i.test(a)) - Number(/^notice/i.test(b)) || byString(a, b));
    const license = licenseOf(node, overrides) ?? 'none';
    parts.push(`\n${RULE}\n${node.name ?? ''} ${node.version ?? ''}\nLicense: ${license}\n${RULE}\n`);
    if (names.length === 0) {
      parts.push(`\nThe package contains no license file; its package.json declares ${license}.\n`);
    }
    for (const name of names) {
      const text = normalize(await readFile(path.join(dir, name), 'utf8'));
      parts.push(`\n== ${name} ==\n\n${text || '(empty file)\n'}`);
    }
  }
  return parts.join('');
}

const USAGE = 'usage: node web/scripts/licenses.mjs check|notices [--dir web|docs] [--overrides FILE] [--out FILE]';

/**
 * @param {string[]} argv
 * @returns {Promise<number>} the exit status
 */
export async function main(argv) {
  /** @type {{ values: { dir?: string, overrides?: string, out?: string }, positionals: string[] }} */
  let parsed;
  try {
    parsed = parseArgs({
      args: argv,
      allowPositionals: true,
      options: { dir: { type: 'string' }, overrides: { type: 'string' }, out: { type: 'string' } },
    });
  } catch (err) {
    console.error(`licenses: ${err instanceof Error ? err.message : String(err)}\n${USAGE}`);
    return 2;
  }
  const [mode, ...rest] = parsed.positionals;
  if ((mode !== 'check' && mode !== 'notices') || rest.length > 0) {
    console.error(USAGE);
    return 2;
  }
  const dirArg = parsed.values.dir ?? 'web';
  const dir = path.resolve(ROOT, dirArg);
  const out = path.resolve(ROOT, parsed.values.out ?? path.join('web', 'dist', 'licenses.txt'));

  try {
    const overrides = await readOverrides(
      path.resolve(ROOT, parsed.values.overrides ?? path.join('web', 'licenses.overrides.json')),
    );
    const shipped = await npmQuery(dir, '.prod');
    const all = mode === 'check' ? await npmQuery(dir, '*') : shipped;
    assertInstalled(all, dirArg, mode === 'check');
    const result = evaluate({ shipped, all }, overrides);
    if (result.problems.length > 0) {
      console.error(`licenses: ${dirArg}: ${String(result.problems.length)} packages fail the license gate (06 §8.3):`);
      for (const line of formatProblems(result.problems)) console.error(line);
      console.error(`licenses: shipped packages must be under ${ALLOWLIST.join(', ')}`);
      console.error('licenses: a package that states its license only in a file needs web/licenses.overrides.json');
      return 1;
    }
    if (mode === 'check') {
      console.log(
        `licenses: ${dirArg}: ok (${String(result.shipped)} shipped packages allowed, ${String(result.all)} installed packages checked)`,
      );
      return 0;
    }
    await mkdir(path.dirname(out), { recursive: true });
    await writeFile(out, await renderNotices(shipped, overrides, dir));
    const shown = path.relative(ROOT, out);
    console.log(`licenses: wrote ${shown.startsWith('..') ? out : shown} (${String(result.shipped)} packages)`);
    return 0;
  } catch (err) {
    console.error(`licenses: ${err instanceof Error ? err.message : String(err)}`);
    return 1;
  }
}

// import.meta.main, not a comparison with process.argv[1] (see scripts/check-size.mjs).
if (import.meta.main) {
  process.exitCode = await main(process.argv.slice(2));
}
