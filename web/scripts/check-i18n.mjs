#!/usr/bin/env node
// check:i18n (05 §16.5): fails when the code uses a message key that src/i18n/en.json doesn't have.
//
// Rules, each added by the slice that makes it checkable (05 §23):
// 1. (S09) Every literal key in a t('…') call, and in an i18nKey="…" attribute, is a string in en.json: at its path,
//    or as a plural form (key_one/key_other …, key_ordinal_…). Keys are written in full: a `keyPrefix` that i18next
//    or react-i18next reads would hide them from this check, so it is an error too (see keyPrefixSite).
// 2. (S27) Every ErrorCode (types.gen.ts, 01) and every api Code… constant (api.gen.ts, 03 and 04) has an
//    errors.<code> entry: a string, plural forms, or an object with messages below it (errors.rate_limited.wait).
//    Some codes need one sub-key per value of a constant list: errors.limit_reached.<LimitKind> (SUB_KEYED). The
//    constants are read from the generated union types (`export type ErrorCode = typeof A | typeof B …`), so a code
//    added in Go and regenerated with `task gen` fails the check until en.json has its text. A src tree without
//    protocol/types.gen.ts and protocol/api.gen.ts (test fixtures) skips the rule with a note; one without the other
//    is a problem.
// 3. (S37) Every CloudProvider constant (api.gen.ts, 04 §13.3) has a fix.firewall.<provider> message and every
//    NATKind constant (04 §7.4) a conntest.nat.<nat> message: the connection test's fix text (05 §14.2), whose keys
//    are built from the server's ids and so never appear as t('…') literals (CONSTANT_KEYED). The constants are read
//    from the generated unions like rule 2's, so a provider added in Go and regenerated with `task gen` fails the
//    check until en.json has its text. A src tree that has neither union in api.gen.ts nor either message family in
//    its catalog (test fixtures) skips the rule with a note. Anything in between is a problem: a union missing next
//    to the other one, or next to a catalog that has these texts (a renamed Go type must not turn the rule off).
// 4. (S93) Unused keys are warnings.
//
// Usage: node scripts/check-i18n.mjs [--src <dir>] [--catalog <en.json>]
// Paths default to this package's src/ and src/i18n/en.json, so it runs the same from the repo root and from web/.
// Exit 0 when clean, 1 with one line per problem, 2 on bad usage.
import { access, readdir, readFile } from 'node:fs/promises';
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

/** Calls that read `keyPrefix` from an options object argument: t(key, opts), useTranslation(ns, opts), … */
const KEY_PREFIX_OPTION_CALLS = new Set(['t', 'useTranslation', 'withTranslation', 'keyFromSelector']);
/** Calls that take i18next init options, whose `react` object sets react-i18next's default `keyPrefix`. */
const INIT_CALLS = new Set(['init', 'createInstance', 'cloneInstance']);
/** getFixedT(lng, ns, keyPrefix): the positional keyPrefix argument. */
const GET_FIXED_T_KEY_PREFIX_ARG = 2;

/**
 * The name a call is made by: `f(…)` → f, `a.b.f(…)` → f.
 * @param {ts.CallExpression} call
 * @returns {string | undefined}
 */
function calleeName(call) {
  const callee = call.expression;
  if (ts.isIdentifier(callee)) return callee.text;
  if (ts.isPropertyAccessExpression(callee)) return callee.name.text;
  return undefined;
}

/**
 * The node an expression stands for once parentheses and type assertions (`as`, `satisfies`, `!`, `<T>`) are
 * stripped from around it: the node whose parent gives the expression its role.
 * @param {ts.Node} node
 */
function outermost(node) {
  let n = node;
  while (
    ts.isParenthesizedExpression(n.parent) ||
    ts.isAsExpression(n.parent) ||
    ts.isSatisfiesExpression(n.parent) ||
    ts.isNonNullExpression(n.parent) ||
    ts.isTypeAssertionExpression(n.parent)
  ) {
    n = n.parent;
  }
  return n;
}

