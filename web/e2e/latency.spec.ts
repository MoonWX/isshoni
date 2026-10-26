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
// Either way it catches timestamp mistakes that make Chrome's jitter buffer grow, and through the A/V offset (below)
// sender-report mistakes too. The values are logged and annotated, pass or fail, and the frames' metadata and the
// receivers' stats are attached to the report.
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
// records, the spec samples the stage's receivers every 250 ms and gives each frame the hold of the interval it was
// shown in. The hold's p95 must be under 400 ms: a frame's hold is part of its leg, so CI's legs as shown bound the
// hold there from above (p95 279.8 and 266.0 ms), and an audio path broken by seconds still fails.
//
// The A/V offset. Chrome lines a share's audio and video up by their sender reports (webrtc's RtpStreamsSynchronizer):
// each track's RTP → NTP mapping says when the audio it plays and the frame it shows were captured, and Chrome holds
// the video (or the audio) until the two match. getStats gives both instants, each inbound-rtp's
// estimatedPlayoutTimestamp: the sender's NTP time of what plays now, which here is the tool's wall clock, this
// machine's. So now − estimatedPlayoutTimestamp is a track's age by its sender reports. The receiver measures the
// same age without them: the audio's is NetEq's delay plus the output's, the video's the frame's receive → display
// leg (the loopback network adds next to nothing to either). A track's age by its reports less its measured age is
// what its capture stamps, its reports and its sending add; the audio's less the video's is the share's A/V offset,
// by which Chrome holds the video beyond what the paths need (or the audio, when negative), and a clock error common
// to both tracks drops out. Per interval, the spec takes the reports' ages at its two ends against NetEq's and the
// output's delay over it and the median leg of the frames shown in it, and the offset's median over the recording
// must be under 60 ms. (An offset the other way makes Chrome delay the audio, which takes nothing off the legs.) The
// median, because the receiver's side lags in a transient: NetEq times its packets' wait in ticks of its own playout,
// so packets that came before Chrome's audio output started pulling count only from then on (the first second or two
// of the audio's measured age then falls short by up to seconds), and a frame shown on arrival after a keyframe has a
// short leg. With the stamps right the median was −13 to −22 ms in 10 runs on the Mac (playing to Bluetooth
// headphones; 4 of the runs at once), and −16 and −14 ms with the stage's audio element started 1.5 and 2.5 s late,
// which gives the backlog of PR #42's e2e job (below). With the tool's audio sender reports 100 ms early it was
// +80 ms, and +89 ms with the late start; 150 ms early, +127 ms. It shows from the first interval on.
//
// Why not the hold against the audio path, as PR #41 had it (the hold's p95 beyond max(0, NetEq + output − the video's
// own delay) < 60 ms)? Chrome's sync (webrtc's StreamSynchronization) moves the hold once a second by half of a 4-tap
// average of the difference, at most 80 ms and not at all under 30 ms, and starts the average again after a move: it
// takes a hold back by about an eighth of the excess per second. In PR #42's e2e job Chrome's audio output started
// pulling about 1.5 s after the share's audio arrived; NetEq played that backlog out (its delay per emitted sample
// peaked at 1.3 s), Chrome raised the hold to 249 and 226 ms at 80 ms a second, and 12 s after NetEq had drained the
// hold was still 43–54 ms above the audio path: that check failed at p95 138 and 135 ms, although that controller,
// fitted to both recordings once NetEq had drained (2.9 ms RMS), puts their offset 10–15 ms below the Mac's. An
// offset, conversely, shows in the hold only as fast as the hold climbs (the tool's reports 150 ms early gave PR #41
// a p95 of 95–113 ms).
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
/** The bound on the median of the share's A/V offset by its sender reports (the top of the file). */
const AV_OFFSET_MS = 60;
/**
 * The fewest intervals the A/V offset is judged on: half of the recording's. Chrome gives estimatedPlayoutTimestamp
 * once it has both tracks' sender reports, a second or two into the audio.
 */
