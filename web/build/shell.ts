// The app shell (05 §16.2): the files a new service worker precaches, and the hash that versions them. The version
// plugin writes the hash into dist/version.json as `shell`; the service-worker plugin (S38) injects the same list and
// hash as __SHELL__ and __SHELL_VERSION__. Both compute them here, from the written dist/, so they always agree.
import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import path from 'node:path';

import type { Rolldown } from 'vite';

/** Shell files copied from public/ rather than built. A file that doesn't exist (yet) is left out. */
export const SHELL_PUBLIC_FILES: readonly string[] = ['/boot-check.js', '/manifest.webmanifest', '/icons/icon-192.png'];

/**
 * The shell's URL paths from a finished bundle: `/` (index.html), each entry chunk with its static imports
 * (transitively) and their CSS, then SHELL_PUBLIC_FILES. Lazy (dynamically imported) chunks are not in the shell;
 * the service worker caches them on first use. `/` comes first, the rest sorted.
 */
export function shellUrls(bundle: Rolldown.OutputBundle): string[] {
  const urls = new Set<string>();
  const seen = new Set<string>();
  const visit = (fileName: string): void => {
    if (seen.has(fileName)) return;
    seen.add(fileName);
    const out = bundle[fileName];
    if (out?.type !== 'chunk') return;
    urls.add('/' + out.fileName);
    for (const css of out.viteMetadata?.importedCss ?? []) urls.add('/' + css);
    for (const dep of out.imports) visit(dep);
  };
  for (const out of Object.values(bundle)) {
    if (out.type === 'chunk' && out.isEntry) visit(out.fileName);
  }
  for (const file of SHELL_PUBLIC_FILES) urls.add(file);
  return ['/', ...[...urls].sort()];
}

/** The file under outDir that serves a shell URL (`/` is index.html). */
export function shellUrlToPath(outDir: string, url: string): string {
  return path.join(outDir, url === '/' ? 'index.html' : url.replace(/^\/+/, ''));
}

export interface Shell {
  /** The shell URLs whose files exist in outDir, in shellUrls order. */
  urls: string[];
  /** The first 12 hex digits of SHA-256 over each existing file's URL and contents, in URL order. */
  version: string;
}

/** Reads the shell files from outDir and hashes them. `/` (index.html) must exist; others are skipped if missing. */
export async function resolveShell(outDir: string, urls: readonly string[]): Promise<Shell> {
  const hash = createHash('sha256');
  const present: string[] = [];
  for (const url of [...urls].sort()) {
    let content: Buffer;
    try {
      content = await readFile(shellUrlToPath(outDir, url));
    } catch (err) {
      if (url !== '/' && isNotFound(err)) continue;
      throw err;
    }
    present.push(url);
    hash.update(url).update('\0').update(content).update('\0');
  }
  const order = new Map(urls.map((u, i) => [u, i]));
  present.sort((a, b) => (order.get(a) ?? 0) - (order.get(b) ?? 0));
  return { urls: present, version: hash.digest('hex').slice(0, 12) };
}

function isNotFound(err: unknown): boolean {
  return err instanceof Error && 'code' in err && err.code === 'ENOENT';
}
