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
// Either way it catches RTP timestamp mistakes that make Chrome's video jitter buffer grow, and with captureTime
// sender-report mistakes too. The values are logged and annotated, pass or fail, and the frames' metadata is attached
// to the report.
//
// The legs are the video path's: each frame's leg is taken without Chrome's lip-sync hold. A share's audio and video
// have one msid stream (M1 02 §9.6), so Chrome shows a frame no earlier than the audio captured with it (01 §12.1):
// when the audio path (NetEq, Chrome's audio jitter buffer, then the audio output) is the slower one, every frame
// waits for the difference, and that wait is the audio path's number, not the video path's. NetEq raises its target
// to cover the longest gap it has seen and lowers it over many seconds; the output is a PulseAudio null sink in CI
// (headful Chrome) and a muted fake one locally (headless, a fixed 33.8 ms); and Chrome's sync moves the hold by a
// fraction of the difference each second. On PR #41's CI the receive leg as shown came out at median 179.4 / 166.0
// ms, p95 279.8 / 266.0 ms, max 280.6 / 283.2 ms with 585 frames, the shape of that ramp. Locally (an M5 Mac) it is
// about 30 ms with a hold of 0–20 ms; with the audio's jitter buffer held at 250 ms (jitterBufferTarget) it is median
// 185.4 / p95 252.7 ms, and after one 250 ms pause of the tool (SIGSTOP, as a busy runner stalls it) median 140.3 ms,
// while the same frames less their hold are at median 26.5 and 51.9 ms: the video's own jitter buffer stayed at
// 20–30 ms.
// Chrome's stats give the hold: the video receiver's jitterBufferTargetDelay − jitterBufferMinimumDelay per emitted
// frame (webrtc-stats: the minimum delay leaves out A/V sync and jitterBufferTarget, which this app never sets). The
// spec samples the stage's receivers every 250 ms while it records, takes each frame's hold from the interval it was
// shown in, and logs and annotates the hold, the legs as shown and the audio path next to the asserted legs. The
// hold itself is not bounded: it follows the audio path, which this spec doesn't own. A sender-report mismatch between
// the share's audio and video would show there, as a hold that the audio path doesn't explain.
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
/** How often the recording samples the stage's receivers for the lip-sync hold (Chrome's sync moves it each second). */
const STATS_EVERY_MS = 250;

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

/** The stage's receivers at one moment of the recording: getStats' cumulative counters, in s as getStats has them. */
interface ReceiverSample {
  /** performance.now() when the stats came back: the clock of the frames' times. */
  at: number;
  /** The inbound-rtp of the stage's video track; null when getStats has no such stream or lacks these counters. */
  video: { emitted: number; targetDelay: number; minimumDelay: number } | null;
  /** The inbound-rtp of the audible audio track (NetEq) and its media-playout (the output); for the log only. */
  audio: { emitted: number; jitterBufferDelay: number } | null;
  playout: { samples: number; playoutDelay: number } | null;
}

/** What recordFrames returns. */
interface Recording {
  frames: FrameSample[];
  receivers: ReceiverSample[];
}

