#!/usr/bin/env node
// check:i18n (05 §16.5): fails when the code uses a message key that the catalog doesn't have.
//
// The catalog is src/i18n/en.json, the main bundle's part, and one src/i18n/lazy/<ns>.en.json for each lazy
// namespace: a top-level namespace that comes with the page folders that use it, not with the main bundle
// (src/i18n/index.ts). A lazy file holds its namespace under the namespace's name, `{"admin": {…}}`, and nothing
// else, and en.json doesn't have that namespace. The rules read all the files as one catalog, and a problem names
// the file a key belongs in.
//
// Rules, each added by the slice that makes it checkable (05 §23):
// 1. (S09) Every literal key in a t('…') call, and in an i18nKey="…" attribute, is a string in the catalog: at its
//    path, or as a plural form (key_one/key_other …, key_ordinal_…). Keys are written in full: a `keyPrefix` that
//    i18next or react-i18next reads would hide them from this check, so it is an error too (see keyPrefixSite).
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
// 5. (W00) A lazy namespace is in the catalog before a key of it is asked for, and it is not in the main bundle.
//    src/i18n/lazy/<ns>.ts adds the namespace when a module that imports it runs, so every page folder that uses
//    the namespace imports that module in its entry module (admin/index.ts). The check follows the imports from
//    the app's entry, main.tsx: those that run before the importer's own code (import, export … from, an eager
//    import.meta.glob) and those that the importer runs later (import(), a lazy import.meta.glob). A literal key
//    of a lazy namespace is a problem when its file can be reached from the entry without passing a module that
//    depends on lazy/<ns>.ts through imports of the first kind: on that way nothing adds the namespace, and the
//    page would show the bare key. The tests can't see this, because Vitest loads every namespace up front. A lazy
//    namespace that the entry itself depends on in that way is a problem too (it would be in the main bundle), and
//    so is a lazy file without its lazy/<ns>.ts. Keys that are built at run time are not followed, as in rule 1.
//    A src tree without lazy namespaces (test fixtures) has nothing to check.
//
// Usage: node scripts/check-i18n.mjs [--src <dir>] [--catalog <en.json>]
// Paths default to this package's src/ and src/i18n/en.json, so it runs the same from the repo root and from web/.
// --catalog names the main file. The lazy namespaces are always the ones under <src>/i18n/lazy/, in the language of
// the main file's name (en.json: <ns>.en.json).
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
 * @typedef {{ spec: string, eager: boolean, glob: boolean }} ImportUse
 *   A module that a file imports, as written. `eager`: the import runs before the file's own code (import,
 *   export … from, import.meta.glob with `eager: true`); false for import() and a lazy import.meta.glob, which the
 *   file's code runs later. `glob`: `spec` is an import.meta.glob pattern.
 * @typedef {{ name: string, file: string }} LazyNamespace
 *   A lazy namespace and its catalog file as problems show it.
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
 * Whether a call is import.meta.glob(…).
 * @param {ts.CallExpression} call
 */
function isImportMetaGlob(call) {
  const callee = call.expression;
  return (
    ts.isPropertyAccessExpression(callee) &&
    callee.name.text === 'glob' &&
    ts.isMetaProperty(callee.expression) &&
    callee.expression.keywordToken === ts.SyntaxKind.ImportKeyword &&
    callee.expression.name.text === 'meta'
  );
}

/**
 * The modules a node imports, if it is an import: `import … from`, `export … from`, import('…') and
 * import.meta.glob(…) with literal patterns. Type-only imports and exports load nothing and are left out; so are
 * a glob's negative patterns ('!…') and specifiers that are not literals.
 * @param {ts.Node} node
 * @returns {ImportUse[]}
 */
