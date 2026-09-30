// The REST client (05 §6.2): api<T>(method, path, body?) over platform.apiFetch, and ApiError from the one REST
// error envelope {"error": {code, fields?, params?, retryAfter?, requestId?}} (03 §12.2; 04 §9.4 uses it too).
//
// Nothing outside platform/ builds /api URLs or reads cookies (05 §8): api() hands a path to the platform, which
// resolves it against its server origin and adds its credentials (browser: the session cookie; desktop, M2: a
// bearer token). Boot binds the platform once with configureApi().
//
// CSRF (03 §7.5): unsafe methods always send Content-Type: application/json and a JSON body ({} when empty). With
// Go's cross-origin protection that is the whole defence; no token or custom header is needed.
import { CodeInvalidToken, CodeRateLimited, CodeServerBusy, CodeUnauthenticated } from './api.gen';

export type HttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';

/** An API path as the platform takes it: "/api/v1/info". */
export type ApiPath = `/api/${string}`;

/** What api() needs from the platform. */
export interface ApiTransport {
  apiFetch(path: ApiPath, init?: RequestInit): Promise<Response>;
}

export interface ApiOptions {
  /** Aborts the request (TanStack Query passes its own). */
  signal?: AbortSignal;
}

/** The code of an ApiError built from a response without a valid envelope (the Host check's 421, a proxy page). */
export const CodeUnknown = 'unknown';

/**
 * A non-2xx REST response (05 §6.2). `code` is the envelope's error code, or "unknown" when the body isn't the
 * envelope. The UI shows `errors.<code>` and `fieldErrors.<field>.<code>` (05 §6.3); the server never sends English.
 */
export class ApiError extends Error {
  override readonly name = 'ApiError';
  /** The HTTP status. */
  readonly status: number;
  /** A 03 §12.2 code (api.gen.ts Code…), "unknown", or a newer code this build doesn't know. */
  readonly code: string;
  /** validation_failed: field JSON name → field code (03 §12.2 Field…). */
  declare readonly fields?: Readonly<Record<string, string>>;
  /** Machine-readable values: {limit}, {field}, {reason}, {transport} (api.gen.ts Param…). */
  declare readonly params?: Readonly<Record<string, unknown>>;
  /** Seconds to wait before retrying: the envelope's retryAfter, else the Retry-After header. */
  declare readonly retryAfterSec?: number;
  /** 04's request ID, set on 500 internal (shown as the error reference). */
  declare readonly requestId?: string;

  constructor(init: {
    status: number;
    code: string;
    fields?: Readonly<Record<string, string>>;
    params?: Readonly<Record<string, unknown>>;
    retryAfterSec?: number;
    requestId?: string;
  }) {
    super(`api: ${init.code} (HTTP ${String(init.status)})`);
    this.status = init.status;
    this.code = init.code;
    if (init.fields !== undefined) this.fields = init.fields;
    if (init.params !== undefined) this.params = init.params;
    if (init.retryAfterSec !== undefined) this.retryAfterSec = init.retryAfterSec;
    if (init.requestId !== undefined) this.requestId = init.requestId;
  }
}

/** The request never got a response: offline, DNS, TLS, a reset connection (shown as `errors.local.offline`). */
export class NetworkError extends Error {
  override readonly name = 'NetworkError';
  readonly code = 'offline';

  constructor(options?: ErrorOptions) {
    super('api: network error', options);
  }
}

let transport: ApiTransport | null = null;

/** Binds api() to the platform (boot step 5; tests bind a test platform). null unbinds. */
export function configureApi(t: ApiTransport | null): void {
  transport = t;
}

const JSON_TYPE = /^application\/(?:[\w.+-]+\+)?json\s*(?:;|$)/i;

function isJsonResponse(res: Response): boolean {
  return JSON_TYPE.test(res.headers.get('Content-Type') ?? '');
}

/**
 * Calls the API: GET without a body, every other method with Content-Type: application/json and a JSON body ({} when
 * body is undefined). Resolves with the parsed JSON, or undefined for 204 and for a 2xx without a JSON body (202 from
 * /push/test). Rejects with ApiError for a non-2xx response, NetworkError when there is no response, and the signal's
 * reason when aborted.
 */
