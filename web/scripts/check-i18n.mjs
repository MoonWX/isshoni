#!/usr/bin/env node
// check:i18n (05 §16.5): fails when the code uses a message key that src/i18n/en.json doesn't have.
//
// Rules, each added by the slice that makes it checkable (05 §23):
// 1. (S09) Every literal key in a t('…') call, and in an i18nKey="…" attribute, is a string in en.json: at its path,
//    or as a plural form (key_one/key_other …, key_ordinal_…). Keys are written in full: a `keyPrefix` option would
//    hide them from this check, so it is an error too.
// 2. (S27) Every ErrorCode (types.gen.ts) and api Code… constant (api.gen.ts) has an errors.<code> entry.
// 3. (S37) Every CloudProvider has fix.firewall.<provider>, every NATKind has conntest.nat.<nat>.
// 4. (S93) Unused keys are warnings.
//
// Usage: node scripts/check-i18n.mjs [--src <dir>] [--catalog <en.json>]
// Paths default to this package's src/ and src/i18n/en.json, so it runs the same from the repo root and from web/.
// Exit 0 when clean, 1 with one line per problem, 2 on bad usage.
import { readdir, readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';

import ts from 'typescript';

const WEB_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

/** i18next plural and ordinal suffixes (Intl.PluralRules categories). */
const PLURAL_SUFFIXES = ['zero', 'one', 'two', 'few', 'many', 'other'].flatMap((c) => [`_${c}`, `_ordinal_${c}`]);

/**
 * @typedef {{ key: string, file: string, line: number, column: number }} KeyUse
 *   A literal key at a 1-based line and column.
 * @typedef {{ file: string, line: number, column: number, message: string }} Problem
 * @typedef {{ [key: string]: string | Catalog }} Catalog
 */

/**
 * Whether a source file is scanned: TypeScript under src/, except declarations, generated files and tests (a test may
 * use a missing key on purpose).
 * @param {string} rel path relative to the src dir, with forward slashes
 */
export function isScanned(rel) {
  if (!/\.(ts|tsx)$/.test(rel)) return false;
  if (rel.endsWith('.d.ts') || rel.endsWith('.gen.ts')) return false;
  if (/\.test\.tsx?$/.test(rel) || rel.startsWith('test/')) return false;
  return true;
}

/**
 * The literal message keys a file uses, and the places that set `keyPrefix`.
 * @param {string} file shown in problems
 * @param {string} text the source
 * @returns {{ keys: KeyUse[], prefixes: Problem[] }}
 */
export function scanSource(file, text) {
  const kind = file.endsWith('.tsx') ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  const sf = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, kind);
  /** @type {KeyUse[]} */
  const keys = [];
  /** @type {Problem[]} */
  const prefixes = [];
  /** @param {ts.Node} node */
  const at = (node) => {
    const { line, character } = sf.getLineAndCharacterOfPosition(node.getStart(sf));
    return { file, line: line + 1, column: character + 1 };
  };
  /** @param {ts.Node} node */
  const visit = (node) => {
    if (ts.isCallExpression(node)) {
      const callee = node.expression;
      const name = ts.isIdentifier(callee)
        ? callee.text
        : ts.isPropertyAccessExpression(callee)
          ? callee.name.text
          : undefined;
      const first = node.arguments[0];
      if (name === 't' && first && ts.isStringLiteralLike(first)) {
        keys.push({ key: first.text, ...at(first) });
      }
    } else if (ts.isJsxAttribute(node) && ts.isIdentifier(node.name) && node.name.text === 'i18nKey') {
      const init = node.initializer;
      const lit = init && ts.isJsxExpression(init) ? init.expression : init;
      if (lit && ts.isStringLiteralLike(lit)) keys.push({ key: lit.text, ...at(lit) });
    } else if (
      (ts.isPropertyAssignment(node) || ts.isShorthandPropertyAssignment(node)) &&
      ts.isIdentifier(node.name) &&
      node.name.text === 'keyPrefix'
    ) {
      prefixes.push({ ...at(node), message: 'keyPrefix hides message keys from check:i18n; write each key in full' });
    }
    ts.forEachChild(node, visit);
  };
  visit(sf);
  return { keys, prefixes };
}

