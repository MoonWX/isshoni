// Precompressed siblings (05 §17.1): <file>.br (Brotli quality 11) and <file>.gz (gzip level 9) next to every
// text file of 1 KiB or more in dist/, so 04's SPA handler can serve them to phones on mobile data. node:zlib only.
// Runs last (order: post, sequential), after every other plugin has written its files (e.g. S38's sw.js).
import { readdir, readFile, stat, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { promisify } from 'node:util';
import zlib from 'node:zlib';

import type { Plugin } from 'vite';

export const COMPRESS_EXTENSIONS: ReadonlySet<string> = new Set([
  '.js',
  '.css',
  '.html',
  '.svg',
  '.json',
  '.webmanifest',
]);
export const COMPRESS_MIN_BYTES = 1024;

const brotli = promisify(zlib.brotliCompress);
const gzip = promisify(zlib.gzip);

export function shouldCompress(fileName: string, size: number): boolean {
  return size >= COMPRESS_MIN_BYTES && COMPRESS_EXTENSIONS.has(path.extname(fileName).toLowerCase());
}

export interface Compressed {
  /** Path relative to the compressed directory, with forward slashes. */
  file: string;
  bytes: number;
  /** Size of the .br sibling, or null when Brotli didn't make the file smaller (no sibling written). */
  br: number | null;
  /** Size of the .gz sibling, or null when gzip didn't make the file smaller (no sibling written). */
  gz: number | null;
}

/** Writes .br and .gz siblings for every file under dir that shouldCompress accepts. */
export async function compressDir(dir: string): Promise<Compressed[]> {
  const entries = await readdir(dir, { recursive: true, withFileTypes: true });
  const files = entries
    .filter((e) => e.isFile())
    .map((e) => path.join(e.parentPath, e.name))
    .sort();
  const results: Compressed[] = [];
  for (const file of files) {
    const { size } = await stat(file);
    if (!shouldCompress(file, size)) continue;
    const content = await readFile(file);
    const [br, gz] = await Promise.all([
      brotli(content, {
        params: {
          [zlib.constants.BROTLI_PARAM_QUALITY]: 11,
          [zlib.constants.BROTLI_PARAM_MODE]: zlib.constants.BROTLI_MODE_TEXT,
          [zlib.constants.BROTLI_PARAM_SIZE_HINT]: size,
        },
      }),
      gzip(content, { level: 9 }),
    ]);
    const smaller = async (ext: string, data: Buffer): Promise<number | null> => {
      if (data.length >= size) return null;
      await writeFile(file + ext, data);
      return data.length;
    };
    results.push({
      file: path.relative(dir, file).split(path.sep).join('/'),
      bytes: size,
      br: await smaller('.br', br),
      gz: await smaller('.gz', gz),
    });
  }
  return results;
}

export function compressPlugin(): Plugin {
  let outDir = '';
  let wrote = false;
  return {
    name: 'isshoni:compress',
    apply: 'build',
    configResolved(config) {
      outDir = path.resolve(config.root, config.build.outDir);
    },
    writeBundle() {
      wrote = true;
    },
    closeBundle: {
      order: 'post',
      sequential: true,
      async handler() {
        if (!wrote) return; // the build failed before writing
        await compressDir(outDir);
      },
    },
  };
}
