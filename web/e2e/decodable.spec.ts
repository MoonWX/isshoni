// decodable.spec (02 §17, README S89): the Go decodable publisher → Chrome. isshoni-loadtest publishes one share of
// the fake Decodable video (Constrained Baseline H.264 of I_PCM and P_Skip macroblocks, 02 §15.1) with its Opus tone
// through a server of the test's own, as `-scenario smoke -decodable -publishers 1 -subscribers 0`: the tool has no
// viewers of its own (02 §15.2), so the browser is the viewer. A friend in Chrome sees the share go live, puts it on
// the stage, and the browser's decoder turns it into pictures: framesDecoded > 0, at the full layer's 640×360. The
// tool then ends its run with exit 0 and a report that passes, and deletes the account it made.
//
// The tool: ISSHONI_LOADTEST_BIN names a built binary (a relative path counts from the repository root). Without it
// this spec builds ./cmd/isshoni-loadtest with the Go toolchain on the PATH (go.mod's toolchain line picks the
// version), once per worker, into a temporary directory; `task e2e` builds only bin/isshoni.
import { execFile, spawn } from 'node:child_process';
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { promisify } from 'node:util';

import { expect, test as base } from './fixtures.ts';
import { REPO_ROOT } from './global-setup.ts';
import { videoOf, waitForRoom, waitForState, waitForStats } from './stats.ts';

const execFileAsync = promisify(execFile);

/** The environment variable that names a built isshoni-loadtest. */
const LOADTEST_ENV = 'ISSHONI_LOADTEST_BIN';
/** The tool reads the admin's password from here (02 §15.2, 04 §4.2). */
const PASSWORD_ENV = 'ISSHONI_LOADTEST_PASSWORD';
/** A cold build downloads the toolchain and the modules first. */
const BUILD_TIMEOUT_MS = 10 * 60_000;
/** The tool signs up its account (one request per -login-interval, 3 s), connects and publishes. */
const LIVE_TIMEOUT_MS = 40_000;
const MEDIA_TIMEOUT_MS = 20_000;

/** The tool's run: `-scenario smoke` publishes for 30 s once the share is live, then tears down. */
const RUN_TIMEOUT_MS = 90_000;

/** The JSON report of isshoni-loadtest (cmd/isshoni-loadtest/report.go), as far as this spec reads it. */
interface LoadtestReport {
  scenario: string;
  pass: boolean;
  failures: string[];
  params: { publishers: number; subscribers: number; decodable: boolean; profile: string };
  users: { created: number; deleted: number; kept: boolean };
}

/** The finished run of the tool: its exit code and what it wrote. */
interface ToolRun {
  code: number | null;
  output: string;
}

/**
 * The tool's binary: ISSHONI_LOADTEST_BIN, or a fresh build. A worker fixture, so that a cold build happens once
 * per worker and under a timeout of its own.
 */
const test = base.extend<object, { loadtest: string }>({
  loadtest: [
    // eslint-disable-next-line no-empty-pattern -- Playwright reads the fixture names from this pattern
    async ({}, use) => {
      const given = process.env[LOADTEST_ENV];
      if (given !== undefined && given !== '') {
        await use(path.resolve(REPO_ROOT, given));
        return;
      }
      const dir = await fsp.mkdtemp(path.join(os.tmpdir(), 'isshoni-loadtest-'));
      const bin = path.join(dir, process.platform === 'win32' ? 'isshoni-loadtest.exe' : 'isshoni-loadtest');
      try {
        await execFileAsync('go', ['build', '-o', bin, './cmd/isshoni-loadtest'], {
          cwd: REPO_ROOT,
          env: { ...process.env, CGO_ENABLED: '0' },
          timeout: BUILD_TIMEOUT_MS,
        });
      } catch (err) {
        await fsp.rm(dir, { recursive: true, force: true });
        const stderr = (err as { stderr?: unknown }).stderr;
        throw new Error(
          `building ./cmd/isshoni-loadtest failed; set ${LOADTEST_ENV} to a built binary, or put Go on the PATH: ` +
            (typeof stderr === 'string' && stderr !== '' ? stderr.trim() : String(err)),
          { cause: err },
        );
      }
      await use(bin);
      await fsp.rm(dir, { recursive: true, force: true });
    },
    { scope: 'worker', timeout: BUILD_TIMEOUT_MS + 30_000 },
  ],
});

