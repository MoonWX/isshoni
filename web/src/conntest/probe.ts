// One connection-test probe (05 §14.2): a real ICE check of one transport against the server's probe
// PeerConnection (04 §7.7 on top of 02 §7.6).
//
//   1. pc = platform.createPeerConnection({iceServers: [], bundlePolicy: 'max-bundle'}); dc = pc.createDataChannel
//   2. createOffer → setLocalDescription. Nobody waits for gathering: the server learns this side's addresses as
//      peer-reflexive candidates from the browser's own checks.
//   3. POST /api/v1/conntest {transport, offer} → setRemoteDescription(answer). The answer is complete (no trickle)
//      and holds only that transport's candidates.
//   4. Success = connectionState "connected" and the channel open within 8 s (04's ICE failed timeout).
//   5. RTT: 5 messages {"n": i, "t": performance.now()} 200 ms apart, which the server echoes unchanged; the median.
//      Without an echo, the selected candidate pair's currentRoundTripTime.
//   6. Close the PC.
//
// No React here (05 §3 layering rule), and no SDP, candidate or address is logged or kept (05 §20).
import { LocalError } from '../lib/errors';
import type { Platform } from '../platform/types';
import {
  CodeRateLimited,
  CodeTransportDisabled,
  TransportTCP443,
  TransportTCP7882,
  TransportUDP,
  type ConnTestRequest,
  type ConnTestResponse,
  type ConnTestServerInfo,
} from '../protocol/api.gen';
import { api, ApiError, isSignedOutError } from '../protocol/rest';

/** The probes of one run, in the order the result lists them. Each is a `Transport` of 04 §7.3. */
export const PROBE_TRANSPORTS = [TransportUDP, TransportTCP443, TransportTCP7882] as const;

export type ProbeTransport = (typeof PROBE_TRANSPORTS)[number]; // 'udp' | 'tcp443' | 'tcp7882'

export interface ProbeVerdict {
  transport: ProbeTransport;
  /** disabled: 409 transport_disabled, for any transport without a server listener. */
  result: 'ok' | 'failed' | 'disabled';
  /** Median of the 5 data-channel echoes; fallback currentRoundTripTime. Only with result "ok". */
  rttMs?: number;
  /**
   * Why a probe failed. "timeout": no connection within 8 s (✗). "server" and "rate_limited" mean the probe could
   * not run, so the transport is "not tested": never ✗. A failed probe without an error connected nowhere before the
   * 8 s were over (ICE or the channel failed outright): ✗ as well.
   */
  error?: 'timeout' | 'server' | 'rate_limited';
}

/** 05 §18: connect within 8 s; 5 pings, 200 ms apart. */
export const CONNECT_TIMEOUT_MS = 8000;
export const PING_COUNT = 5;
export const PING_INTERVAL_MS = 200;
/** How long the last echoes may take after the last ping before the median is taken from what arrived. */
export const ECHO_WAIT_MS = 1000;
/** POST /api/v1/conntest answers at once (the server gathers nothing); past this the probe counts as not tested. */
export const REQUEST_TIMEOUT_MS = 10_000;

/** What one probe found: its verdict and what the server's reply said besides the answer. */
export interface ProbeOutcome {
  verdict: ProbeVerdict;
  /** From a 200 reply. The public IP in it is never kept (runConnTest derives two flags from it). */
  server?: ConnTestServerInfo;
  /** From a 429: seconds until the server takes another probe. */
  retryAfterSec?: number;
}

/** POST /api/v1/conntest. Rejects with ApiError or NetworkError like api(). */
export type ConnTestRequester = (body: ConnTestRequest, signal: AbortSignal) => Promise<ConnTestResponse>;

export interface ProbeOptions {
  /** Stops the probe: the PC is closed and runProbe rejects with the signal's reason. */
  signal?: AbortSignal;
  /** The REST call; default api() (protocol/rest.ts, bound to the platform at boot). Tests pass their own. */
  request?: ConnTestRequester;
}

const defaultRequest: ConnTestRequester = (body, signal) =>
  api<ConnTestResponse>('POST', '/api/v1/conntest', body, { signal });

/** The signal's reason as the error to reject with: any error-like reason as it is, else an AbortError. */
function abortError(signal: AbortSignal | undefined): Error {
  const reason: unknown = signal?.reason;
  if (reason instanceof Error) return reason;
  // An error object of another realm (an iframe's DOMException) is not `instanceof Error` here.
  if (typeof reason === 'object' && reason !== null && 'name' in reason && 'message' in reason) return reason as Error;
  return new DOMException('The operation was aborted.', 'AbortError');
}

function throwIfAborted(signal: AbortSignal | undefined): void {
  if (signal?.aborted) throw abortError(signal);
}

/**
 * Runs request with a signal that aborts when the caller's does or after timeoutMs. (AbortSignal.any and
 * AbortSignal.timeout are newer than the oldest browsers this build targets.)
 */