const MIN_OFFSET_INTERVALS = RECORD_MS / STATS_EVERY_MS / 2;
/**
 * From the NTP epoch (1900) to the Unix one (1970), ms. Chrome gives estimatedPlayoutTimestamp on the NTP epoch
 * (webrtc-stats has it on the Unix one); reportAge reads either.
 */
const NTP_UNIX_OFFSET_MS = 2_208_988_800_000;
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

/**
 * When the media that a track plays now was captured, by its sender reports: the inbound-rtp's
 * estimatedPlayoutTimestamp, and the stats' own timestamp (ms, as getStats has them). A receiver's `playout` is null
 * while Chrome gives no estimatedPlayoutTimestamp.
 */
interface Playout {
  timestamp: number;
  estimatedPlayoutTimestamp: number;
}

/** The stage's receivers at one moment of the recording: getStats' cumulative counters, in s as getStats has them. */
interface ReceiverSample {
  /** performance.now() when the stats came back: the clock of the frames' times. */
  at: number;
  /** The inbound-rtp of the stage's video track; null when getStats has no such stream or lacks these counters. */
  video: { emitted: number; targetDelay: number; minimumDelay: number; playout: Playout | null } | null;
  /** The inbound-rtp of the audible audio track (NetEq) and its media-playout (the output). */
  audio: { emitted: number; jitterBufferDelay: number; playout: Playout | null } | null;
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
   * The hold less what the audio path asks for now, max(0, NetEq + output − videoDelay), ms; without NetEq's delay
   * the audio path asks for nothing, and without the output's it counts as 0. Logged, not asserted: Chrome's sync
   * takes a hold back slowly when the audio path falls (the top of the file).
   */
  beyondAudioPath: number;
  /**
   * Each track's age by its sender reports over the interval, ms: now − estimatedPlayoutTimestamp, the mean of the
   * interval's two ends; null where Chrome gave none.
   */
  audioReportAge: number | null;
  videoReportAge: number | null;
  /** The median receive → display leg of the frames shown in the interval, ms; null without any. */
  videoLeg: number | null;
  /**
   * The share's A/V offset by its sender reports, ms: the audio's report age less NetEq + output, less the video's
   * report age less videoLeg (the top of the file); null without any of them.
   */
  avOffset: number | null;
}