/**
 * Starts the tool and returns its finished run. Its environment is this process's without any ISSHONI_* variable,
 * plus the admin's password; what it prints (the summary, its log) goes to `logPath` too.
 */
function runLoadtest(bin: string, args: string[], password: string, logPath: string): Promise<ToolRun> {
  const env: NodeJS.ProcessEnv = {};
  for (const [name, value] of Object.entries(process.env)) {
    if (!name.startsWith('ISSHONI_')) env[name] = value;
  }
  env[PASSWORD_ENV] = password;
  const log = fs.createWriteStream(logPath);
  const child = spawn(bin, args, { cwd: REPO_ROOT, env, stdio: ['ignore', 'pipe', 'pipe'] });
  let output = '';
  const collect = (chunk: Buffer): void => {
    output += chunk.toString('utf8');
    log.write(chunk);
  };
  child.stdout.on('data', collect);
  child.stderr.on('data', collect);
  const timer = setTimeout(() => child.kill('SIGINT'), RUN_TIMEOUT_MS);
  return new Promise((resolve) => {
    const done = (code: number | null): void => {
      clearTimeout(timer);
      log.end(() => {
        resolve({ code, output });
      });
    };
    child.once('error', (err) => {
      output += `\ncould not start ${bin}: ${err.message}\n`;
      done(null);
    });
    child.once('close', done);
  });
}

test('Chrome decodes the Go publisher’s decodable video', async ({ startServer, asUser, loadtest }, testInfo) => {
  // The tool's sign-up, its 30 s run and its teardown, around the browser's checks.
  test.setTimeout(150_000);
  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');

  // The friend is in the lounge before anybody shares.
  const friend = await asUser(server);
  await friend.page.goto(`${server.url}/`);
  await waitForRoom(friend.page, 'lounge');

  const reportPath = testInfo.outputPath('loadtest-report.json');
  const logPath = testInfo.outputPath('loadtest.log');
  const run = runLoadtest(
    loadtest,
    [
      ...['-server', server.url, '-admin', admin.username],
      ...['-scenario', 'smoke', '-decodable', '-publishers', '1', '-subscribers', '0'],
      ...['-json', reportPath],
    ],
    admin.password,
    logPath,
  );
  try {
    // The tool's share goes live, and the friend's stage takes it.
    const share = await waitForState(
      friend.page,
      (s) => s.viewer.shares.find((sh) => !sh.own && sh.status === 'live' && sh.layers.includes('high')),
      { timeout: LIVE_TIMEOUT_MS, message: "the tool's share is live with its full layer on the friend's page" },
    );
    await waitForState(friend.page, (s) => s.viewer.focusedShareId === share.id && s, {
      message: "the tool's share is on the friend's stage",
    });

    // Chrome decodes it: frames, in the share's Constrained Baseline, at the full layer's size, and more of them.
    const first = await waitForStats(friend.page, (s) => (videoOf(s, share.id)?.framesDecoded ?? 0) > 0 && s, {
      timeout: MEDIA_TIMEOUT_MS,
      message: 'decoded frames of the decodable video',
    });
    expect(videoOf(first, share.id)?.codec).toBe('h264/42e0');
    const full = await waitForStats(
      friend.page,
      (s) => {
        const v = videoOf(s, share.id);
        return v?.frameWidth === 640 && v.frameHeight === 360 && s;
      },
      { timeout: MEDIA_TIMEOUT_MS, message: 'the stage decodes the full layer: 640×360' },
    );
    await waitForStats(
      friend.page,
      (s) => (videoOf(s, share.id)?.framesDecoded ?? 0) > (videoOf(full, share.id)?.framesDecoded ?? 0) + 30 && s,
      { timeout: MEDIA_TIMEOUT_MS, message: 'more decoded frames: the P_Skip frames decode too' },
    );
  } finally {
    // The tool's run ends by itself; its log belongs to the report either way.
    const finished = await run;
    await testInfo.attach('loadtest.log', { path: logPath, contentType: 'text/plain' });
    expect(finished.code, `isshoni-loadtest exited with ${String(finished.code)}:\n${finished.output}`).toBe(0);
  }

  // Its report passes, and the one account it made is gone.
  const report = JSON.parse(await fsp.readFile(reportPath, 'utf8')) as LoadtestReport;
  expect(report).toMatchObject({
    scenario: 'smoke',
    pass: true,
    failures: [],
    params: { publishers: 1, subscribers: 0, decodable: true, profile: '42e0' },
    users: { created: 1, deleted: 1, kept: false },
  });
});
