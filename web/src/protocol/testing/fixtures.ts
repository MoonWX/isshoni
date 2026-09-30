// Wire payloads for tests (01 §8): a welcome and errors with the catalog's scope and retryable (01 §12.1), so a test
// states only what it is about.
import type { Error as WireError, ErrorCode, ErrorScope, Limits, Welcome } from '../types.gen';
import {
  ErrorCodeHelloTimeout,
  ErrorCodeIdleTimeout,
  ErrorCodeInternal,
  ErrorCodeRateLimited,
  ErrorCodeRoomFull,
  ErrorCodeServerShutdown,
  ErrorCodeSlowConnection,
  Version,
} from '../types.gen';

/** welcome.limits as the M1 server sends them (01 §8.2). */
export const testLimits: Limits = {
  maxMessageBytes: 65536,
  maxSdpBytes: 262144,
  maxSharesPerUser: 4,
  messagesPerSecond: 20,
  messageBurst: 100,
  pingIntervalMs: 15000,
  idleTimeoutMs: 45000,
  graceMs: 30000,
};

/**
 * A welcome like 01 §8.2's example. serverVersion is a dev build, so it is never stale whatever the test build's
 * version is.
 */
export function makeWelcome(overrides: Partial<Welcome> = {}): Welcome {
  return {
    protocol: Version,
    serverVersion: '0.0.0-dev',
    minClientVersion: '',
    features: [],
    limits: { ...testLimits },
    iceServers: [],
    connectionId: 'c_k3v9q2m7xw4pa8d1',
    resumeToken: 'r1.test-token',
    resumed: false,
    defaultRoomId: 'lounge',
    user: { id: 'k3m9p2qxw7ht', name: 'Alex' },
    serverTime: '2026-10-12T19:04:05.123Z',
    ...overrides,
  };
}

/** The codes that are retryable in 01 §12.1 (kicked is retryable only in scope request; pass it explicitly). */
const retryableCodes: ReadonlySet<string> = new Set<ErrorCode>([
  ErrorCodeHelloTimeout,
  ErrorCodeIdleTimeout,
  ErrorCodeRateLimited,
  ErrorCodeSlowConnection,
  ErrorCodeServerShutdown,
  ErrorCodeInternal,
  ErrorCodeRoomFull,
]);

/**
 * An error payload with code and scope; retryable follows 01 §12.1 unless extra sets it. code is a string so tests
 * can send codes this build doesn't know.
 */
export function makeError(
  code: ErrorCode | (string & Record<never, never>),
  scope: ErrorScope | (string & Record<never, never>),
  extra: Partial<WireError> = {},
): WireError {
  return { code, scope, retryable: retryableCodes.has(code), ...extra } as WireError;
}
