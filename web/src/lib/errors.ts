// Client-side error classes that are not protocol errors (05 §6.3). 01's ProtocolError (protocol/errors.ts) covers
// the WebSocket and its three client-local codes; protocol/rest.ts has ApiError and NetworkError for REST. What is
// left lives here: 05's own local codes, and the "not implemented" error of the interfaces that later slices fill.

/**
 * 05's client-local error codes: each has an `errors.local.<code>` text in en.json (05 §6.3). 01's local codes
 * (`connection_lost`, `request_timeout`, `not_ready`) are ProtocolErrors with `local: true` and use the same
 * `errors.local` namespace.
 */
export const LOCAL_ERROR_CODES = [
  // 01 §12.4 (ProtocolError.local)
  'connection_lost',
  'request_timeout',
  'not_ready',
  // 05
  'capture_failed',
  'h264_unavailable',
  'webrtc_failed',
  'offline',
] as const;

export type LocalErrorCode = (typeof LOCAL_ERROR_CODES)[number];

/** A failure the client detects itself: never on the wire, shown through `errors.local.<code>`. */
export class LocalError extends Error {
  override readonly name = 'LocalError';
  readonly code: LocalErrorCode;

  constructor(code: LocalErrorCode, options?: ErrorOptions) {
    super(`isshoni: ${code}`, options);
    this.code = code;
  }
}

/**
 * Thrown (or used to reject) by an interface member whose behaviour a later slice fills in ("interfaces first",
 * docs/m1/README.md §4). The message names the member and the slice, so a call that arrives too early is easy to
 * trace. It is a programming error, never shown to users as such: the UI falls back to `errors.unknown`.
 */
export class NotImplementedError extends Error {
  override readonly name = 'NotImplementedError';
  /** The interface member, e.g. "sharing.pick". */
  readonly member: string;
  /** The slice that implements it, e.g. "S35". */
  readonly slice: string;

  constructor(member: string, slice: string) {
    super(`${member} is not implemented yet (${slice})`);
    this.member = member;
    this.slice = slice;
  }
}

/** Whether a value is a DOMException (or an Error) with the given name, e.g. "AbortError" or "NotAllowedError". */
export function isErrorNamed(err: unknown, name: string): boolean {
  return err instanceof Error && err.name === name;
}
