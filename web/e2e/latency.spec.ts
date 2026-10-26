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
// Either way it catches timestamp mistakes that make Chrome's jitter buffer grow, and through the lip-sync hold
// (below) sender-report mistakes too. The values are logged and annotated, pass or fail, and the frames' metadata and
// the receivers' stats are attached to the report.
//
// The lip-sync hold. A share's audio and video have one msid stream (M1 02 §9.6), so Chrome shows a frame no earlier
// than the audio captured with it (01 §12.1): when the audio path (NetEq, then the audio output) takes longer than the
// video's own jitter buffer, every frame waits for the difference. That wait follows the viewer's audio output more
// than anything this spec checks. On an M5 Mac the legs as shown are about 30 ms playing to a display's speakers over
// HDMI (an output delay of 33.8 ms, a hold of 0–32 ms), and at median 136.3 ms playing to Bluetooth headphones (180
// ms; the hold climbs to 160 ms through the recording), where the same frames less their hold are at 43.1 ms. In PR
// #41's e2e job (headful Chrome under Xvfb playing into a PulseAudio null sink, on a 4-vCPU runner) they were at
// median 179.4 / 166.0 ms, p95 279.8 / 266.0 ms, min 19.3 / 16.4 ms and max 280.6 / 283.2 ms (585 frames): the same
// climb. So the legs are asserted less the hold, and the hold has bounds of its own.
//
// Chrome's stats give the hold: the video receiver's jitterBufferTargetDelay − jitterBufferMinimumDelay per emitted
// frame (webrtc-stats: the minimum leaves out A/V sync and jitterBufferTarget, which this app never sets). While it
// records, the spec samples the stage's receivers every 250 ms and gives each frame the hold, NetEq's delay, the
// output's delay and the video's own delay (its minimum) of the interval it was shown in. Then:
// - the hold must be the audio path's: its p95 over the frames may exceed max(0, NetEq + output − the video's own
//   delay) by less than 60 ms. Chrome's sync (webrtc's StreamSynchronization) aims the hold at that difference plus
//   the offset of the audio's and the video's arrivals against their sender reports, leaves a difference under 30 ms
//   alone and closes a part of the rest each second. With the stamps right the excess stayed at or below +29 ms at
//   the p95 in 13 runs on the Mac (HDMI and Bluetooth outputs, CPU load, Playwright's video recording, a 250 ms
//   stall of the tool, the audio's jitterBufferTarget held at 250 ms), and at most +37 ms, while the hold lagged a
//   NetEq that fell after the stall. An offset between the share's audio and video sender reports or capture stamps,
//   or a publisher that sends one of them late, is a hold the audio path doesn't explain: with the tool's audio
//   sender reports 150 ms early the excess came out at p95 113 and 95 ms, and 300 ms early at 264 ms (playing to
//   Bluetooth, where the hold has the longest way to climb);
// - the hold's p95 must be under 400 ms. A frame's hold is part of its leg, so CI's legs as shown bound the hold there
//   from above (p95 279.8 and 266.0 ms), and an audio path broken by seconds still fails.
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
/** The bound on the hold's p95 beyond what the audio path explains (the top of the file). */
const UNEXPLAINED_HOLD_P95_MS = 60;
/** The bound on the hold's p95 (the top of the file: CI's legs as shown had p95 279.8 and 266.0 ms). */
const HOLD_P95_MS = 400;

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
  /** The inbound-rtp of the audible audio track (NetEq) and its media-playout (the output). */
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
  /** The video's own delay per emitted frame (its jitter buffer's minimum), ms. */
  videoDelay: number;
  /** NetEq's delay per emitted sample and the output's delay per played sample, ms; null without them. */
  audioJitterBuffer: number | null;
  audioOutput: number | null;
  /**
   * The hold less what the audio path explains, max(0, NetEq + output − videoDelay), ms. Without NetEq's delay the
   * audio path explains nothing, and without the output's it counts as 0.
   */
  unexplained: number;
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
 * Δ(jitterBufferTargetDelay − jitterBufferMinimumDelay) / ΔjitterBufferEmittedCount, never below 0; the video's own
 * delay, ΔjitterBufferMinimumDelay / ΔjitterBufferEmittedCount; the audio path over it, NetEq's ΔjitterBufferDelay /
 * ΔjitterBufferEmittedCount and the output's ΔtotalPlayoutDelay / ΔtotalSamplesCount; and the part of the hold that
 * the audio path doesn't explain. An interval in which the video emitted no frame is left out.
 */
