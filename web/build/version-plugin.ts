// The build version (05 §17.1) and dist/version.json.
//
// One build has one version (04 §15, 06 §7.2): 06's Taskfile passes the same string to the Go binary (ldflags) and to
// this build (env ISSHONI_VERSION). The SPA gets it as __ISSHONI_VERSION__ (01's BUILD_VERSION); the server compares
// version.json's `version` with its own at startup; UpdatePill (S38) polls the file for a new `shell`.
import { writeFile } from 'node:fs/promises';
import path from 'node:path';

import type { Plugin } from 'vite';

import { resolveShell, shellUrls } from './shell.ts';

/** The version of a build without ISSHONI_VERSION: a dev build (04 §15: the prerelease starts with `dev`). */
export const DEFAULT_VERSION = '0.0.0-dev';

/** Signaling protocol version (01 §8.2: `hello.protocol`, `welcome.protocol`). Bump it together with 01's. */
export const PROTOCOL_VERSION = 1;

// SemVer 2.0.0 without a leading "v" (releases pass the tag without it, 06 §7.2).
const SEMVER =
  /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;

/**
 * The version of this build: env ISSHONI_VERSION, or DEFAULT_VERSION when it is unset or empty. Throws when it is not
 * SemVer (for example a tag with its "v"), so a wrong value fails the build instead of shipping a SPA that the server
 * would call stale.
 */
export function resolveBuildVersion(env: Readonly<Record<string, string | undefined>> = process.env): string {
  const raw = env['ISSHONI_VERSION']?.trim() ?? '';
  if (raw === '') return DEFAULT_VERSION;
  if (!SEMVER.test(raw)) {
    throw new Error(
      `ISSHONI_VERSION=${JSON.stringify(raw)} is not a SemVer version without a leading "v" (e.g. 0.1.0 or 0.0.0-ci.42)`,
    );
  }
  return raw;
}

/** The contents of dist/version.json. */
export interface VersionFile {
  version: string;
  protocol: number;
  /** The app-shell hash (build/shell.ts), the same value as the service worker's __SHELL_VERSION__. */
  shell: string;
}

export function versionFileContents(file: VersionFile): string {
  return JSON.stringify(file) + '\n';
}

/** Writes dist/version.json once the bundle and public/ are on disk. */
export function versionPlugin(opts: { version: string }): Plugin {
  let outDir = '';
  let urls: string[] | undefined;
  return {
    name: 'isshoni:version',
    apply: 'build',
    configResolved(config) {
      outDir = path.resolve(config.root, config.build.outDir);
    },
    writeBundle(_options, bundle) {
      urls = shellUrls(bundle);
    },
    closeBundle: {
      sequential: true,
      async handler() {
        if (!urls) return; // the build failed before writing
        const shell = await resolveShell(outDir, urls);
        const file: VersionFile = { version: opts.version, protocol: PROTOCOL_VERSION, shell: shell.version };
        await writeFile(path.join(outDir, 'version.json'), versionFileContents(file));
      },
    },
  };
}
