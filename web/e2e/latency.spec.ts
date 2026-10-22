// latency.spec (docs/m2/01-client-core.md §12.4 item 2, README WA43): the viewer path's latency proxy.
// isshoni-loadtest publishes one share of the fake Decodable video with its Opus tone, as decodable.spec has it
// publish (`-scenario smoke -decodable -publishers 1 -subscribers 0`, here for 45 s), and Chrome watches it on a
// friend's stage. For 20 s the stage's <video> reports every frame it presents through requestVideoFrameCallback, and
// the frames' metadata says how long each one took:
// - captureTime is Chrome's estimate of the frame's capture instant, from the RTP timestamp and the sender reports
//   (RFC 3550 §6.4.1), which the SFU maps from the publisher's. The tool and Chrome share this machine's clock, so
//   with it the spec asserts the whole capture → display leg: median expectedDisplayTime − captureTime < 150 ms and
//   p95 < 250 ms;
// - without it (Chrome gives no captureTime until it can estimate the sender's clock, which a receive-only
//   connection may never let it do) the spec asserts the receive → display leg, median
//   expectedDisplayTime − receiveTime < 100 ms, and annotates the test that the capture leg was not measured.
// Either way it catches timestamp or sender-report mistakes that make Chrome's jitter buffer grow. The values are
// logged and annotated, pass or fail, and the frames' metadata is attached to the report.
//
// The tool: LOADTEST_BIN names a built binary (a relative path counts from the repository root); without it this
// spec builds ./cmd/isshoni-loadtest once per worker, as decodable.spec does (that spec's fixture is its own, so
// this one has a copy).
import { execFile, spawn } from 'node:child_process';
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { promisify } from 'node:util';

import type { Page } from '@playwright/test';

import { expect, test as base } from './fixtures.ts';
import { REPO_ROOT } from './global-setup.ts';
import { videoOf, waitForRoom, waitForState, waitForStats } from './stats.ts';

const execFileAsync = promisify(execFile);

/** The environment variable that names a built isshoni-loadtest. */
const LOADTEST_ENV = 'LOADTEST_BIN';
/** The tool reads the admin's password from here (02 §15.2, 04 §4.2). */
const PASSWORD_ENV = 'ISSHONI_LOADTEST_PASSWORD';
/** A cold build downloads the toolchain and the modules first. */
const BUILD_TIMEOUT_MS = 10 * 60_000;
/** The tool signs up its account (one request per -login-interval, 3 s), connects and publishes. */
const LIVE_TIMEOUT_MS = 40_000;
const MEDIA_TIMEOUT_MS = 20_000;
/**
 * How long the tool publishes once its share is up: smoke's 30 s would leave the 20 s recording only what the
 * friend's stage and decoder take to start, so the spec asks for more. The tool's run then ends by itself.
 */
const TOOL_DURATION = '45s';
const RUN_TIMEOUT_MS = 120_000;

/** The recording (§12.4 item 2) and its bounds. */
const RECORD_MS = 20_000;
const CAPTURE_MEDIAN_MS = 150;
const CAPTURE_P95_MS = 250;
const RECEIVE_MEDIAN_MS = 100;
/**
 * The fewest frames a leg is judged on: half of what 20 s of the full layer (640×360 at 30 fps) presents. A stage
 * that showed fewer stood still, which is a failure of its own.
 */
const MIN_FRAMES = 300;

/** The finished run of the tool: its exit code and what it wrote. */
interface ToolRun {
  code: number | null;
  output: string;
}

/** One presented frame: requestVideoFrameCallback's metadata (ms on the page's performance clock). */
interface FrameSample {
  expectedDisplayTime: number;
  /** Absent until Chrome can map the RTP timestamps to the sender's clock. */
  captureTime: number | null;
  /** The arrival of the frame's last packet. */
  receiveTime: number | null;
  presentedFrames: number;
  width: number;
  height: number;
}

/** A leg's distribution in ms; the percentiles are nearest-rank. */
interface Summary {
  frames: number;
  min: number;
  median: number;
  p95: number;
  max: number;
}

/**
 * The tool's binary: LOADTEST_BIN, or a fresh build. A worker fixture, so that a cold build happens once
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

/**
 * Records the frames that the stage's <video> of `shareId` presents for `ms` ms: one requestVideoFrameCallback
 * after the other, each with its metadata. A stage that presents nothing returns no frames when the time is up.
 */
function recordFrames(page: Page, shareId: string, ms: number): Promise<FrameSample[]> {
  return page.evaluate(
    async ({ id, duration }) => {
      const video = document.querySelector<HTMLVideoElement>(
        `[data-viewer-stage][data-share-id="${CSS.escape(id)}"] video`,
      );
      if (!video) throw new Error(`the stage shows no video of ${id}`);
      const frames: FrameSample[] = [];
      let recording = true;
      const step = (_now: DOMHighResTimeStamp, md: VideoFrameCallbackMetadata): void => {
        if (!recording) return;
        frames.push({
          expectedDisplayTime: md.expectedDisplayTime,
          captureTime: md.captureTime ?? null,
          receiveTime: md.receiveTime ?? null,
          presentedFrames: md.presentedFrames,
          width: md.width,
          height: md.height,
        });
        video.requestVideoFrameCallback(step);
      };
      video.requestVideoFrameCallback(step);
      await new Promise((resolve) => setTimeout(resolve, duration));
      recording = false;
      return frames;
    },
    { id: shareId, duration: ms },
  );
}