function holdIntervals(receivers: ReceiverSample[]): HoldInterval[] {
  const intervals: HoldInterval[] = [];
  const msPer = (delta: number, count: number): number | null => (count > 0 ? (delta / count) * 1000 : null);
  for (const [i, b] of receivers.entries()) {
    const a = receivers[i - 1];
    if (!a?.video || !b.video) continue;
    const emitted = b.video.emitted - a.video.emitted;
    const hold = msPer(
      b.video.targetDelay - b.video.minimumDelay - (a.video.targetDelay - a.video.minimumDelay),
      emitted,
    );
    const videoDelay = msPer(b.video.minimumDelay - a.video.minimumDelay, emitted);
    if (hold === null || videoDelay === null) continue;
    const audioJitterBuffer =
      a.audio && b.audio
        ? msPer(b.audio.jitterBufferDelay - a.audio.jitterBufferDelay, b.audio.emitted - a.audio.emitted)
        : null;
    const audioOutput =
      a.playout && b.playout
        ? msPer(b.playout.playoutDelay - a.playout.playoutDelay, b.playout.samples - a.playout.samples)
        : null;
    const explained = audioJitterBuffer === null ? 0 : Math.max(0, audioJitterBuffer + (audioOutput ?? 0) - videoDelay);
    intervals.push({
      from: a.at,
      to: b.at,
      hold: Math.max(0, hold),
      videoDelay,
      audioJitterBuffer,
      audioOutput,
      unexplained: Math.max(0, hold) - explained,
    });
  }
  return intervals;
}