function importsAt(node) {
  if (ts.isImportDeclaration(node)) {
    // `import { type A } from` still loads the module (verbatimModuleSyntax); `import type` doesn't.
    if (node.importClause?.isTypeOnly || !ts.isStringLiteralLike(node.moduleSpecifier)) return [];
    return [{ spec: node.moduleSpecifier.text, eager: true, glob: false }];
  }
  if (ts.isExportDeclaration(node)) {
    if (node.isTypeOnly || !node.moduleSpecifier || !ts.isStringLiteralLike(node.moduleSpecifier)) return [];
    return [{ spec: node.moduleSpecifier.text, eager: true, glob: false }];
  }
  if (!ts.isCallExpression(node)) return [];
  const [first, second] = node.arguments;
  if (node.expression.kind === ts.SyntaxKind.ImportKeyword) {
    return first && ts.isStringLiteralLike(first) ? [{ spec: first.text, eager: false, glob: false }] : [];
  }
  if (!isImportMetaGlob(node) || !first) return [];
  const eager =
    second !== undefined &&
    ts.isObjectLiteralExpression(second) &&
    second.properties.some(
      (p) =>
        ts.isPropertyAssignment(p) &&
        ts.isIdentifier(p.name) &&
        p.name.text === 'eager' &&
        p.initializer.kind === ts.SyntaxKind.TrueKeyword,
    );
  /** @type {ImportUse[]} */
  const uses = [];
  for (const pattern of ts.isArrayLiteralExpression(first) ? first.elements : [first]) {
    if (ts.isStringLiteralLike(pattern) && !pattern.text.startsWith('!')) {
      uses.push({ spec: pattern.text, eager, glob: true });
    }
  }
  return uses;
}

/**
 * The literal message keys a file uses, the places that set an i18n `keyPrefix` (keyPrefixSite), and the modules
 * the file imports (importsAt).
 * @param {string} file shown in problems
 * @param {string} text the source
 * @returns {{ keys: KeyUse[], prefixes: Problem[], imports: ImportUse[] }}
 */
export function scanSource(file, text) {
  const kind = file.endsWith('.tsx') ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  const sf = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, kind);
  /** @type {KeyUse[]} */
  const keys = [];
  /** @type {Problem[]} */
  const prefixes = [];
  /** @type {ImportUse[]} */
  const imports = [];
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
    imports.push(...importsAt(node));
    ts.forEachChild(node, visit);
  };
  visit(sf);
  return { keys, prefixes, imports };
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
 * Scans every source file under a src dir (isScanned).
 * @param {string} srcDir
 * @returns {Promise<{ uses: (KeyUse & { module: string })[], prefixes: Problem[],
 *   imports: Map<string, ImportUse[]>, shown: (module: string) => string }>}
 *   uses: every literal key, with the file it is in as a path relative to the src dir with forward slashes
 *   (`module`); prefixes: the keyPrefix problems; imports: what each file imports, by that path, with an entry for
 *   every scanned file; shown: such a path as problems name the file
 */
export async function scanTree(srcDir) {
  /** @param {string} module */
  const shown = (module) => path.relative(process.cwd(), path.join(srcDir, module)) || module;
  /** @type {(KeyUse & { module: string })[]} */
  const uses = [];
  /** @type {Problem[]} */
  const prefixes = [];
  /** @type {Map<string, ImportUse[]>} */
  const imports = new Map();
  for (const module of await sourceFiles(srcDir)) {
    const scan = scanSource(shown(module), await readFile(path.join(srcDir, module), 'utf8'));
    prefixes.push(...scan.prefixes);
    imports.set(module, scan.imports);
    for (const use of scan.keys) uses.push({ ...use, module });
  }
  return { uses, prefixes, imports, shown };
}

/**
 * Reads the catalog: the main file, and the lazy namespaces under <srcDir>/i18n/lazy/ in the main file's language
 * (`<ns>.en.json` next to en.json's sources), merged into one. A lazy file is a problem when it holds anything but
 * its own namespace, `{"<ns>": {…}}`, or when the main file has that namespace too; such a file is left out.
 * @param {{ srcDir: string, catalogPath: string }} opts
 * @returns {Promise<{ catalog: Catalog, lazy: LazyNamespace[], fileOf: (key: string) => string,
 *   problems: Problem[] }>} fileOf: the file a key belongs in, as problems show it
 */
