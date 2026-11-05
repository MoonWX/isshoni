// The CDP spec's rows (M2 03 §3.2, §20, §22.4 B): a Playwright reporter for e2e/cdp/playwright.config.ts that brings
// the spec's "Wails checklist" and "Budget (03 §20)" annotations out of the HTML-less CI run. Each one is printed to
// stdout as one line ("Wails checklist | W2: isSecureContext = true") as its test ends, so the step's log holds it even
// when a later test hangs; at the end of the run they go, as a Markdown table, to annotations.md in the run's
// outputDir (which windows-desktop uploads as cdp-test-results, passed or failed) and, when GITHUB_STEP_SUMMARY is
// set, are appended to that file, the job summary. Annotations of other types (skip, fixme, …) are left out.
//
// The formatting is plain functions, tested on fake results by annotations.node.test.ts (node --test).
import fs from 'node:fs';
import path from 'node:path';

import type { FullConfig, Reporter, TestCase, TestResult } from '@playwright/test/reporter';

/** The annotation types the CDP spec writes (desktop.cdp.ts's row() and budget()), in the table's order. */
export const ROW_TYPES = ['Wails checklist', 'Budget (03 §20)'] as const;

/** The file, in the run's outputDir, that holds the table. */
export const ANNOTATIONS_FILE = 'annotations.md';

/** One annotation of ROW_TYPES with the test that wrote it. */
export interface Row {
  readonly type: string;
  readonly description: string;
  /** The test's title (without the file). */
  readonly test: string;
  /** The test's status: passed, failed, timedOut, skipped or interrupted. */
  readonly status: string;
}

/** Folds whitespace runs (a description may span lines) into one space. */
function oneLine(s: string): string {
  return s.replace(/\s+/g, ' ').trim();
}

/** The annotation's stdout line: "<type> | <description>", on one line. */
export function annotationLine(row: Pick<Row, 'type' | 'description'>): string {
  return `${row.type} | ${oneLine(row.description)}`;
}

/**
 * The text of a Markdown table cell: one line, its pipes escaped, and no HTML (GitHub renders a summary's tags, and
 * a description may hold "<p>" or "<name>" placeholders).
 */
export function tableCell(s: string): string {
  return oneLine(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/\\/g, '\\\\')
    .replace(/\|/g, '\\|');
}

/** The rows of one test result: its annotations of ROW_TYPES, in the order the test wrote them. */
export function rowsOf(test: Pick<TestCase, 'title'>, result: Pick<TestResult, 'annotations' | 'status'>): Row[] {
  const types: readonly string[] = ROW_TYPES;
  return result.annotations
    .filter((a) => types.includes(a.type))
    .map((a) => ({ type: a.type, description: a.description ?? '', test: test.title, status: result.status }));
}

/**
 * The Markdown of the rows: a heading, then one table with the Wails checklist rows first and the budgets after, each
 * group in the order the run wrote it; a line saying so when there is no row.
 */
export function summaryMarkdown(rows: readonly Row[], heading = "The CDP spec's rows (03 §3.2, §20)"): string {
  const lines = [`### ${heading}`, ''];
  if (rows.length === 0) {
    lines.push('No "Wails checklist" or "Budget (03 §20)" annotation was recorded.', '');
    return lines.join('\n');
  }
  lines.push('| Annotation | Result | Test | Status |', '|---|---|---|---|');
  for (const type of ROW_TYPES) {
    for (const r of rows.filter((x) => x.type === type)) {
      lines.push(
        `| ${tableCell(r.type)} | ${tableCell(r.description)} | ${tableCell(r.test)} | ${tableCell(r.status)} |`,
      );
    }
  }
  lines.push('');
  return lines.join('\n');
}

/** The reporter's options (Playwright passes the config's second tuple element); tests set all three. */
export interface AnnotationsReporterOptions {
  /** Where a line goes; stdout by default. */
  readonly write?: (line: string) => void;
  /** The job summary's file; GITHUB_STEP_SUMMARY by default (none when unset or empty). */
  readonly summaryPath?: string;
  /** The folder of annotations.md; the first project's outputDir by default. */
  readonly outputDir?: string;
}

/** The reporter: lines as tests end, the table at the end of the run. */
export default class AnnotationsReporter implements Reporter {
  private readonly rows: Row[] = [];
  private readonly write: (line: string) => void;
  private readonly summaryPath: string | undefined;
  private outputDir: string | undefined;

  constructor(options: AnnotationsReporterOptions = {}) {
    this.write = options.write ?? ((line) => process.stdout.write(`${line}\n`));
    this.summaryPath = options.summaryPath ?? process.env['GITHUB_STEP_SUMMARY'];
    this.outputDir = options.outputDir;
  }

  /** The list reporter keeps the console; these lines come in between. */
  printsToStdio(): boolean {
    return false;
  }

  onBegin(config: FullConfig): void {
    this.outputDir ??= config.projects[0]?.outputDir;
  }

  onTestEnd(test: TestCase, result: TestResult): void {
    for (const row of rowsOf(test, result)) {
      this.rows.push(row);
      this.write(annotationLine(row));
    }
  }

  onEnd(): void {
    const md = summaryMarkdown(this.rows);
    const dir = this.outputDir;
    if (dir !== undefined && dir !== '') {
      this.tryWrite(() => {
        fs.mkdirSync(dir, { recursive: true });
        fs.writeFileSync(path.join(dir, ANNOTATIONS_FILE), md);
      }, ANNOTATIONS_FILE);
    }
    const summary = this.summaryPath;
    if (summary !== undefined && summary !== '') {
      this.tryWrite(() => {
        fs.appendFileSync(summary, `${md}\n`);
      }, 'the job summary');
    }
  }

  /** A file that can't be written is reported, never a failed run: the lines are in the log already. */
  private tryWrite(fn: () => void, what: string): void {
    try {
      fn();
    } catch (err) {
      process.stderr.write(`annotations reporter: ${what} not written: ${String(err)}\n`);
    }
  }
}
