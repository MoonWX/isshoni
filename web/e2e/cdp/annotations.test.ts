// The CDP spec's reporter (annotations.ts) on fake results: one stdout line per annotation, the Markdown table in
// annotations.md and the job summary. Runs on any OS: node --test e2e/cdp/annotations.test.ts (from web/), in CI's
// web job and in task test:web. Not a Vitest file: Vitest covers src/ and build/ only, and *.node.test.ts names
// Vitest files of the node environment here.
// node:test's describe and it return promises that its runner awaits itself, hence the voids.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, it } from 'node:test';

import type { FullConfig, TestCase, TestResult } from '@playwright/test/reporter';

import AnnotationsReporter, {
  ANNOTATIONS_FILE,
  annotationLine,
  rowsOf,
  summaryMarkdown,
  tableCell,
} from './annotations.ts';

/** A fake test and result: only what the reporter reads. */
function fake(
  title: string,
  status: TestResult['status'],
  annotations: TestResult['annotations'],
): [TestCase, TestResult] {
  return [{ title } as TestCase, { status, annotations } as TestResult];
}

const W2 = fake('W1, W2: the page is at the asset origin, in a secure context', 'passed', [
  { type: 'Wails checklist', description: 'W1: location.origin = http://wails.localhost' },
  { type: 'skip', description: 'not a row' },
  { type: 'Wails checklist', description: 'W2: isSecureContext = true' },
]);
const BUDGET = fake('the app end to end', 'failed', [
  { type: 'Budget (03 §20)', description: 'startup to the page: 2140 ms;\n  to Playwright attached: 2600 ms' },
  { type: 'Wails checklist', description: 'W14: Quit | exit 0 in 410 ms (≤ 3000 ms) <p>' },
]);

void describe('the formatting', () => {
  void it('prints an annotation as one line', () => {
    assert.equal(
      annotationLine({ type: 'Wails checklist', description: 'W2: isSecureContext = true' }),
      'Wails checklist | W2: isSecureContext = true',
    );
    assert.equal(annotationLine({ type: 'Budget (03 §20)', description: 'a\n  b\r\nc ' }), 'Budget (03 §20) | a b c');
  });

  void it('escapes table cells', () => {
    assert.equal(tableCell('a | b'), 'a \\| b');
    assert.equal(tableCell('\\\\.\\pipe\\x'), '\\\\\\\\.\\\\pipe\\\\x');
    assert.equal(tableCell('<p> & <name>'), '&lt;p&gt; &amp; &lt;name&gt;');
    assert.equal(tableCell('two\nlines'), 'two lines');
  });

  void it('keeps only the two row types, in order', () => {
    const rows = rowsOf(...W2);
    assert.deepEqual(
      rows.map((r) => r.description),
      ['W1: location.origin = http://wails.localhost', 'W2: isSecureContext = true'],
    );
    const [first] = rows;
    assert.ok(first);
    assert.equal(first.status, 'passed');
    assert.equal(first.test, W2[0].title);
    assert.deepEqual(rowsOf(...fake('none', 'passed', [])), []);
  });

  void it('writes the checklist rows first, then the budgets', () => {
    const md = summaryMarkdown([...rowsOf(...BUDGET), ...rowsOf(...W2)]);
    assert.equal(
      md,
      [
        "### The CDP spec's rows (03 §3.2, §20)",
        '',
        '| Annotation | Result | Test | Status |',
        '|---|---|---|---|',
        '| Wails checklist | W14: Quit \\| exit 0 in 410 ms (≤ 3000 ms) &lt;p&gt; | the app end to end | failed |',
        '| Wails checklist | W1: location.origin = http://wails.localhost | W1, W2: the page is at the asset origin, in a secure context | passed |',
        '| Wails checklist | W2: isSecureContext = true | W1, W2: the page is at the asset origin, in a secure context | passed |',
        '| Budget (03 §20) | startup to the page: 2140 ms; to Playwright attached: 2600 ms | the app end to end | failed |',
        '',
      ].join('\n'),
    );
  });

  void it('says so when no row was recorded', () => {
    assert.match(
      summaryMarkdown([]),
      /^### .*\n\nNo "Wails checklist" or "Budget \(03 §20\)" annotation was recorded\.\n$/,
    );
  });
});

void describe('the reporter', () => {
  let dir = '';
  beforeEach(() => {
    dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-annotations-'));
  });
  afterEach(() => {
    fs.rmSync(dir, { recursive: true, force: true });
  });

  /** Runs a reporter over the fake results; returns its stdout lines. */
  function run(options: { summaryPath?: string; outputDir?: string }): string[] {
    const lines: string[] = [];
    const r = new AnnotationsReporter({ write: (l) => lines.push(l), ...options });
    r.onBegin({ projects: [{ outputDir: path.join(dir, 'from-config') }] } as unknown as FullConfig);
    r.onTestEnd(...W2);
    r.onTestEnd(...BUDGET);
    r.onEnd();
    return lines;
  }

  void it('prints every row as its test ends and appends the table to the job summary', () => {
    const summary = path.join(dir, 'step_summary.md');
    fs.writeFileSync(summary, '## An earlier step\n\n');
    const lines = run({ summaryPath: summary, outputDir: path.join(dir, 'out') });
    assert.deepEqual(lines, [
      'Wails checklist | W1: location.origin = http://wails.localhost',
      'Wails checklist | W2: isSecureContext = true',
      'Budget (03 §20) | startup to the page: 2140 ms; to Playwright attached: 2600 ms',
      'Wails checklist | W14: Quit | exit 0 in 410 ms (≤ 3000 ms) <p>',
    ]);
    const table = summaryMarkdown([...rowsOf(...W2), ...rowsOf(...BUDGET)]);
    assert.equal(fs.readFileSync(summary, 'utf8'), `## An earlier step\n\n${table}\n`);
    assert.equal(fs.readFileSync(path.join(dir, 'out', ANNOTATIONS_FILE), 'utf8'), table);
  });

  void it("writes no summary when GITHUB_STEP_SUMMARY is unset, and annotations.md in the project's outputDir", () => {
    const saved = process.env['GITHUB_STEP_SUMMARY'];
    delete process.env['GITHUB_STEP_SUMMARY'];
    try {
      const lines = run({});
      assert.equal(lines.length, 4);
    } finally {
      if (saved !== undefined) process.env['GITHUB_STEP_SUMMARY'] = saved;
    }
    assert.deepEqual(fs.readdirSync(dir), ['from-config']);
    assert.ok(fs.existsSync(path.join(dir, 'from-config', ANNOTATIONS_FILE)));
  });

  void it('does not fail the run when the summary cannot be written', () => {
    const lines = run({ summaryPath: path.join(dir, 'missing', 'step_summary.md'), outputDir: path.join(dir, 'out') });
    assert.equal(lines.length, 4);
    assert.ok(fs.existsSync(path.join(dir, 'out', ANNOTATIONS_FILE)));
  });
});