export async function readCatalog({ srcDir, catalogPath }) {
  /** @param {string} p */
  const shown = (p) => path.relative(process.cwd(), p) || p;
  /** @type {Catalog} */
  const catalog = JSON.parse(await readFile(catalogPath, 'utf8'));
  const mainName = shown(catalogPath);
  const suffix = `.${path.basename(catalogPath, '.json')}.json`;
  const lazyDir = path.join(srcDir, 'i18n', 'lazy');
  const names = (await exists(lazyDir)) ? await readdir(lazyDir) : [];
  /** @type {LazyNamespace[]} */
  const lazy = [];
  /** @type {Problem[]} */
  const problems = [];
  for (const fileName of names.filter((n) => n.endsWith(suffix)).sort()) {
    const name = fileName.slice(0, -suffix.length);
    const file = shown(path.join(lazyDir, fileName));
    /** @type {unknown} */
    const content = JSON.parse(await readFile(path.join(lazyDir, fileName), 'utf8'));
    /** @param {string} message */
    const problem = (message) => problems.push({ file, line: 1, column: 1, message });
    const keys = typeof content === 'object' && content !== null && !Array.isArray(content) ? Object.keys(content) : [];
    if (keys.length !== 1 || keys[0] !== name) {
      problem(`a lazy namespace file holds its own namespace and nothing else: {"${name}": {…}}`);
    } else if (Object.hasOwn(catalog, name)) {
      problem(`the namespace "${name}" is in ${mainName} too: keep it in one of the two files`);
    } else {
      catalog[name] = /** @type {Catalog} */ (content)[name] ?? {};
      lazy.push({ name, file });
    }
  }
  /** @param {string} key */
  const fileOf = (key) => lazy.find((ns) => key === ns.name || key.startsWith(`${ns.name}.`))?.file ?? mainName;
  return { catalog, lazy, fileOf, problems };
}

/** The app's entry module, relative to the src dir (index.html loads it). */
export const ENTRY_MODULE = 'main.tsx';

/**
 * The module that adds a lazy namespace to the catalog, relative to the src dir.
 * @param {string} name
 */
export const lazyModule = (name) => `i18n/lazy/${name}.ts`;

/**
 * An import.meta.glob pattern as a regular expression over paths: `**` crosses folders, `*` and `?` stay inside a
 * name, `{a,b}` is one of the alternatives.
 * @param {string} pattern
 */
function globToRegExp(pattern) {
  let re = '';
  let depth = 0;
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern.charAt(i);
    if (c === '*' && pattern.charAt(i + 1) === '*') {
      const slash = pattern.charAt(i + 2) === '/';
      re += slash ? '(?:.*/)?' : '.*';
      i += slash ? 2 : 1;
    } else if (c === '*') {
      re += '[^/]*';
    } else if (c === '?') {
      re += '[^/]';
    } else if (c === '{') {
      depth++;
      re += '(?:';
    } else if (c === '}' && depth > 0) {
      depth--;
      re += ')';
    } else if (c === ',' && depth > 0) {
      re += '|';
    } else {
      re += c.replace(/[\\^$.*+?()[\]{}|]/, '\\$&');
    }
  }
  return new RegExp(`^${re}$`);
}

/**
 * The source files an import leads to: the file a relative specifier names (as written, with .ts or .tsx, or its
 * index.ts or index.tsx), or every file a glob pattern matches. Packages, styles, JSON and anything else outside
 * `files` lead nowhere.
 * @param {string} from the importing file, relative to the src dir with forward slashes
 * @param {ImportUse} use
 * @param {ReadonlySet<string>} files every source file, in that form
 * @returns {string[]}
 */