/** A distribution in ms over `count` frames (a leg, the hold) or intervals (the A/V offset); nearest-rank percentiles. */
interface Summary {
  count: number;
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
 * one <audio> element, 05 §10.2) with its media-playout; with each inbound-rtp, its estimatedPlayoutTimestamp.
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
      const playoutOf = (s: Record<string, unknown>): Playout | null => {
        const timestamp = num(s, 'timestamp');
        const estimatedPlayoutTimestamp = num(s, 'estimatedPlayoutTimestamp');
        return timestamp !== undefined && estimatedPlayoutTimestamp !== undefined
          ? { timestamp, estimatedPlayoutTimestamp }
          : null;
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
                got.video = { emitted, targetDelay, minimumDelay, playout: playoutOf(s) };
              }
            } else if (s['kind'] === 'audio' && audioTracks.includes(track)) {
              const emitted = num(s, 'jitterBufferEmittedCount');
              const jitterBufferDelay = num(s, 'jitterBufferDelay');
              if (emitted !== undefined && jitterBufferDelay !== undefined) {
                got.audio = { emitted, jitterBufferDelay, playout: playoutOf(s) };
              }
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
 * A track's age by its sender reports at one sample, ms: the stats' timestamp less estimatedPlayoutTimestamp, the
 * latter moved to the Unix epoch when it is on the NTP one (70 years ahead); null without it.
 */
function reportAge(p: Playout | null | undefined): number | null {
  if (!p) return null;
  const { timestamp, estimatedPlayoutTimestamp: played } = p;
  return timestamp - (played - timestamp > NTP_UNIX_OFFSET_MS / 2 ? played - NTP_UNIX_OFFSET_MS : played);
}

/**
 * The intervals between consecutive receiver samples, each with the lip-sync hold on the frames emitted in it:
 * Δ(jitterBufferTargetDelay − jitterBufferMinimumDelay) / ΔjitterBufferEmittedCount, never below 0; the video's own
 * delay, ΔjitterBufferMinimumDelay / ΔjitterBufferEmittedCount; the audio path over it, NetEq's ΔjitterBufferDelay /
 * ΔjitterBufferEmittedCount and the output's ΔtotalPlayoutDelay / ΔtotalSamplesCount; the part of the hold beyond what
 * the audio path asks for; each track's age by its sender reports, the mean of its reportAge at the two ends; the
 * median receive → display leg of the frames shown in it; and the share's A/V offset from those (the top of the
 * file). An interval in which the video emitted no frame is left out.
 */
function holdIntervals(receivers: ReceiverSample[], frames: FrameSample[]): HoldInterval[] {
  const intervals: HoldInterval[] = [];
  const msPer = (delta: number, count: number): number | null => (count > 0 ? (delta / count) * 1000 : null);
  const mean = (x: number | null, y: number | null): number | null => (x === null || y === null ? null : (x + y) / 2);
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
    const asked = audioJitterBuffer === null ? 0 : Math.max(0, audioJitterBuffer + (audioOutput ?? 0) - videoDelay);
    const audioReportAge = mean(reportAge(a.audio?.playout), reportAge(b.audio?.playout));
    const videoReportAge = mean(reportAge(a.video.playout), reportAge(b.video.playout));
    const videoLeg =
      summarize(
        frames.flatMap((f) =>
          f.receiveTime !== null && f.expectedDisplayTime >= a.at && f.expectedDisplayTime < b.at
            ? [f.expectedDisplayTime - f.receiveTime]
            : [],
        ),
      )?.median ?? null;
    const avOffset =
      audioReportAge === null ||
      audioJitterBuffer === null ||
      audioOutput === null ||
      videoReportAge === null ||
      videoLeg === null
        ? null
        : audioReportAge - (audioJitterBuffer + audioOutput) - (videoReportAge - videoLeg);
    intervals.push({
      from: a.at,
      to: b.at,
      hold: Math.max(0, hold),
      videoDelay,
      audioJitterBuffer,
      audioOutput,
      beyondAudioPath: Math.max(0, hold) - asked,
      audioReportAge,
      videoReportAge,
      videoLeg,
      avOffset,
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
    count: sorted.length,
    min: sorted[0] ?? NaN,
    median: rank(50),
    p95: rank(95),
    max: sorted[sorted.length - 1] ?? NaN,
  };
}

/** A summary for the log: "600 frames, median 41.2 ms, p95 52.3 ms (min 30.1, max 70.4)". */
function describeLeg(s: Summary | null, unit = 'frames'): string {
  if (!s) return `no ${unit}`;
  const ms = (v: number): string => v.toFixed(1);
  return (
    `${String(s.count)} ${unit}, median ${ms(s.median)} ms, p95 ${ms(s.p95)} ms ` +
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

/**
 * The share's A/V offset over the recording, and what each track adds to it (medians over the same intervals):
 * "78 intervals, median -11.9 ms, p95 -7.1 ms (min -33.3, max 1.7); the audio's report age less NetEq + output
 * median -8.1 ms, the video's less its leg median 3.4 ms".
 */
function describeOffset(intervals: HoldInterval[], offset: Summary | null): string {
  const judged = intervals.filter((iv) => iv.avOffset !== null);
  const median = (pick: (iv: HoldInterval) => number): string =>
    `median ${(summarize(judged.map(pick))?.median ?? NaN).toFixed(1)} ms`;
  if (!offset) return describeLeg(offset, 'intervals');
  return (
    `${describeLeg(offset, 'intervals')}; the audio's report age less NetEq + output ` +
    `${median((iv) => (iv.audioReportAge ?? NaN) - (iv.audioJitterBuffer ?? NaN) - (iv.audioOutput ?? NaN))}, ` +
    `the video's less its leg ${median((iv) => (iv.videoReportAge ?? NaN) - (iv.videoLeg ?? NaN))}`
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
    const intervals = holdIntervals(receivers, frames);
    const measuredHold = intervals.length > 0;
    const holdOf = (f: FrameSample): number => intervalAt(intervals, f.expectedDisplayTime)?.hold ?? 0;
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
    const avOffset = summarize(intervals.flatMap((iv) => iv.avOffset ?? []));
    const withCapture = (captureLegs.asShown?.count ?? 0) >= MIN_FRAMES;
    // The numbers go to the test's output folder too, so that a passing run keeps them (CI uploads that folder).
    const jsonPath = testInfo.outputPath('latency.json');
    await fsp.writeFile(
      jsonPath,
      JSON.stringify(
        {
          recordMs: RECORD_MS,
          statsEveryMs: STATS_EVERY_MS,
          captureAsShown: captureLegs.asShown,
          receiveAsShown: receiveLegs.asShown,
          captureLessHold: captureLegs.lessHold,
          receiveLessHold: receiveLegs.lessHold,
          hold,
          avOffset,
          intervals,
          frames,
          receivers,
        },
        null,
        2,
      ),
    );
    await testInfo.attach('latency.json', { path: jsonPath, contentType: 'application/json' });
    const asserted = measuredHold ? 'less the lip-sync hold' : 'as shown (the hold not measured)';
    const captureLine = `expectedDisplayTime − captureTime ${asserted}: ${describeLeg(capture)}`;
    const receiveLine = `expectedDisplayTime − receiveTime ${asserted}: ${describeLeg(receive)}`;
    const shownLine =
      `as shown: expectedDisplayTime − captureTime ${describeLeg(captureLegs.asShown)}; ` +
      `expectedDisplayTime − receiveTime ${describeLeg(receiveLegs.asShown)}`;
    const holdLine = hold
      ? `the lip-sync hold: ${describeLeg(hold)}; ${describeAudio(intervals)}`
      : `the lip-sync hold: not measured (no video receiver stats in ${String(receivers.length)} samples)`;
    const offsetLine = `the A/V offset by the sender reports: ${describeOffset(intervals, avOffset)}`;
    console.log(
      `latency.spec: ${String(frames.length)} frames in ${String(RECORD_MS)} ms; ${captureLine}; ${receiveLine}; ` +
        `${shownLine}; ${holdLine}; ${offsetLine}`,
    );
    testInfo.annotations.push({ type: 'measured', description: captureLine });
    testInfo.annotations.push({ type: 'measured', description: receiveLine });
    testInfo.annotations.push({ type: 'measured', description: shownLine });
    testInfo.annotations.push({ type: measuredHold ? 'measured' : 'not measured', description: holdLine });
    testInfo.annotations.push({ type: avOffset ? 'measured' : 'not measured', description: offsetLine });

    // With the receivers' stats: the share's audio and video line up by their sender reports, and the hold that
    // comes off the legs stays within its bound (the top of the file).
    if (hold) {
      expect(
        avOffset?.count ?? 0,
        `${offsetLine}: intervals with both tracks' estimatedPlayoutTimestamp, NetEq's and the output's delay and a ` +
          'shown frame',
      ).toBeGreaterThanOrEqual(MIN_OFFSET_INTERVALS);
      expect(
        avOffset?.median ?? NaN,
        `${offsetLine}: the share's audio and video sender reports or capture stamps disagree, or the publisher ` +
          'sends its audio late: Chrome holds the video for it',
      ).toBeLessThan(AV_OFFSET_MS);
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
          `the capture leg: Chrome gave captureTime on ${String(captureLegs.asShown?.count ?? 0)} of ` +
          `${String(frames.length)} frames (fewer than ${String(MIN_FRAMES)}), so the receive leg is asserted instead`,
      });
      expect(
        receive?.count ?? 0,
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
