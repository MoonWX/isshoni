// Bundle size report (05 §17.3): gzip sizes of the initial JS (entry chunks and their static imports), of each lazy
// chunk, and of the CSS, written to web/build-report.json (gitignored). scripts/check-size.mjs checks the budgets.
import { writeFile } from 'node:fs/promises';
import path from 'node:path';
import zlib from 'node:zlib';

import type { Plugin, Rolldown } from 'vite';

export const REPORT_FILE = 'build-report.json';

export interface SizeEntry {
  /** Path inside dist/, e.g. assets/index-1a2b3c4d.js. */
  file: string;
  bytes: number;
  /** gzip level 9, as served from the .gz sibling. */
  gzip: number;
}

/** The format of build-report.json; scripts/check-size.mjs reads it. */
export interface BuildReport {
  format: 1;
  /** Entry chunks and everything they import statically: what the first page load downloads. */
  initial: { files: SizeEntry[]; gzip: number };
  /** Every other JS chunk, loaded on demand (lazy routes); each is checked on its own. */
  lazy: SizeEntry[];
  /** All CSS files. */
  css: { files: SizeEntry[]; gzip: number };
}

function sizeOf(file: string, content: string | Uint8Array): SizeEntry {
  const buf = typeof content === 'string' ? Buffer.from(content) : content;
  return { file, bytes: buf.length, gzip: zlib.gzipSync(buf, { level: 9 }).length };
}

const total = (files: readonly SizeEntry[]): number => files.reduce((sum, f) => sum + f.gzip, 0);
const byName = (a: SizeEntry, b: SizeEntry): number => a.file.localeCompare(b.file);

export function buildReport(bundle: Rolldown.OutputBundle): BuildReport {
  const initialNames = new Set<string>();
  const visit = (fileName: string): void => {
    if (initialNames.has(fileName)) return;
    const out = bundle[fileName];
    if (out?.type !== 'chunk') return;
    initialNames.add(fileName);
    for (const dep of out.imports) visit(dep);
  };
  for (const out of Object.values(bundle)) {
    if (out.type === 'chunk' && out.isEntry) visit(out.fileName);
  }

  const initial: SizeEntry[] = [];
  const lazy: SizeEntry[] = [];
  const css: SizeEntry[] = [];
  for (const out of Object.values(bundle)) {
    if (out.type === 'chunk') {
      (initialNames.has(out.fileName) ? initial : lazy).push(sizeOf(out.fileName, out.code));
    } else if (out.fileName.endsWith('.css')) {
      css.push(sizeOf(out.fileName, out.source));
    }
  }
  initial.sort(byName);
  lazy.sort(byName);
  css.sort(byName);
  return {
    format: 1,
    initial: { files: initial, gzip: total(initial) },
    lazy,
    css: { files: css, gzip: total(css) },
  };
}

export function reportPlugin(): Plugin {
  let reportPath = '';
  return {
    name: 'isshoni:report',
    apply: 'build',
    configResolved(config) {
      reportPath = path.resolve(config.root, REPORT_FILE);
    },
    async writeBundle(_options, bundle) {
      await writeFile(reportPath, JSON.stringify(buildReport(bundle), null, 2) + '\n');
    },
  };
}