export function importTargets(from, use, files) {
  if (!use.spec.startsWith('.')) return [];
  const dir = path.posix.dirname(from);
  if (use.glob) {
    const re = globToRegExp(path.posix.normalize(path.posix.join(dir, use.spec)));
    return [...files].filter((f) => re.test(f));
  }
  // Without a query (`./worker?worker`).
  const target = path.posix.normalize(path.posix.join(dir, use.spec.replace(/[?#].*$/, '')));
  const found = ['', '.ts', '.tsx', '/index.ts', '/index.tsx'].map((s) => target + s).find((f) => files.has(f));
  return found === undefined ? [] : [found];
}

/**
 * Everything that can be reached from `start` along `edges`, without entering a node of `closed`, each with the
 * node it was first reached from (start: undefined).
 * @param {string} start
 * @param {ReadonlyMap<string, readonly string[]>} edges
 * @param {ReadonlySet<string>} [closed]
 * @returns {Map<string, string | undefined>}
 */
function reach(start, edges, closed = new Set()) {
  /** @type {Map<string, string | undefined>} */
  const from = new Map();
  if (closed.has(start)) return from;
  from.set(start, undefined);
  const queue = [start];
  for (let node = queue.shift(); node !== undefined; node = queue.shift()) {
    for (const next of edges.get(node) ?? []) {
      if (from.has(next) || closed.has(next)) continue;
      from.set(next, node);
      queue.push(next);
    }
  }
  return from;
}

/**
 * The way reach() took to a node: "main.tsx → app/boot.tsx → …".
 * @param {ReadonlyMap<string, string | undefined>} from
 * @param {string} node
 */
function wayTo(from, node) {
  const way = [node];
  for (let prev = from.get(node); prev !== undefined; prev = from.get(prev)) way.unshift(prev);
  return way.join(' → ');
}

/**
 * Rule 5: a lazy namespace is there before a key of it is asked for, and it is not in the main bundle.
 * @param {{
 *   lazy: readonly LazyNamespace[],
 *   imports: ReadonlyMap<string, readonly ImportUse[]>,
 *   uses: readonly (KeyUse & { module: string })[],
 *   entry?: string,
 *   shown?: (module: string) => string,
 * }} opts
 *   imports: what each source file imports, by its path relative to the src dir with forward slashes; uses: every
 *   literal key with the file it is in, in that form (`module`); entry: default ENTRY_MODULE; shown: a module as
 *   problems name its file
 * @returns {Problem[]}
 */
export function checkLazyNamespaces({ lazy, imports, uses, entry = ENTRY_MODULE, shown = (m) => m }) {
  if (lazy.length === 0) return [];
  /** @type {Problem[]} */
  const problems = [];
  const files = new Set(imports.keys());
  if (!files.has(entry)) {
    const names = lazy.map((ns) => ns.name).join(', ');
    return [
      {
        file: shown(entry),
        line: 1,
        column: 1,
        message: `no ${entry}: can't tell what runs before ${names} is loaded`,
      },
    ];
  }
  /** @type {Map<string, string[]>} every import; the eager ones; and the eager ones backwards */
  const all = new Map();
  /** @type {Map<string, string[]>} */
  const eager = new Map();
  /** @type {Map<string, string[]>} */
  const eagerBack = new Map();
  /**
   * @param {Map<string, string[]>} edges
   * @param {string} a
   * @param {string} b
   */
  const link = (edges, a, b) => {
    const list = edges.get(a);
    if (list) list.push(b);
    else edges.set(a, [b]);
  };
  for (const [file, list] of imports) {
    for (const use of list) {
      for (const target of importTargets(file, use, files)) {
        link(all, file, target);
        if (use.eager) {
          link(eager, file, target);
          link(eagerBack, target, file);
        }
      }
    }
  }
  for (const ns of lazy) {
    const adder = lazyModule(ns.name);
    if (!files.has(adder)) {
      problems.push({
        file: ns.file,
        line: 1,
        column: 1,
        message: `no ${adder} adds the lazy namespace "${ns.name}" to the catalog (see i18n/index.ts)`,
      });
      continue;
    }
    // Whoever depends on the adding module through eager imports has the namespace by the time its own code runs.
    const loaded = new Set(reach(adder, eagerBack).keys());
    if (loaded.has(entry)) {
      problems.push({
        file: shown(entry),
        line: 1,
        column: 1,
        message: `the lazy namespace "${ns.name}" is in the main bundle: ${wayTo(reach(entry, eager), adder)}`,
      });
      continue;
    }
    const without = reach(entry, all, loaded);
    for (const use of uses) {
      if (use.key !== ns.name && !use.key.startsWith(`${ns.name}.`)) continue;
      if (!without.has(use.module)) continue;
      problems.push({
        file: use.file,
        line: use.line,
        column: use.column,
        message:
          `message key "${use.key}" is in the lazy namespace "${ns.name}", which nothing loads on the way ` +
          `${wayTo(without, use.module)}: the entry module of the folder that uses it must import ${adder}`,
      });
    }
  }
  return problems;
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
 * The catalog file a problem names for a key: the same one for every key, or the one readCatalog's fileOf gives.
 * @typedef {string | ((key: string) => string)} CatalogName
 */

/**
 * @param {CatalogName} catalogName
 * @param {string} key
 */
const fileFor = (catalogName, key) => (typeof catalogName === 'function' ? catalogName(key) : catalogName);

/**
 * Rule 2: every ErrorCode (protocol/types.gen.ts) and api Code (protocol/api.gen.ts) has an errors.<code> entry.
 * @param {{ srcDir: string, catalog: Catalog, catalogName: CatalogName }} opts
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
        message: `error code "${c.value}" (${c.name}) ${why} in ${fileFor(catalogName, `errors.${c.value}`)}`,
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
 * @param {{ srcDir: string, catalog: Catalog, catalogName: CatalogName }} opts
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
        const file = fileFor(catalogName, key);
        problems.push({
          file: c.file,
          line: c.line,
          column: c.column,
          message:
            why === 'missing'
              ? `${family.what} "${c.value}" (${c.name}) has no ${key} in ${file}`
              : `${family.what} "${c.value}" (${c.name}): ${key} ${why} in ${file}`,
        });
      }
    }
  }
  return { problems, counts };
}

/**
 * Runs every rule.
 * @param {{ srcDir: string, catalogPath: string }} opts catalogPath: the main catalog file (readCatalog)
 * @returns {Promise<{ problems: Problem[], keysUsed: number, files: number, errorCodes: number | null,
 *   constantKeys: Record<string, number> | null, lazyNamespaces: string[] }>}
 *   errorCodes: the distinct error codes rule 2 checked, or null when it was skipped; constantKeys: the constants
 *   rule 3 checked per union type name (CloudProvider, NATKind), or null when it was skipped; lazyNamespaces: the
 *   lazy namespaces rule 5 checked
 */
export async function checkI18n({ srcDir, catalogPath }) {
  const { catalog, lazy, fileOf, problems } = await readCatalog({ srcDir, catalogPath });
  const tree = await scanTree(srcDir);
  problems.push(...tree.prefixes);
  for (const use of tree.uses) {
    const why = keyProblem(catalog, use.key);
    if (why) {
      problems.push({
        file: use.file,
        line: use.line,
        column: use.column,
        message: `message key "${use.key}" ${why} in ${fileOf(use.key)}`,
      });
    }
  }
  const errorCodes = await checkErrorCodes({ srcDir, catalog, catalogName: fileOf });
  problems.push(...errorCodes.problems);
  const constantKeys = await checkConstantKeys({ srcDir, catalog, catalogName: fileOf });
  problems.push(...constantKeys.problems);
  problems.push(...checkLazyNamespaces({ lazy, imports: tree.imports, uses: tree.uses, shown: tree.shown }));
  return {
    problems,
    keysUsed: tree.uses.length,
    files: tree.imports.size,
    errorCodes: errorCodes.codes,
    constantKeys: constantKeys.counts,
    lazyNamespaces: lazy.map((ns) => ns.name),
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
  const { problems, keysUsed, files, errorCodes, constantKeys, lazyNamespaces } = await checkI18n({
    srcDir,
    catalogPath,
  });
  for (const p of problems) console.error(`${p.file}:${p.line}:${p.column}: ${p.message}`);
  if (problems.length > 0) {
    console.error(`check:i18n: ${problems.length} problem(s)`);
    process.exit(1);
  }
  if (lazyNamespaces.length > 0) {
    console.log(`check:i18n: lazy namespaces, each loaded before its keys are used: ${lazyNamespaces.join(', ')}`);
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
