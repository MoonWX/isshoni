#!/usr/bin/env node
// check:size (05 §17.3): the bundle-size budget, from build-report.json (build/report-plugin.ts writes it on every
// `npm run build`). gzip sizes; 1 KB = 1000 bytes.
//   initial JS (entry chunks and their static imports) <= 200 KB
//   each lazy chunk                                     <= 120 KB
//   CSS (all files)                                     <=  30 KB
//
// Usage: node scripts/check-size.mjs [--report <build-report.json>]
// Exit 0 within budget, 1 over budget or without a report, 2 on bad usage.
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';

const WEB_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

export const BUDGETS = Object.freeze({ initialJs: 200_000, lazyChunk: 120_000, css: 30_000 });

/**
 * @typedef {{ file: string, bytes: number, gzip: number }} SizeEntry
 * @typedef {{ format: number, initial: { files: SizeEntry[], gzip: number }, lazy: SizeEntry[],
 *             css: { files: SizeEntry[], gzip: number } }} BuildReport
 */

/** @param {number} bytes */
const kb = (bytes) => `${(bytes / 1000).toFixed(1)} KB`;

/**
 * Checks a report against the budgets.
 * @param {BuildReport} report
 * @param {typeof BUDGETS} [budgets]
 * @returns {{ lines: string[], errors: string[] }} a summary, and one line per budget exceeded
 */
export function checkSize(report, budgets = BUDGETS) {
  if (report.format !== 1) {
    return { lines: [], errors: [`unknown build-report format ${String(report.format)}: rebuild with npm run build`] };
  }
  /** @type {string[]} */
  const errors = [];
  const lines = [
    `initial JS ${kb(report.initial.gzip)} of ${kb(budgets.initialJs)} (${report.initial.files.length} files)`,
    `lazy chunks ${report.lazy.length}, largest ${kb(Math.max(0, ...report.lazy.map((c) => c.gzip)))} of ${kb(budgets.lazyChunk)} each`,
    `CSS ${kb(report.css.gzip)} of ${kb(budgets.css)}`,
  ];
  if (report.initial.gzip > budgets.initialJs) {
    const files = report.initial.files.map((f) => `${f.file} ${kb(f.gzip)}`).join(', ');
    errors.push(`initial JS is ${kb(report.initial.gzip)}, over the ${kb(budgets.initialJs)} budget: ${files}`);
  }
  for (const chunk of report.lazy) {
    if (chunk.gzip > budgets.lazyChunk) {
      errors.push(`lazy chunk ${chunk.file} is ${kb(chunk.gzip)}, over the ${kb(budgets.lazyChunk)} budget`);
    }
  }
  if (report.css.gzip > budgets.css) {
    errors.push(`CSS is ${kb(report.css.gzip)}, over the ${kb(budgets.css)} budget`);
  }
  return { lines, errors };
}

async function main() {
  /** @type {{ report?: string }} */
  let values;
  try {
    ({ values } = parseArgs({ options: { report: { type: 'string' } } }));
  } catch (err) {
    console.error(`check:size: ${err instanceof Error ? err.message : String(err)}`);
    console.error('usage: node scripts/check-size.mjs [--report <build-report.json>]');
    process.exit(2);
  }
  const reportPath = path.resolve(values.report ?? path.join(WEB_ROOT, 'build-report.json'));
  /** @type {BuildReport} */
  let report;
  try {
    report = JSON.parse(await readFile(reportPath, 'utf8'));
  } catch (err) {
    console.error(`check:size: can't read ${reportPath} (${err instanceof Error ? err.message : String(err)})`);
    console.error('check:size: run npm run build first');
    process.exit(1);
  }
  const { lines, errors } = checkSize(report);
  for (const line of lines) console.log(`check:size: ${line}`);
  for (const e of errors) console.error(`check:size: ${e}`);
  if (errors.length > 0) process.exit(1);
  console.log('check:size: ok');
}

// import.meta.main, not a comparison with process.argv[1]: that path isn't resolved through symlinks, junctions or
// subst drives, and a check that silently skips main() would pass without checking anything.
if (import.meta.main) {
  await main();
}
