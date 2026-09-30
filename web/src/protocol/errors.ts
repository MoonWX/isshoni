// ProtocolError (docs/m1/01-protocol.md §8.13, §12, §16): the one error type of the signaling layer. It carries a
// wire error{code, scope, retryable, params} as the server sent it, a client-local error of 01 §12.4, or the error a
// close code stands for when the server closed without one (01 §12.2). SignalClient.request() rejects with it, and
// onState() reports it with the backoff and stopped states. It never carries English for users: 05 maps code to
// errors.<code> (errors.local.<code> when local is set) in en.json.
import {
  CloseCodeForbidden,
  CloseCodeProtocolViolation,
  CloseCodeRateLimited,
  CloseCodeReplaced,
  CloseCodeUnauthenticated,
  CloseCodeUnsupportedData,
  CloseCodeVersionUnsupported,
  ErrorCodeBadMessage,
  ErrorCodeForbidden,
  ErrorCodeProtocolUnsupported,
  ErrorCodeRateLimited,
  ErrorCodeReplaced,
  ErrorCodeUnauthenticated,
  ErrorScopeConnection,
  ErrorScopeRequest,
  ErrorScopeSession,
  type CloseCode,
  type ErrorCode,
  type ErrorScope,
  type PCKind,
} from './types.gen';

/** The connection went away before the reply (01 §12.4). Pending requests reject with it on every drop. */
export const LocalErrorCodeConnectionLost = 'connection_lost';
/** No reply within the request timeout (10 s by default, 01 §13). */
export const LocalErrorCodeRequestTimeout = 'request_timeout';
/** A request made while the client is stopped (01 §12.4); before start() and after stop() too. */
export const LocalErrorCodeNotReady = 'not_ready';

/** The client-local error codes of 01 §12.4: made by the client, never on the wire. */
export type LocalErrorCode =
  typeof LocalErrorCodeConnectionLost | typeof LocalErrorCodeRequestTimeout | typeof LocalErrorCodeNotReady;

export const localErrorCodes: readonly LocalErrorCode[] = [
  LocalErrorCodeConnectionLost,
  LocalErrorCodeRequestTimeout,
  LocalErrorCodeNotReady,
];

/**
 * A code or scope that this build doesn't know: a newer server may send new ones (01 §8.13, §14.1). The intersection
 * keeps editor completion for the known values while any string is accepted.
 */
type Unknown = string & Record<never, never>;

/** What a ProtocolError is made of; see the fields of ProtocolError. */
export interface ProtocolErrorInit {
  code: ErrorCode | LocalErrorCode | Unknown;
  scope: ErrorScope | Unknown;
  retryable: boolean;
  params?: Readonly<Record<string, unknown>>;
  local?: boolean;
  retryAfterMs?: number;
  shareId?: string;
  roomId?: string;
  pc?: PCKind | Unknown;
  gen?: number;
  neg?: number;
  closeCode?: number;
}

/**
 * A signaling error. Callers switch on code for the codes they know and fall back to scope and retryable for the
 * rest (01 §12.3): request → reject (a generic message only for user-initiated actions); subscription, share, pc →
 * log, the status messages drive the UI; room → leave the room UI; connection → retryable ? backoff : stop; session
 * → the login page. The SignalClient already applies the connection and session rows itself.
 */
export class ProtocolError extends Error {
  override readonly name = 'ProtocolError';
  /** The wire code (01 §12.1, possibly one this build doesn't know), or a LocalErrorCode when local is set. */
  readonly code: ErrorCode | LocalErrorCode | Unknown;
  /** What the error affects (01 §8.13); request for the local codes. */
  readonly scope: ErrorScope | Unknown;
  /** For scope connection: whether reconnecting helps. For a request: whether retrying it can succeed. */
  readonly retryable: boolean;
  /** Machine-readable values for i18n interpolation, never prose; {} when the error had none. */
  readonly params: Readonly<Record<string, unknown>>;
  /** True for the client-local codes of 01 §12.4 (connection_lost, request_timeout, not_ready). */
  readonly local: boolean;
  /** rate_limited (and later kicked): how long to wait before retrying, in ms. */
  readonly retryAfterMs: number | undefined;
  readonly shareId: string | undefined;
  readonly roomId: string | undefined;
  /** Scope pc: the PeerConnection, gen and neg of the negotiation that failed. */
  readonly pc: PCKind | Unknown | undefined;
  readonly gen: number | undefined;
  readonly neg: number | undefined;
  /**
   * Set when the error stands for a WebSocket close code that came without a preceding error (01 §12.2; see
   * fromCloseCode). Wire errors and local errors leave it undefined.
   */
  readonly closeCode: number | undefined;

