// Deletes the previous build from dist/ before a build. vite.config.ts sets emptyOutDir: false so that Vite keeps
// the committed dist/.gitkeep (06 §7.2), which `//go:embed all:dist` needs on a fresh clone; this plugin does the
// emptying instead, so stale hashed assets never end up embedded in the Go binary.
import { readdir, rm } from 'node:fs/promises';
import path from 'node:path';

import type { Plugin } from 'vite';

/** Names at the top of dist/ that survive a clean. */
export const KEEP_IN_DIST: ReadonlySet<string> = new Set(['.gitkeep']);

/** Removes everything in outDir except the names in keep. A missing outDir is fine. */
export async function cleanOutDir(outDir: string, keep: ReadonlySet<string> = KEEP_IN_DIST): Promise<void> {
  let names: string[];
  try {
    names = await readdir(outDir);
  } catch (err) {
    if (err instanceof Error && 'code' in err && err.code === 'ENOENT') return;
    throw err;
  }
  await Promise.all(
    names.filter((n) => !keep.has(n)).map((n) => rm(path.join(outDir, n), { recursive: true, force: true })),
  );
}

export function cleanPlugin(): Plugin {
  let outDir = '';
  return {
    name: 'isshoni:clean',
    apply: 'build',
    configResolved(config) {
      const root = path.resolve(config.root);
      outDir = path.resolve(root, config.build.outDir);
      // Never empty the project itself or anything outside it.
      if (!outDir.startsWith(root + path.sep)) {
        throw new Error(`isshoni:clean: build.outDir ${outDir} must be inside ${root}`);
      }
    },
    async buildStart() {
      await cleanOutDir(outDir);
    },
  };
}