async function requestWithTimeout(
  request: ConnTestRequester,
  body: ConnTestRequest,
  timeoutMs: number,
  signal: AbortSignal | undefined,
): Promise<ConnTestResponse> {
  const ctl = new AbortController();
  const onAbort = (): void => {
    ctl.abort(abortError(signal));
  };
  const timer = setTimeout(() => {
    ctl.abort(new DOMException('The connection test request timed out.', 'TimeoutError'));
  }, timeoutMs);
  signal?.addEventListener('abort', onAbort, { once: true });
  try {
    return await request(body, ctl.signal);
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener('abort', onAbort);
  }
}

type ConnectResult = 'connected' | 'failed' | 'timeout';

/**
 * Resolves when the PC is connected and the channel open ("connected"), when either can't get there any more
 * ("failed"), or after timeoutMs ("timeout"). Rejects when the signal aborts.
 */
function waitConnected(
  pc: RTCPeerConnection,
  dc: RTCDataChannel,
  timeoutMs: number,
  signal: AbortSignal | undefined,
): Promise<ConnectResult> {
  return new Promise<ConnectResult>((resolve, reject) => {
    const cleanup = (): void => {
      clearTimeout(timer);
      pc.removeEventListener('connectionstatechange', check);
      dc.removeEventListener('open', check);
      dc.removeEventListener('close', check);
      signal?.removeEventListener('abort', onAbort);
    };
    const finish = (result: ConnectResult): void => {
      cleanup();
      resolve(result);
    };
    const check = (): void => {
      const pcState = pc.connectionState;
      const dcState = dc.readyState;
      if (pcState === 'connected' && dcState === 'open') finish('connected');
      else if (pcState === 'failed' || pcState === 'closed' || dcState === 'closing' || dcState === 'closed') {
        finish('failed');
      }
    };
    const onAbort = (): void => {
      cleanup();
      reject(abortError(signal));
    };
    const timer = setTimeout(() => {
      finish('timeout');
    }, timeoutMs);
    pc.addEventListener('connectionstatechange', check);
    dc.addEventListener('open', check);
    dc.addEventListener('close', check);
    signal?.addEventListener('abort', onAbort, { once: true });
    check();
  });
}

/**
 * The `n` of an echoed ping, or undefined for anything else. The server echoes text as text; an echo that comes back
 * as binary is read too (the channel's binaryType is "arraybuffer").
 */
function echoIndex(data: unknown): number | undefined {
  let text: string;
  if (typeof data === 'string') text = data;
  // By tag, not instanceof: a buffer from another realm (a worker, a test environment) is still a buffer.
  else if (Object.prototype.toString.call(data) === '[object ArrayBuffer]') {
    text = new TextDecoder().decode(data as ArrayBuffer);
  } else return undefined;
  try {
    const msg: unknown = JSON.parse(text);
    if (typeof msg !== 'object' || msg === null) return undefined;
    const n: unknown = (msg as Record<string, unknown>)['n'];
    return typeof n === 'number' ? n : undefined;
  } catch {
    return undefined;
  }
}

/**
 * Sends the pings and collects the round-trip time of each echo, in ms. Resolves when every ping was echoed, when
 * the channel closes, or ECHO_WAIT_MS after the last ping, with what arrived (possibly nothing). Rejects when the
 * signal aborts.
 */
function measureEchoes(dc: RTCDataChannel, signal: AbortSignal | undefined): Promise<number[]> {
  return new Promise<number[]>((resolve, reject) => {
    const sentAt = new Map<number, number>();
    const samples: number[] = [];
    let next = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const cleanup = (): void => {
      clearTimeout(timer);
      dc.removeEventListener('message', onMessage);
      dc.removeEventListener('close', finish);
      signal?.removeEventListener('abort', onAbort);
    };
    const finish = (): void => {
      cleanup();
      resolve(samples);
    };
    const onAbort = (): void => {
      cleanup();
      reject(abortError(signal));
    };
    const onMessage = (ev: MessageEvent): void => {
      const now = performance.now();
      const n = echoIndex(ev.data);
      const sent = n === undefined ? undefined : sentAt.get(n);
      if (n === undefined || sent === undefined) return; // not ours, or echoed twice
      sentAt.delete(n);
      samples.push(now - sent);
      if (samples.length === PING_COUNT) finish();
    };
    const ping = (): void => {
      const n = next++;
      const t = performance.now();
      sentAt.set(n, t);
      try {
        dc.send(JSON.stringify({ n, t }));
      } catch {
        finish(); // the channel closed under us
        return;
      }
      timer = next < PING_COUNT ? setTimeout(ping, PING_INTERVAL_MS) : setTimeout(finish, ECHO_WAIT_MS);
    };
    dc.addEventListener('message', onMessage);
    dc.addEventListener('close', finish);
    signal?.addEventListener('abort', onAbort, { once: true });
    ping();
  });
}

/** The median of a non-empty list. */
export function median(values: readonly number[]): number {
  const sorted = [...values].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  const hi = sorted[mid] ?? 0;
  return sorted.length % 2 === 1 ? hi : ((sorted[mid - 1] ?? hi) + hi) / 2;
}

type StatsEntry = Readonly<Record<string, unknown>>;