  constructor(init: ProtocolErrorInit) {
    super(`signaling error ${init.code} (scope ${init.scope})`);
    this.code = init.code;
    this.scope = init.scope;
    this.retryable = init.retryable;
    this.params = init.params ?? {};
    this.local = init.local ?? false;
    this.retryAfterMs = init.retryAfterMs;
    this.shareId = init.shareId;
    this.roomId = init.roomId;
    this.pc = init.pc;
    this.gen = init.gen;
    this.neg = init.neg;
    this.closeCode = init.closeCode;
  }

  /**
   * Builds a ProtocolError from the data of an error message. It tolerates a malformed payload (a server bug): a
   * missing code reads as internal, a missing scope as request, a missing retryable as false, and fields of the
   * wrong type are dropped. Unknown codes and scopes are kept as they are.
   */
  static fromWire(data: unknown): ProtocolError {
    const d: Readonly<Record<string, unknown>> = isRecord(data) ? data : {};
    return new ProtocolError({
      code: nonEmptyString(d['code']) ?? 'internal',
      scope: nonEmptyString(d['scope']) ?? ErrorScopeRequest,
      retryable: d['retryable'] === true,
      params: isRecord(d['params']) ? d['params'] : undefined,
      retryAfterMs: count(d['retryAfterMs']),
      shareId: nonEmptyString(d['shareId']),
      roomId: nonEmptyString(d['roomId']),
      pc: nonEmptyString(d['pc']),
      gen: count(d['gen']),
      neg: count(d['neg']),
    });
  }

  /** A client-local error (01 §12.4), always with scope request. connection_lost and request_timeout are retryable. */
  static local(code: LocalErrorCode): ProtocolError {
    return new ProtocolError({
      code,
      scope: ErrorScopeRequest,
      retryable: code !== LocalErrorCodeNotReady,
      local: true,
    });
  }

  /**
   * The error a close code stands for when the server closed without a preceding error (01 §12.2, the column "Client
   * without a preceding error"), or undefined when the client just backs off (1000, 1001, 1006, 1009, 1011, 1012,
   * 4408, 4503 and every code this build doesn't know; SignalClient also logs a 1009). The code is chosen so that
   * 05's handling by code gives the table's client action:
   * - 4401 → unauthenticated (scope session): the login page;
   * - 4409 → replaced: stop silently;
   * - 4426 → protocol_unsupported: the update screen (params are empty: the server's versions are unknown);
   * - 4429 → rate_limited (retryable): back off for at least 30 s;
   * - 1003, 4400 → bad_message, 4403 → forbidden: stop with "Something went wrong" and Reload (05 §7.1).
   */
  static fromCloseCode(closeCode: number): ProtocolError | undefined {
    const row = closeCodeErrors.get(closeCode);
    if (row === undefined) {
      return undefined;
    }
    return new ProtocolError({ code: row.code, scope: row.scope, retryable: row.retryable, closeCode });
  }
}

interface CloseCodeError {
  code: ErrorCode;
  scope: ErrorScope;
  retryable: boolean;
}

/** The rows of 01 §12.2 whose client action isn't a plain backoff. */
const closeCodeErrors: ReadonlyMap<number, CloseCodeError> = new Map<CloseCode, CloseCodeError>([
  [CloseCodeUnsupportedData, { code: ErrorCodeBadMessage, scope: ErrorScopeConnection, retryable: false }],
  [CloseCodeProtocolViolation, { code: ErrorCodeBadMessage, scope: ErrorScopeConnection, retryable: false }],
  [CloseCodeUnauthenticated, { code: ErrorCodeUnauthenticated, scope: ErrorScopeSession, retryable: false }],
  [CloseCodeForbidden, { code: ErrorCodeForbidden, scope: ErrorScopeConnection, retryable: false }],
  [CloseCodeReplaced, { code: ErrorCodeReplaced, scope: ErrorScopeConnection, retryable: false }],
  [CloseCodeVersionUnsupported, { code: ErrorCodeProtocolUnsupported, scope: ErrorScopeConnection, retryable: false }],
  [CloseCodeRateLimited, { code: ErrorCodeRateLimited, scope: ErrorScopeConnection, retryable: true }],
]);

/** Reports whether e is a ProtocolError. */
export function isProtocolError(e: unknown): e is ProtocolError {
  return e instanceof ProtocolError;
}

function isRecord(v: unknown): v is Readonly<Record<string, unknown>> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function nonEmptyString(v: unknown): string | undefined {
  return typeof v === 'string' && v !== '' ? v : undefined;
}

/** A finite, non-negative number, else undefined. */
function count(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : undefined;
}