/**
 * The value at a dotted key path, or undefined.
 * @param {Catalog} catalog
 * @param {string} key
 * @returns {string | Catalog | undefined}
 */
export function lookup(catalog, key) {
  /** @type {string | Catalog | undefined} */
  let node = catalog;
  for (const part of key.split('.')) {
    if (typeof node !== 'object' || !Object.hasOwn(node, part)) return undefined;
    node = node[part];
  }
  return node;
}

/**
 * Why a key can't be used with t(), or null when it can: a string at its path, or a plural form of it.
 * @param {Catalog} catalog
 * @param {string} key
 * @returns {string | null}
 */
export function keyProblem(catalog, key) {
  const value = lookup(catalog, key);
  if (typeof value === 'string') return null;
  if (PLURAL_SUFFIXES.some((s) => typeof lookup(catalog, key + s) === 'string')) return null;
  if (value !== undefined) return 'has sub-keys, not a message';
  return 'missing';
}

/**
 * Every .ts/.tsx file under dir that isScanned accepts, as paths relative to dir with forward slashes, sorted.
 * @param {string} dir
 */
async function sourceFiles(dir) {
  const entries = await readdir(dir, { recursive: true, withFileTypes: true });
  return entries
    .filter((e) => e.isFile())
    .map((e) => path.relative(dir, path.join(e.parentPath, e.name)).split(path.sep).join('/'))
    .filter(isScanned)
    .sort();
}

/**
 * Runs every rule.
 * @param {{ srcDir: string, catalogPath: string }} opts
 * @returns {Promise<{ problems: Problem[], keysUsed: number, files: number }>}
 */
export async function checkI18n({ srcDir, catalogPath }) {
  /** @type {Catalog} */
  const catalog = JSON.parse(await readFile(catalogPath, 'utf8'));
  const catalogName = path.relative(process.cwd(), catalogPath) || catalogPath;
  /** @type {Problem[]} */
  const problems = [];
  let keysUsed = 0;
  const files = await sourceFiles(srcDir);
  for (const rel of files) {
    const shown = path.relative(process.cwd(), path.join(srcDir, rel)) || rel;
    const { keys, prefixes } = scanSource(shown, await readFile(path.join(srcDir, rel), 'utf8'));
    problems.push(...prefixes);
    for (const use of keys) {
      keysUsed++;
      const why = keyProblem(catalog, use.key);
      if (why) {
        problems.push({
          file: use.file,
          line: use.line,
          column: use.column,
          message: `message key "${use.key}" ${why} in ${catalogName}`,
        });
      }
    }
  }
  return { problems, keysUsed, files: files.length };
}

async function main() {
  /** @type {{ src?: string, catalog?: string }} */
  let values;
  try {
    ({ values } = parseArgs({ options: { src: { type: 'string' }, catalog: { type: 'string' } } }));
  } catch (err) {
    console.error(`check:i18n: ${err instanceof Error ? err.message : String(err)}`);
    console.error('usage: node scripts/check-i18n.mjs [--src <dir>] [--catalog <en.json>]');
    process.exit(2);
  }
  const srcDir = path.resolve(values.src ?? path.join(WEB_ROOT, 'src'));
  const catalogPath = path.resolve(values.catalog ?? path.join(WEB_ROOT, 'src', 'i18n', 'en.json'));
  const { problems, keysUsed, files } = await checkI18n({ srcDir, catalogPath });
  for (const p of problems) console.error(`${p.file}:${p.line}:${p.column}: ${p.message}`);
  if (problems.length > 0) {
    console.error(`check:i18n: ${problems.length} problem(s)`);
    process.exit(1);
  }
  console.log(`check:i18n: ok (${keysUsed} message keys in ${files} files)`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