/** One interval between two receiver samples (ms on the page's performance clock), and what it held. */
interface HoldInterval {
  from: number;
  to: number;
  /** The lip-sync hold on each frame emitted in it, ms (never below 0). */
  hold: number;
  /** NetEq's delay per emitted sample and the output's delay per played sample, ms; null without them. */
  audioJitterBuffer: number | null;
  audioOutput: number | null;
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
 * An init script for the friend's context: every RTCPeerConnection the page makes is also kept in
 * window.__latencyPCs, so that the recording can read its receivers' stats as getStats has them (the page's debug
 * handle sums them up without the jitter buffer's target and minimum delays). The page's constructor becomes a
 * subclass of the native one, so the page gets the same objects, prototype chain and statics as before.
 */
function keepPeerConnections(): void {
  const Native = window.RTCPeerConnection;
  const pcs: RTCPeerConnection[] = [];
  Object.defineProperty(window, '__latencyPCs', { value: pcs });
  window.RTCPeerConnection = class extends Native {
    constructor(configuration?: RTCConfiguration) {
      super(configuration);
      pcs.push(this);
    }
  };
}

/**
 * Records the frames that the stage's <video> of `shareId` presents for `ms` ms: one requestVideoFrameCallback
 * after the other, each with its metadata. A stage that presents nothing returns no frames when the time is up.
 * Meanwhile, every `statsEveryMs` and once more at the end, it samples the receivers of the page's PeerConnections
 * (keepPeerConnections): the inbound-rtp of the stage's video track, and that of the audio track the page plays (its
 * one <audio> element, 05 §10.2) with its media-playout.
 */
function recordFrames(page: Page, shareId: string, ms: number, statsEveryMs: number): Promise<Recording> {
  return page.evaluate(
    async ({ id, duration, every }) => {
      const video = document.querySelector<HTMLVideoElement>(
        `[data-viewer-stage][data-share-id="${CSS.escape(id)}"] video`,
      );
      if (!video) throw new Error(`the stage shows no video of ${id}`);
      const frames: FrameSample[] = [];
      const receivers: ReceiverSample[] = [];
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

      const pcs = (window as unknown as { __latencyPCs?: RTCPeerConnection[] }).__latencyPCs ?? [];
      const trackIds = (el: HTMLMediaElement, kind: 'audio' | 'video'): string[] => {
        const src = el.srcObject;
        if (!(src instanceof MediaStream)) return [];
        return (kind === 'audio' ? src.getAudioTracks() : src.getVideoTracks()).map((t) => t.id);
      };
      const num = (s: Record<string, unknown>, key: string): number | undefined => {
        const v = s[key];
        return typeof v === 'number' ? v : undefined;
      };
      const sample = async (): Promise<void> => {
        const videoTracks = trackIds(video, 'video');
        const audioTracks = [...document.querySelectorAll('audio')].flatMap((el) => trackIds(el, 'audio'));
        const got: Omit<ReceiverSample, 'at'> = { video: null, audio: null, playout: null };
        for (const pc of pcs) {
          if (pc.connectionState === 'closed') continue;
          const stats: Record<string, unknown>[] = [];
          (await pc.getStats()).forEach((s: Record<string, unknown>) => {
            stats.push(s);
          });
          let playoutId: unknown;
          for (const s of stats) {
            if (s['type'] !== 'inbound-rtp') continue;
            const track = String(s['trackIdentifier']);
            if (s['kind'] === 'video' && videoTracks.includes(track)) {
              const emitted = num(s, 'jitterBufferEmittedCount');
              const targetDelay = num(s, 'jitterBufferTargetDelay');
              const minimumDelay = num(s, 'jitterBufferMinimumDelay');
              if (emitted !== undefined && targetDelay !== undefined && minimumDelay !== undefined) {
                got.video = { emitted, targetDelay, minimumDelay };
              }
            } else if (s['kind'] === 'audio' && audioTracks.includes(track)) {
              const emitted = num(s, 'jitterBufferEmittedCount');
              const jitterBufferDelay = num(s, 'jitterBufferDelay');
              if (emitted !== undefined && jitterBufferDelay !== undefined) got.audio = { emitted, jitterBufferDelay };
              playoutId = s['playoutId'];
            }
          }
          const playout = stats.find((s) => s['type'] === 'media-playout' && s['id'] === playoutId);
          const samples = playout && num(playout, 'totalSamplesCount');
          const playoutDelay = playout && num(playout, 'totalPlayoutDelay');
          if (samples !== undefined && playoutDelay !== undefined) got.playout = { samples, playoutDelay };
        }
        receivers.push({ at: performance.now(), ...got });
      };
      const end = performance.now() + duration;
      const sampling = (async () => {
        while (performance.now() < end) {
          await sample();
          await new Promise((resolve) => setTimeout(resolve, every));
        }
        await sample();
      })();

      video.requestVideoFrameCallback(step);
      await new Promise((resolve) => setTimeout(resolve, duration));
      recording = false;
      await sampling;
      return { frames, receivers };
    },
    { id: shareId, duration: ms, every: statsEveryMs },
  );
}

/**
 * The intervals between consecutive receiver samples, each with the lip-sync hold on the frames emitted in it:
 * Δ(jitterBufferTargetDelay − jitterBufferMinimumDelay) / ΔjitterBufferEmittedCount, never below 0; and the audio
 * path over it: NetEq's ΔjitterBufferDelay / ΔjitterBufferEmittedCount and the output's ΔtotalPlayoutDelay /
 * ΔtotalSamplesCount. An interval in which the video emitted no frame is left out.
 */
function holdIntervals(receivers: ReceiverSample[]): HoldInterval[] {
  const intervals: HoldInterval[] = [];
  const msPer = (delta: number, count: number): number | null => (count > 0 ? (delta / count) * 1000 : null);
  for (const [i, b] of receivers.entries()) {
    const a = receivers[i - 1];
    if (!a?.video || !b.video) continue;
    const hold = msPer(
      b.video.targetDelay - b.video.minimumDelay - (a.video.targetDelay - a.video.minimumDelay),
      b.video.emitted - a.video.emitted,
    );
    if (hold === null) continue;
    intervals.push({
      from: a.at,
      to: b.at,
      hold: Math.max(0, hold),
      audioJitterBuffer:
        a.audio && b.audio
          ? msPer(b.audio.jitterBufferDelay - a.audio.jitterBufferDelay, b.audio.emitted - a.audio.emitted)
          : null,
      audioOutput:
        a.playout && b.playout
          ? msPer(b.playout.playoutDelay - a.playout.playoutDelay, b.playout.samples - a.playout.samples)
          : null,
    });
  }
  return intervals;
}

/** The hold of the interval that `t` falls in, else of the nearest one; null without any interval. */
function holdAt(intervals: HoldInterval[], t: number): number | null {
  let nearest: HoldInterval | undefined;
  let gap = Infinity;
  for (const iv of intervals) {
    if (t >= iv.from && t < iv.to) return iv.hold;
    const d = t < iv.from ? iv.from - t : t - iv.to;
    if (d < gap) {
      nearest = iv;
      gap = d;
    }
  }
  return nearest?.hold ?? null;
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

/** The audio path over the recording: "NetEq median 20.0 ms (max 40.0), output median 33.8 ms (max 33.8)". */
function describeAudio(intervals: HoldInterval[]): string {
  const part = (label: string, pick: (iv: HoldInterval) => number | null): string => {
    const s = summarize(intervals.flatMap((iv) => pick(iv) ?? []));
    return s ? `${label} median ${s.median.toFixed(1)} ms (max ${s.max.toFixed(1)})` : `${label} not measured`;
  };
  return `${part('NetEq', (iv) => iv.audioJitterBuffer)}, ${part('output', (iv) => iv.audioOutput)}`;
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

  // The friend is in the lounge before anybody shares; the recording reads the friend's receivers.
  const friend = await asUser(server);
  await friend.context.addInitScript(keepPeerConnections);
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

    // 20 s of presented frames, and the receivers' stats over them.
    const { frames, receivers } = await recordFrames(friend.page, share.id, RECORD_MS, STATS_EVERY_MS);
    const intervals = holdIntervals(receivers);
    const holdOf = (f: FrameSample): number => holdAt(intervals, f.expectedDisplayTime) ?? 0;
    // A frame's leg on the video path: as shown, less the hold it was shown under. A frame that came after its held
    // render time (behind a keyframe) was shown on arrival, so the hold didn't delay it: the hold takes off at most
    // what the leg has above 0, so such a frame counts as 0 and stays below the median and the p95, where it was.
    const legs = (pick: (f: FrameSample) => number | null) => {
      const shown = frames.flatMap((f) => {
        const v = pick(f);
        return v === null ? [] : [{ f, v }];
      });
      return {
        asShown: summarize(shown.map(({ v }) => v)),
        videoPath: summarize(shown.map(({ f, v }) => v - Math.min(holdOf(f), Math.max(v, 0)))),
      };
    };
    const captureLegs = legs((f) => (f.captureTime === null ? null : f.expectedDisplayTime - f.captureTime));
    const receiveLegs = legs((f) => (f.receiveTime === null ? null : f.expectedDisplayTime - f.receiveTime));
    const capture = captureLegs.videoPath;
    const receive = receiveLegs.videoPath;
    const hold = intervals.length > 0 ? summarize(frames.map(holdOf)) : null;
    const withCapture = (captureLegs.asShown?.frames ?? 0) >= MIN_FRAMES;
    await testInfo.attach('latency.json', {
      body: JSON.stringify(
        {
          recordMs: RECORD_MS,
          statsEveryMs: STATS_EVERY_MS,
          capture,
          receive,
          captureAsShown: captureLegs.asShown,
          receiveAsShown: receiveLegs.asShown,
          hold,
          intervals,
          frames,
          receivers,
        },
        null,
        2,
      ),
      contentType: 'application/json',
    });
    const captureLine = `expectedDisplayTime − captureTime less the lip-sync hold: ${describeLeg(capture)}`;
    const receiveLine = `expectedDisplayTime − receiveTime less the lip-sync hold: ${describeLeg(receive)}`;
    const shownLine =
      `as shown: expectedDisplayTime − captureTime ${describeLeg(captureLegs.asShown)}; ` +
      `expectedDisplayTime − receiveTime ${describeLeg(receiveLegs.asShown)}`;
    const holdLine = hold
      ? `the lip-sync hold: ${describeLeg(hold)}; audio path: ${describeAudio(intervals)}`
      : `the lip-sync hold: not measured (no video receiver stats in ${String(receivers.length)} samples)`;
    console.log(
      `latency.spec: ${String(frames.length)} frames in ${String(RECORD_MS)} ms; ${captureLine}; ${receiveLine}; ` +
        `${shownLine}; ${holdLine}`,
    );
    testInfo.annotations.push({ type: 'measured', description: captureLine });
    testInfo.annotations.push({ type: 'measured', description: receiveLine });
    testInfo.annotations.push({ type: 'measured', description: shownLine });
    testInfo.annotations.push({ type: hold ? 'measured' : 'not measured', description: holdLine });

    if (withCapture && capture && captureLegs.asShown) {
      // The capture → display leg. A frame shows after it was captured: a leg below zero means captureTime is off
      // (a wrong RTP timestamp or sender report), which would pass the bounds without measuring anything. That check
      // is on the legs as shown, before the hold comes off.
      expect(capture.median, captureLine).toBeLessThan(CAPTURE_MEDIAN_MS);
      expect(capture.p95, captureLine).toBeLessThan(CAPTURE_P95_MS);
      expect(captureLegs.asShown.median, `${shownLine}: a frame shown before it was captured`).toBeGreaterThan(0);
    } else {
      testInfo.annotations.push({
        type: 'not measured',
        description:
          `the capture leg: Chrome gave captureTime on ${String(captureLegs.asShown?.frames ?? 0)} of ` +
          `${String(frames.length)} frames (fewer than ${String(MIN_FRAMES)}), so the receive leg is asserted instead`,
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