/** The distribution of `values` (ms), or null when there are none. */
function summarize(values: number[]): Summary | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const rank = (p: number): number => sorted[Math.max(Math.ceil((sorted.length * p) / 100), 1) - 1] ?? NaN;
  return {
    frames: sorted.length,
    min: sorted[0] ?? NaN,
    median: rank(50),
    p95: rank(95),
    max: sorted[sorted.length - 1] ?? NaN,
  };
}

/** A summary for the log: "600 frames, median 41.2 ms, p95 52.3 ms (min 30.1, max 70.4)". */
function describeLeg(s: Summary | null): string {
  if (!s) return 'no frames';
  const ms = (v: number): string => v.toFixed(1);
  return (
    `${String(s.frames)} frames, median ${ms(s.median)} ms, p95 ${ms(s.p95)} ms ` +
    `(min ${ms(s.min)}, max ${ms(s.max)})`
  );
}

test('Chrome shows the Go publisher’s frames soon after they were captured', async ({
  startServer,
  asUser,
  loadtest,
}, testInfo) => {
  // The tool's sign-up, its 45 s run and its teardown, around the browser's checks.
  test.setTimeout(180_000);
  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');

  // The friend is in the lounge before anybody shares.
  const friend = await asUser(server);
  await friend.page.goto(`${server.url}/`);
  await waitForRoom(friend.page, 'lounge');

  const logPath = testInfo.outputPath('loadtest.log');
  const run = runLoadtest(
    loadtest,
    [
      ...['-server', server.url, '-admin', admin.username],
      ...['-scenario', 'smoke', '-decodable', '-publishers', '1', '-subscribers', '0', '-duration', TOOL_DURATION],
    ],
    admin.password,
    logPath,
  );
  // A failed browser check is the test's error; the tool's exit code then joins it as a soft error rather than
  // replacing it, since the tool often fails for the same reason.
  let failure: unknown;
  try {
    // The tool's share goes live, the friend's stage takes it, and Chrome decodes its full layer.
    const share = await waitForState(
      friend.page,
      (s) => s.viewer.shares.find((sh) => !sh.own && sh.status === 'live' && sh.layers.includes('high')),
      { timeout: LIVE_TIMEOUT_MS, message: "the tool's share is live with its full layer on the friend's page" },
    );
    await waitForState(friend.page, (s) => s.viewer.focusedShareId === share.id && s, {
      message: "the tool's share is on the friend's stage",
    });
    await waitForStats(
      friend.page,
      (s) => {
        const v = videoOf(s, share.id);
        return (v?.framesDecoded ?? 0) > 0 && v?.frameWidth === 640 && v.frameHeight === 360 && s;
      },
      { timeout: MEDIA_TIMEOUT_MS, message: 'the stage decodes the full layer: 640×360' },
    );

    // 20 s of presented frames.
    const frames = await recordFrames(friend.page, share.id, RECORD_MS);
    const captured = frames.filter((f) => f.captureTime !== null);
    const capture = summarize(captured.map((f) => f.expectedDisplayTime - (f.captureTime ?? NaN)));
    const receive = summarize(
      frames.filter((f) => f.receiveTime !== null).map((f) => f.expectedDisplayTime - (f.receiveTime ?? NaN)),
    );
    const withCapture = captured.length >= MIN_FRAMES;
    await testInfo.attach('latency.json', {
      body: JSON.stringify({ recordMs: RECORD_MS, capture, receive, frames }, null, 2),
      contentType: 'application/json',
    });
    const captureLine = `expectedDisplayTime − captureTime: ${describeLeg(capture)}`;
    const receiveLine = `expectedDisplayTime − receiveTime: ${describeLeg(receive)}`;
    console.log(
      `latency.spec: ${String(frames.length)} frames in ${String(RECORD_MS)} ms; ${captureLine}; ${receiveLine}`,
    );
    testInfo.annotations.push({ type: 'measured', description: captureLine });
    testInfo.annotations.push({ type: 'measured', description: receiveLine });

    if (withCapture && capture) {
      // The capture → display leg. A frame shows after it was captured: a leg below zero means captureTime is off
      // (a wrong RTP timestamp or sender report), which would pass the bounds without measuring anything.
      expect(capture.median, captureLine).toBeLessThan(CAPTURE_MEDIAN_MS);
      expect(capture.p95, captureLine).toBeLessThan(CAPTURE_P95_MS);
      expect(capture.median, `${captureLine}: a frame shown before it was captured`).toBeGreaterThan(0);
    } else {
      testInfo.annotations.push({
        type: 'not measured',
        description:
          `the capture leg: Chrome gave captureTime on ${String(captured.length)} of ${String(frames.length)} ` +
          `frames (fewer than ${String(MIN_FRAMES)}), so the receive leg is asserted instead`,
      });
      expect(
        receive?.frames ?? 0,
        `presented frames with a receiveTime in ${String(RECORD_MS)} ms`,
      ).toBeGreaterThanOrEqual(MIN_FRAMES);
      expect(receive?.median ?? NaN, receiveLine).toBeLessThan(RECEIVE_MEDIAN_MS);
    }
  } catch (err) {
    failure = err;
    throw err;
  } finally {
    // The tool's run ends by itself; its log belongs to the report either way.
    const finished = await run;
    await testInfo.attach('loadtest.log', { path: logPath, contentType: 'text/plain' });
    const exited = `isshoni-loadtest exited with ${String(finished.code)}:\n${finished.output}`;
    if (failure === undefined) expect(finished.code, exited).toBe(0);
    else expect.soft(finished.code, exited).toBe(0);
  }
});