/** The interval that `t` falls in, else the nearest one; undefined without any interval. */
function intervalAt(intervals: HoldInterval[], t: number): HoldInterval | undefined {
  let nearest: HoldInterval | undefined;
  let gap = Infinity;
  for (const iv of intervals) {
    if (t >= iv.from && t < iv.to) return iv;
    const d = t < iv.from ? iv.from - t : t - iv.to;
    if (d < gap) {
      nearest = iv;
      gap = d;
    }
  }
  return nearest;
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

/**
 * The audio path and the video's own delay over the recording, per interval: "NetEq median 20.0 ms (max 40.0), output
 * median 33.8 ms (max 33.8), the video's own delay median 20.4 ms (max 22.1)".
 */
function describeAudio(intervals: HoldInterval[]): string {
  const part = (label: string, pick: (iv: HoldInterval) => number | null): string => {
    const s = summarize(intervals.flatMap((iv) => pick(iv) ?? []));
    return s ? `${label} median ${s.median.toFixed(1)} ms (max ${s.max.toFixed(1)})` : `${label} not measured`;
  };
  return (
    `${part('NetEq', (iv) => iv.audioJitterBuffer)}, ${part('output', (iv) => iv.audioOutput)}, ` +
    part("the video's own delay", (iv) => iv.videoDelay)
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
    const measuredHold = intervals.length > 0;
    const holdOf = (f: FrameSample): number => intervalAt(intervals, f.expectedDisplayTime)?.hold ?? 0;
    const unexplainedOf = (f: FrameSample): number => intervalAt(intervals, f.expectedDisplayTime)?.unexplained ?? 0;
    // A frame's leg less the hold it was shown under. A frame that came after its held render time (behind a
    // keyframe) was shown on arrival, so the hold didn't delay it: the hold takes off at most what the leg has above
    // 0, so such a frame counts as 0 and stays below the median and the p95, where it was.
    const legs = (pick: (f: FrameSample) => number | null) => {
      const shown = frames.flatMap((f) => {
        const v = pick(f);
        return v === null ? [] : [{ f, v }];
      });
      return {
        asShown: summarize(shown.map(({ v }) => v)),
        lessHold: summarize(shown.map(({ f, v }) => v - Math.min(holdOf(f), Math.max(v, 0)))),
      };
    };
    const captureLegs = legs((f) => (f.captureTime === null ? null : f.expectedDisplayTime - f.captureTime));
    const receiveLegs = legs((f) => (f.receiveTime === null ? null : f.expectedDisplayTime - f.receiveTime));
    // The legs less the hold when the stats gave it, else as shown.
    const capture = measuredHold ? captureLegs.lessHold : captureLegs.asShown;
    const receive = measuredHold ? receiveLegs.lessHold : receiveLegs.asShown;
    const hold = measuredHold ? summarize(frames.map(holdOf)) : null;
    const unexplained = measuredHold ? summarize(frames.map(unexplainedOf)) : null;
    const withCapture = (captureLegs.asShown?.frames ?? 0) >= MIN_FRAMES;
    await testInfo.attach('latency.json', {
      body: JSON.stringify(
        {
          recordMs: RECORD_MS,
          statsEveryMs: STATS_EVERY_MS,
          captureAsShown: captureLegs.asShown,
          receiveAsShown: receiveLegs.asShown,
          captureLessHold: captureLegs.lessHold,
          receiveLessHold: receiveLegs.lessHold,
          hold,
          unexplained,
          intervals,
          frames,
          receivers,
        },
        null,
        2,
      ),
      contentType: 'application/json',
    });
    const asserted = measuredHold ? 'less the lip-sync hold' : 'as shown (the hold not measured)';
    const captureLine = `expectedDisplayTime − captureTime ${asserted}: ${describeLeg(capture)}`;
    const receiveLine = `expectedDisplayTime − receiveTime ${asserted}: ${describeLeg(receive)}`;
    const shownLine =
      `as shown: expectedDisplayTime − captureTime ${describeLeg(captureLegs.asShown)}; ` +
      `expectedDisplayTime − receiveTime ${describeLeg(receiveLegs.asShown)}`;
    const holdLine = hold
      ? `the lip-sync hold: ${describeLeg(hold)}; beyond what the audio path explains: ${describeLeg(unexplained)}; ` +
        describeAudio(intervals)
      : `the lip-sync hold: not measured (no video receiver stats in ${String(receivers.length)} samples)`;
    console.log(
      `latency.spec: ${String(frames.length)} frames in ${String(RECORD_MS)} ms; ${captureLine}; ${receiveLine}; ` +
        `${shownLine}; ${holdLine}`,
    );
    testInfo.annotations.push({ type: 'measured', description: captureLine });
    testInfo.annotations.push({ type: 'measured', description: receiveLine });
    testInfo.annotations.push({ type: 'measured', description: shownLine });
    testInfo.annotations.push({ type: measuredHold ? 'measured' : 'not measured', description: holdLine });

    // The hold that comes off the legs is the audio path's, within bounds (the top of the file): more than the audio
    // path explains is an offset between the share's audio and video.
    if (hold && unexplained) {
      expect(
        unexplained.p95,
        `${holdLine}: a lip-sync hold that the audio path doesn't explain (the share's audio and video sender ` +
          'reports or capture stamps disagree, or the publisher sends one of them late)',
      ).toBeLessThan(UNEXPLAINED_HOLD_P95_MS);
      expect(hold.p95, `${holdLine}: the lip-sync hold`).toBeLessThan(HOLD_P95_MS);
    }

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