/**
 * Whether an object literal is an argument of a call whose name is in names.
 * @param {ts.ObjectLiteralExpression} obj
 * @param {Set<string>} names
 */
function isOptionsArgOf(obj, names) {
  const arg = outermost(obj);
  const call = arg.parent;
  if (!ts.isCallExpression(call) || !call.arguments.some((a) => a === arg)) return false;
  const name = calleeName(call);
  return name !== undefined && names.has(name);
}

/**
 * Whether obj is an options object that i18next or react-i18next reads `keyPrefix` from:
 * - t(key, { keyPrefix }), useTranslation(ns, { keyPrefix }), withTranslation(ns, { keyPrefix }),
 *   keyFromSelector(fn, { keyPrefix });
 * - i18next.init({ react: { keyPrefix } }) (also createInstance and cloneInstance): useTranslation's default;
 * - <Trans tOptions={{ keyPrefix }}>.
 * The same name elsewhere (a storage wrapper's `keyPrefix: 'isshoni.'`) is not an i18n option. An options object
 * built elsewhere and passed by name is not followed.
 * @param {ts.ObjectLiteralExpression} obj
 */
function isI18nOptions(obj) {
  if (isOptionsArgOf(obj, KEY_PREFIX_OPTION_CALLS)) return true;
  const holder = outermost(obj).parent;
  if (
    ts.isPropertyAssignment(holder) &&
    ts.isIdentifier(holder.name) &&
    holder.name.text === 'react' &&
    ts.isObjectLiteralExpression(holder.parent) &&
    isOptionsArgOf(holder.parent, INIT_CALLS)
  ) {
    return true;
  }
  return (
    ts.isJsxExpression(holder) &&
    ts.isJsxAttribute(holder.parent) &&
    ts.isIdentifier(holder.parent.name) &&
    holder.parent.name.text === 'tOptions'
  );
}

/**
 * Where a node sets a `keyPrefix` that i18next or react-i18next reads, or undefined: an i18n options property
 * (isI18nOptions), the keyPrefix attribute of react-i18next's <Translation>, or getFixedT's third argument when it
 * isn't undefined or null.
 * @param {ts.Node} node
 * @returns {ts.Node | undefined}
 */