export async function api<T>(method: HttpMethod, path: ApiPath, body?: unknown, opts: ApiOptions = {}): Promise<T> {
  if (!transport) throw new Error('api: no platform (call configureApi at boot)');
  const headers = new Headers({ Accept: 'application/json' });
  const init: RequestInit = { method, headers };
  if (opts.signal) init.signal = opts.signal;
  if (method === 'GET') {
    if (body !== undefined) throw new TypeError('api: a GET request has no body');
  } else {
    headers.set('Content-Type', 'application/json');
    init.body = JSON.stringify(body ?? {});
  }

  let res: Response;
  try {
    res = await transport.apiFetch(path, init);
  } catch (err) {
    if (opts.signal?.aborted) throw opts.signal.reason;
    throw new NetworkError({ cause: err });
  }
  if (!res.ok) throw await apiErrorFromResponse(res);
  if (res.status === 204 || !isJsonResponse(res)) return undefined as T;
  try {
    return (await res.json()) as T;
  } catch {
    if (opts.signal?.aborted) throw opts.signal.reason;
    // A 2xx with a broken JSON body: nothing the caller can use.
    throw new ApiError({ status: res.status, code: CodeUnknown });
  }
}

/** Reads a non-2xx response into an ApiError. Never throws. */
export async function apiErrorFromResponse(res: Response): Promise<ApiError> {
  let text = '';
  try {
    text = await res.text();
  } catch {
    // A body that fails to stream is treated as no body.
  }
  return parseApiError(res.status, res.headers, isJsonResponse(res) ? text : '');
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function stringRecord(v: unknown): Record<string, string> | undefined {
  if (!isRecord(v)) return undefined;
  const out: Record<string, string> = {};
  for (const [k, val] of Object.entries(v)) {
    if (typeof val === 'string') out[k] = val;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/**
 * Parses the Retry-After header: delay-seconds, or an HTTP date (then the seconds from now, at least 0). undefined
 * when absent or invalid.
 */
export function parseRetryAfter(value: string | null, now: number = Date.now()): number | undefined {
  if (value === null) return undefined;
  const v = value.trim();
  if (/^\d+$/.test(v)) return Number(v);
  // An HTTP date has a weekday and month name; Date.parse would also take "-1" or "2026" as dates.
  if (!/[a-z]/i.test(v)) return undefined;
  const date = Date.parse(v);
  if (Number.isNaN(date)) return undefined;
  return Math.max(0, Math.ceil((date - now) / 1000));
}

/**
 * Builds an ApiError from a status, the headers and the body text ('' when the body isn't JSON). The envelope's
 * `error` object is the only source of code, fields, params and requestId; a wrapper around it (the admin socket's
 * {"error", "message", "fix"}, 04 §12.1) parses the same. A body without a string `error.code` gives code "unknown".
 */
export function parseApiError(status: number, headers: Headers, bodyText: string): ApiError {
  let envelope: unknown;
  try {
    envelope = bodyText === '' ? undefined : JSON.parse(bodyText);
  } catch {
    envelope = undefined;
  }
  const headerRetry = parseRetryAfter(headers.get('Retry-After'));
  const e = isRecord(envelope) ? envelope['error'] : undefined;
  if (!isRecord(e) || typeof e['code'] !== 'string' || e['code'] === '') {
    return new ApiError({
      status,
      code: CodeUnknown,
      ...(headerRetry !== undefined ? { retryAfterSec: headerRetry } : {}),
    });
  }
  const fields = stringRecord(e['fields']);
  const params = isRecord(e['params']) ? e['params'] : undefined;
  const retryAfter =
    typeof e['retryAfter'] === 'number' && Number.isFinite(e['retryAfter']) && e['retryAfter'] >= 0
      ? e['retryAfter']
      : headerRetry;
  const requestId = typeof e['requestId'] === 'string' && e['requestId'] !== '' ? e['requestId'] : undefined;
  return new ApiError({
    status,
    code: e['code'],
    ...(fields ? { fields } : {}),
    ...(params ? { params } : {}),
    ...(retryAfter !== undefined ? { retryAfterSec: retryAfter } : {}),
    ...(requestId !== undefined ? { requestId } : {}),
  });
}

export function isApiError(err: unknown, code?: string): err is ApiError {
  return err instanceof ApiError && (code === undefined || err.code === code);
}

/**
 * A 401 that means "signed out": unauthenticated (and, from M2, a bearer's invalid_token). Other 401 codes
 * (invalid_credentials) are ordinary form errors (05 §6.2).
 */
export function isSignedOutError(err: unknown): boolean {
  return (
    err instanceof ApiError && err.status === 401 && (err.code === CodeUnauthenticated || err.code === CodeInvalidToken)
  );
}

/** Whether a query may retry after this error (05 §6.2): network errors, 5xx, server_busy and rate_limited. */
export function isRetryableError(err: unknown): boolean {
  if (err instanceof NetworkError) return true;
  if (!(err instanceof ApiError)) return false;
  return err.status >= 500 || err.code === CodeServerBusy || err.code === CodeRateLimited;
}
