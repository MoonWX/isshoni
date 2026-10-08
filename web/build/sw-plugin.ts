// Service-worker build (05 §16.2). After the main build has written dist/, this plugin builds src/sw/sw.ts on its
// own into dist/sw.js: one classic script (an IIFE, because module service workers aren't universal) at the root,
// where the server serves it with `no-cache` and the scope `/` (04 §9.5).
//
// The worker gets three build-time constants (src/types/sw-globals.d.ts):
// - __SHELL__ and __SHELL_VERSION__: the app-shell URLs and their hash, from build/shell.ts over the written dist/,
//   so they are exactly what version-plugin.ts writes into version.json as `shell`. sw.js itself is not part of the
//   shell. Any change to a shell file changes the hash, so sw.js changes byte-wise and browsers install the new
//   worker;
// - __PUSH_STRINGS__: the `push` section of src/i18n/en.json, for notification texts.
//
// The closeBundle hook is `sequential`, so compress-plugin (order: post) finds sw.js and writes its .br/.gz siblings.
import { readFile } from 'node:fs/promises';
import path from 'node:path';

import { build, type BuildOptions, type InlineConfig, type Plugin } from 'vite';

import { resolveShell, shellUrls, type Shell } from './shell.ts';

/** The worker's file name in dist/. 04 serves exactly this path at the root; pwa.ts registers it. */
export const SW_FILE = 'sw.js';

export interface SwPluginOptions {
  /** The worker's entry module. Default: src/sw/sw.ts under the Vite root. */
  entry?: string;
  /** The i18n catalog whose `push` section becomes __PUSH_STRINGS__. Default: src/i18n/en.json under the root. */
  catalog?: string;
}

/** A nested group of catalog strings (PushStrings in src/types/sw-globals.d.ts). */
export interface PushStrings {
  readonly [key: string]: string | PushStrings;
}

function isPushStrings(v: unknown): v is PushStrings {
  if (typeof v !== 'object' || v === null || Array.isArray(v)) return false;
  return Object.values(v).every((child) => typeof child === 'string' || isPushStrings(child));
}

/**
 * The `push` section of a catalog file. A catalog without one gives `{}` (the worker then shows its built-in generic
 * notification); a `push` section that isn't nested strings fails the build.
 */
export async function readPushStrings(catalogPath: string): Promise<PushStrings> {
  const catalog: unknown = JSON.parse(await readFile(catalogPath, 'utf8'));
  if (typeof catalog !== 'object' || catalog === null || Array.isArray(catalog)) {
    throw new Error(`isshoni:sw: ${catalogPath} is not a JSON object`);
  }
  const push = (catalog as Record<string, unknown>)['push'];
  if (push === undefined) return {};
  if (!isPushStrings(push)) {
    throw new Error(`isshoni:sw: the "push" section of ${catalogPath} must hold only strings and nested groups`);
  }
  return push;
}

/** The `define` replacements of the worker build: each constant as the JSON text of its value. */
export function swDefine(shell: Shell, pushStrings: PushStrings): Record<string, string> {
  return {
    __SHELL__: JSON.stringify(shell.urls),
    __SHELL_VERSION__: JSON.stringify(shell.version),
    __PUSH_STRINGS__: JSON.stringify(pushStrings),
  };
}

export interface SwBuildOptions {
  /** The Vite root of the main build. */
  root: string;
  /** Absolute path of the worker's entry module. */
  entry: string;
  /** Absolute path of dist/. */
  outDir: string;
  define: Record<string, string>;
  /** Taken from the main build, so the worker runs wherever the SPA does. */
  target: BuildOptions['target'];
  minify: boolean;
  mode: string;
  logLevel: InlineConfig['logLevel'];
}

/**
 * The Vite config of the worker build: no config file, no plugins of ours, no public/ copy and no emptying of dist/
 * (the main build's output is already there); one IIFE chunk named sw.js at the root of dist/.
 */
export function swBuildConfig(opts: SwBuildOptions): InlineConfig {
  return {
    configFile: false,
    envDir: false,
    root: opts.root,
    mode: opts.mode,
    logLevel: opts.logLevel ?? 'info',
    publicDir: false,
    define: opts.define,
    build: {
      outDir: opts.outDir,
      emptyOutDir: false,
      copyPublicDir: false,
      sourcemap: false,
      minify: opts.minify,
      target: opts.target,
      modulePreload: false,
      reportCompressedSize: false,
      rolldownOptions: {
        input: opts.entry,
        output: { format: 'iife', entryFileNames: SW_FILE, codeSplitting: false },
      },
    },
  };
}

/** Builds dist/sw.js once the main bundle and public/ are on disk. */
export function swPlugin(opts: SwPluginOptions = {}): Plugin {
  let root = '';
  let outDir = '';
  let mode = 'production';
  let logLevel: InlineConfig['logLevel'];
  let target: SwBuildOptions['target'];
  let minify = true;
  let urls: string[] | undefined;
  return {
    name: 'isshoni:sw',
    apply: 'build',
    configResolved(config) {
      root = path.resolve(config.root);
      outDir = path.resolve(root, config.build.outDir);
      mode = config.mode;
      logLevel = config.logLevel;
      target = config.build.target;
      minify = config.build.minify !== false;
    },
    writeBundle(_options, bundle) {
      urls = shellUrls(bundle);
    },
    closeBundle: {
      sequential: true,
      async handler() {
        if (!urls) return; // the build failed before writing
        const shell = await resolveShell(outDir, urls);
        const pushStrings = await readPushStrings(path.resolve(root, opts.catalog ?? 'src/i18n/en.json'));
        await build(
          swBuildConfig({
            root,
            entry: path.resolve(root, opts.entry ?? 'src/sw/sw.ts'),
            outDir,
            define: swDefine(shell, pushStrings),
            target,
            minify,
            mode,
            logLevel,
          }),
        );
      },
    },
  };
}