function keyPrefixSite(node) {
  if (
    (ts.isPropertyAssignment(node) || ts.isShorthandPropertyAssignment(node)) &&
    ts.isIdentifier(node.name) &&
    node.name.text === 'keyPrefix'
  ) {
    return ts.isObjectLiteralExpression(node.parent) && isI18nOptions(node.parent) ? node : undefined;
  }
  if (ts.isJsxAttribute(node) && ts.isIdentifier(node.name) && node.name.text === 'keyPrefix') {
    const tag = node.parent.parent.tagName; // JsxAttribute → JsxAttributes → JsxOpeningElement | JsxSelfClosingElement
    const tagName = ts.isIdentifier(tag) ? tag.text : ts.isPropertyAccessExpression(tag) ? tag.name.text : undefined;
    return tagName === 'Translation' ? node : undefined;
  }
  if (ts.isCallExpression(node) && calleeName(node) === 'getFixedT') {
    const arg = node.arguments[GET_FIXED_T_KEY_PREFIX_ARG];
    const unset =
      arg === undefined ||
      arg.kind === ts.SyntaxKind.NullKeyword ||
      (ts.isIdentifier(arg) && arg.text === 'undefined') ||
      ts.isVoidExpression(arg);
    return unset ? undefined : arg;
  }
  return undefined;
}

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
 * The literal message keys a file uses, and the places that set an i18n `keyPrefix` (keyPrefixSite).
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
      const first = node.arguments[0];
      if (calleeName(node) === 't' && first && ts.isStringLiteralLike(first)) {
        keys.push({ key: first.text, ...at(first) });
      }
    } else if (ts.isJsxAttribute(node) && ts.isIdentifier(node.name) && node.name.text === 'i18nKey') {
      const init = node.initializer;
      const lit = init && ts.isJsxExpression(init) ? init.expression : init;
      if (lit && ts.isStringLiteralLike(lit)) keys.push({ key: lit.text, ...at(lit) });
    }
    const prefix = keyPrefixSite(node);
    if (prefix) {
      prefixes.push({ ...at(prefix), message: 'keyPrefix hides message keys from check:i18n; write each key in full' });
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
 * @typedef {{ name: string, value: string, file: string, line: number, column: number }} Constant
 *   A string constant of a generated union type, at its declaration.
 */

/**
 * The string constants of a union type in a generated file: for `export type T = typeof A | typeof B;` the values of
 * `export const A = "a";` and `export const B = "b";` (with or without a type annotation), in union order. Returns
 * undefined when the file declares no type alias of that name; union members that aren't `typeof <string const>`
 * become problems.
 * @param {string} file shown in problems
 * @param {string} text the generated source
 * @param {string} typeName
 * @returns {{ constants: Constant[], problems: Problem[] } | undefined}
 */
export function unionConstants(file, text, typeName) {
  const sf = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  /** @param {ts.Node} node */
  const at = (node) => {
    const { line, character } = sf.getLineAndCharacterOfPosition(node.getStart(sf));
    return { file, line: line + 1, column: character + 1 };
  };
  /** @type {Map<string, Constant>} */
  const consts = new Map();
  /** @type {ts.TypeAliasDeclaration | undefined} */
  let alias;
  for (const stmt of sf.statements) {
    if (ts.isVariableStatement(stmt)) {
      for (const decl of stmt.declarationList.declarations) {
        if (ts.isIdentifier(decl.name) && decl.initializer && ts.isStringLiteralLike(decl.initializer)) {
          consts.set(decl.name.text, { name: decl.name.text, value: decl.initializer.text, ...at(decl.name) });
        }
      }
    } else if (ts.isTypeAliasDeclaration(stmt) && stmt.name.text === typeName) {
      alias = stmt;
    }
  }
  if (!alias) return undefined;
  const members = ts.isUnionTypeNode(alias.type) ? alias.type.types : [alias.type];
  /** @type {Constant[]} */
  const constants = [];
  /** @type {Problem[]} */
  const problems = [];
  for (const m of members) {
    const name = ts.isTypeQueryNode(m) && ts.isIdentifier(m.exprName) ? m.exprName.text : undefined;
    const c = name === undefined ? undefined : consts.get(name);
    if (c) {
      constants.push(c);
    } else {
      problems.push({ ...at(m), message: `${typeName} member is not \`typeof\` a string constant in this file` });
    }
  }
  return { constants, problems };
}

/**
 * Error codes whose errors.<code> entry needs one sub-key per constant of a union in api.gen.ts (05 §16.5):
 * limit_reached shows errors.limit_reached.<params.limit>.
 * @type {Readonly<Record<string, string>>}
 */
export const SUB_KEYED = Object.freeze({ limit_reached: 'LimitKind' });

/**
 * Whether a catalog node holds at least one message (a string leaf).
 * @param {string | Catalog | undefined} node
 * @returns {boolean}
 */
function hasMessages(node) {
  if (typeof node === 'string') return true;
  if (node === undefined) return false;
  return Object.values(node).some(hasMessages);
}

/**
 * Why errors.<code> can't show a code, or null when it can: a string, plural forms, or an object with messages.
 * With subKeys, the entry must be an object with a string at every errors.<code>.<subKey>.
 * @param {Catalog} catalog
 * @param {string} code
 * @param {string[]} [subKeys]
 * @returns {string | null}
 */
export function errorEntryProblem(catalog, code, subKeys) {
  const key = `errors.${code}`;
  if (subKeys) {
    const missing = subKeys.filter((s) => typeof lookup(catalog, `${key}.${s}`) !== 'string');
    return missing.length === 0 ? null : `needs ${missing.map((s) => `${key}.${s}`).join(', ')}`;
  }
  if (keyProblem(catalog, key) === null) return null;
  return hasMessages(lookup(catalog, key)) ? null : `has no ${key}`;
}

/**
 * @param {string} p
 * @returns {Promise<boolean>}
 */
async function exists(p) {
  try {
    await access(p);
    return true;
  } catch {
    return false;
  }
}

/**
 * Rule 2: every ErrorCode (protocol/types.gen.ts) and api Code (protocol/api.gen.ts) has an errors.<code> entry.
 * @param {{ srcDir: string, catalog: Catalog, catalogName: string }} opts
 * @returns {Promise<{ problems: Problem[], codes: number | null }>} codes: the distinct codes checked, or null when
 *   the src tree has neither generated file (the rule is skipped)
 */
export async function checkErrorCodes({ srcDir, catalog, catalogName }) {
  const typesPath = path.join(srcDir, 'protocol', 'types.gen.ts');
  const apiPath = path.join(srcDir, 'protocol', 'api.gen.ts');
  const [hasTypes, hasApi] = await Promise.all([exists(typesPath), exists(apiPath)]);
  if (!hasTypes && !hasApi) return { problems: [], codes: null };
  /** @type {Problem[]} */
  const problems = [];
  /** @param {string} p */
  const shown = (p) => path.relative(process.cwd(), p) || p;
  /**
   * @param {string} p
   * @param {boolean} present
   * @param {string[]} typeNames
   * @returns {Promise<Map<string, Constant[]>>}
   */
  const read = async (p, present, typeNames) => {
    /** @type {Map<string, Constant[]>} */
    const out = new Map();
    if (!present) {
      problems.push({ file: shown(p), line: 1, column: 1, message: 'generated file missing: run `task gen`' });
      return out;
    }
    const text = await readFile(p, 'utf8');
    for (const typeName of typeNames) {
      const res = unionConstants(shown(p), text, typeName);
      if (!res) {
        problems.push({ file: shown(p), line: 1, column: 1, message: `no \`export type ${typeName}\` union found` });
        continue;
      }
      problems.push(...res.problems);
      out.set(typeName, res.constants);
    }
    return out;
  };
  const wire = await read(typesPath, hasTypes, ['ErrorCode']);
  const rest = await read(apiPath, hasApi, ['Code', ...new Set(Object.values(SUB_KEYED))]);

  /** @type {Set<string>} */
  const seen = new Set();
  for (const c of [...(wire.get('ErrorCode') ?? []), ...(rest.get('Code') ?? [])]) {
    if (seen.has(c.value)) continue; // a code shared by 01 and 03 (bad_request, internal, …) needs one entry
    seen.add(c.value);
    const subType = Object.hasOwn(SUB_KEYED, c.value) ? SUB_KEYED[c.value] : undefined;
    const subKeys = subType === undefined ? undefined : (rest.get(subType) ?? []).map((s) => s.value);
    const why = errorEntryProblem(catalog, c.value, subKeys);
    if (why) {
      problems.push({
        file: c.file,
        line: c.line,
        column: c.column,
        message: `error code "${c.value}" (${c.name}) ${why} in ${catalogName}`,
      });
    }
  }
  return { problems, codes: seen.size };
}

/**
 * Rule 3: the message families that have one key per constant of a union in api.gen.ts (05 §16.5). `what` names a
 * constant in problems and, with an "s", the count in the summary line.
 * @type {readonly Readonly<{ typeName: string, prefix: string, what: string }>[]}
 */
export const CONSTANT_KEYED = Object.freeze([
  Object.freeze({ typeName: 'CloudProvider', prefix: 'fix.firewall', what: 'cloud provider' }),
  Object.freeze({ typeName: 'NATKind', prefix: 'conntest.nat', what: 'NAT kind' }),
]);

/**
 * Rule 3: every CloudProvider constant has a fix.firewall.<provider> message and every NATKind constant a
 * conntest.nat.<nat> message (a string, or plural forms: these keys are shown as they are).
 * @param {{ srcDir: string, catalog: Catalog, catalogName: string }} opts
 * @returns {Promise<{ problems: Problem[], counts: Record<string, number> | null }>} counts: the constants checked
 *   per union type name, or null when the rule was skipped: no protocol/api.gen.ts (rule 2 reports a lone missing
 *   file), or one that declares none of the unions while the catalog has none of the families either
 */
export async function checkConstantKeys({ srcDir, catalog, catalogName }) {
  const apiPath = path.join(srcDir, 'protocol', 'api.gen.ts');
  if (!(await exists(apiPath))) return { problems: [], counts: null };
  const shown = path.relative(process.cwd(), apiPath) || apiPath;
  const text = await readFile(apiPath, 'utf8');
  const unions = CONSTANT_KEYED.map((family) => ({ family, res: unionConstants(shown, text, family.typeName) }));
  const inCatalog = CONSTANT_KEYED.some((family) => lookup(catalog, family.prefix) !== undefined);
  if (unions.every((u) => u.res === undefined) && !inCatalog) return { problems: [], counts: null };
  /** @type {Problem[]} */
  const problems = [];
  /** @type {Record<string, number>} */
  const counts = {};
  for (const { family, res } of unions) {
    if (!res) {
      problems.push({ file: shown, line: 1, column: 1, message: `no \`export type ${family.typeName}\` union found` });
      continue;
    }
    problems.push(...res.problems);
    counts[family.typeName] = res.constants.length;
    for (const c of res.constants) {
      const key = `${family.prefix}.${c.value}`;
      const why = keyProblem(catalog, key);
      if (why) {
        problems.push({
          file: c.file,
          line: c.line,
          column: c.column,
          message:
            why === 'missing'
              ? `${family.what} "${c.value}" (${c.name}) has no ${key} in ${catalogName}`
              : `${family.what} "${c.value}" (${c.name}): ${key} ${why} in ${catalogName}`,
        });
      }
    }
  }
  return { problems, counts };
}

/**
 * Runs every rule.
 * @param {{ srcDir: string, catalogPath: string }} opts
 * @returns {Promise<{ problems: Problem[], keysUsed: number, files: number, errorCodes: number | null,
 *   constantKeys: Record<string, number> | null }>}
 *   errorCodes: the distinct error codes rule 2 checked, or null when it was skipped; constantKeys: the constants
 *   rule 3 checked per union type name (CloudProvider, NATKind), or null when it was skipped
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
  const errorCodes = await checkErrorCodes({ srcDir, catalog, catalogName });
  problems.push(...errorCodes.problems);
  const constantKeys = await checkConstantKeys({ srcDir, catalog, catalogName });
  problems.push(...constantKeys.problems);
  return {
    problems,
    keysUsed,
    files: files.length,
    errorCodes: errorCodes.codes,
    constantKeys: constantKeys.counts,
  };
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
  const { problems, keysUsed, files, errorCodes, constantKeys } = await checkI18n({ srcDir, catalogPath });
  for (const p of problems) console.error(`${p.file}:${p.line}:${p.column}: ${p.message}`);
  if (problems.length > 0) {
    console.error(`check:i18n: ${problems.length} problem(s)`);
    process.exit(1);
  }
  let summary = `check:i18n: ok (${keysUsed} message keys in ${files} files)`;
  if (errorCodes === null) {
    console.log('check:i18n: note: no protocol/*.gen.ts under the src dir, error-code rule skipped');
  } else {
    summary += `, ${errorCodes} error codes`;
  }
  if (constantKeys === null) {
    const unions = CONSTANT_KEYED.map((f) => f.typeName).join(' or ');
    const families = CONSTANT_KEYED.map((f) => `${f.prefix}.*`).join(' or ');
    console.log(`check:i18n: note: no ${unions} in protocol/api.gen.ts and no ${families} text, fix-text rule skipped`);
  } else {
    for (const f of CONSTANT_KEYED) summary += `, ${constantKeys[f.typeName] ?? 0} ${f.what}s`;
  }
  console.log(summary);
}

// import.meta.main, not a comparison with process.argv[1]: that path isn't resolved through symlinks, junctions or
// subst drives, and a check that silently skips main() would pass without checking anything.
if (import.meta.main) {
  await main();
}