/**
 * The selected candidate pair's currentRoundTripTime in ms, or undefined. The pair is the transport's
 * selectedCandidatePairId, else the nominated pair that succeeded (Firefox marks it `selected`).
 */
async function statsRttMs(pc: RTCPeerConnection): Promise<number | undefined> {
  try {
    const report = await pc.getStats();
    const pairs: StatsEntry[] = [];
    let selectedId: unknown;
    report.forEach((value: unknown) => {
      if (typeof value !== 'object' || value === null) return;
      const s = value as StatsEntry;
      if (s['type'] === 'transport' && typeof s['selectedCandidatePairId'] === 'string') {
        selectedId = s['selectedCandidatePairId'];
      } else if (s['type'] === 'candidate-pair') pairs.push(s);
    });
    const pair =
      pairs.find((p) => selectedId !== undefined && p['id'] === selectedId) ??
      pairs.find((p) => p['selected'] === true) ??
      pairs.find((p) => p['nominated'] === true && p['state'] === 'succeeded');
    const rtt = pair?.['currentRoundTripTime'];
    return typeof rtt === 'number' && Number.isFinite(rtt) && rtt >= 0 ? rtt * 1000 : undefined;
  } catch {
    return undefined;
  }
}

/** Tenths of a millisecond are plenty for a label. */
function roundMs(ms: number): number {
  return Math.round(ms * 10) / 10;
}

/** A REST failure as a verdict: what kind of "couldn't run" it is. Signed-out errors are not handled here. */
function outcomeOfRequestError(transport: ProbeTransport, err: unknown): ProbeOutcome {
  if (err instanceof ApiError) {
    if (err.code === CodeTransportDisabled) return { verdict: { transport, result: 'disabled' } };
    if (err.code === CodeRateLimited || err.status === 429) {
      return {
        verdict: { transport, result: 'failed', error: 'rate_limited' },
        ...(err.retryAfterSec !== undefined ? { retryAfterSec: err.retryAfterSec } : {}),
      };
    }
  }
  // bad_sdp, not_ready, 5xx, a proxy's error page, no network, the request timeout.
  return { verdict: { transport, result: 'failed', error: 'server' } };
}

/**
 * Runs one probe and always closes its PC. Resolves with a verdict for everything the test can report: ok, failed
 * (✗), disabled, or not tested (error "server" or "rate_limited").
 *
 * Rejects only when there is no result to show:
 * - the signal aborted (its reason);
 * - the server says the user is signed out (the ApiError, so the caller's 401 rule runs, 05 §6.2);
 * - this browser can't even make a data-channel offer (LocalError "webrtc_failed").
 */
export async function runProbe(
  platform: Platform,
  transport: ProbeTransport,
  { signal, request = defaultRequest }: ProbeOptions = {},
): Promise<ProbeOutcome> {
  throwIfAborted(signal);
  let pc: RTCPeerConnection | undefined;
  try {
    let dc: RTCDataChannel;
    let offer: string;
    try {
      pc = platform.createPeerConnection({ iceServers: [], bundlePolicy: 'max-bundle' });
      dc = pc.createDataChannel('probe');
      dc.binaryType = 'arraybuffer';
      await pc.setLocalDescription(await pc.createOffer());
      offer = pc.localDescription?.sdp ?? '';
    } catch (err) {
      throwIfAborted(signal);
      throw new LocalError('webrtc_failed', { cause: err });
    }
    throwIfAborted(signal);

    let reply: ConnTestResponse;
    try {
      reply = await requestWithTimeout(request, { transport, offer }, REQUEST_TIMEOUT_MS, signal);
    } catch (err) {
      throwIfAborted(signal);
      if (isSignedOutError(err)) throw err;
      return outcomeOfRequestError(transport, err);
    }
    throwIfAborted(signal);
    // The reply comes off the wire: a 2xx without the JSON body is as good as no reply.
    const body = reply as Partial<ConnTestResponse> | undefined;
    const answer = body?.answer;
    const server = body?.server;
    const withServer = (verdict: ProbeVerdict): ProbeOutcome => (server ? { verdict, server } : { verdict });
    if (typeof answer !== 'string' || answer === '') {
      return withServer({ transport, result: 'failed', error: 'server' });
    }
    try {
      await pc.setRemoteDescription({ type: 'answer', sdp: answer });
    } catch {
      throwIfAborted(signal);
      return withServer({ transport, result: 'failed', error: 'server' });
    }

    const connected = await waitConnected(pc, dc, CONNECT_TIMEOUT_MS, signal);
    if (connected === 'timeout') return withServer({ transport, result: 'failed', error: 'timeout' });
    if (connected === 'failed') return withServer({ transport, result: 'failed' });

    const echoes = await measureEchoes(dc, signal);
    const rttMs = echoes.length > 0 ? median(echoes) : await statsRttMs(pc);
    throwIfAborted(signal);
    return withServer({ transport, result: 'ok', ...(rttMs !== undefined ? { rttMs: roundMs(rttMs) } : {}) });
  } finally {
    pc?.close();
  }
}
