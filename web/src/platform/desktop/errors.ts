// Bridge errors (docs/m2/03 §6.4): a failing bound method returns a Go error whose Error() is the JSON
// {"code":"…","params":{…}}; each transport turns its rejection into a BridgeError {code, params}. Codes are snake_case
// and stable (internal/desktop/bridge/codes, generated into codes.gen.ts); the SPA shows desktop.errors.<code>.

/** The reply wasn't the error JSON (a transport failure, a bug); also Go's own code for a bug. */
export const BridgeCodeInternal = 'internal';
/** A method whose slice hasn't landed yet (Go's code too, README §4 "Interfaces first"). */
export const BridgeCodeNotImplemented = 'not_implemented';

export class BridgeError extends Error {
  override readonly name = 'BridgeError';
  readonly code: string;
  readonly params: Readonly<Record<string, unknown>>;

  constructor(code: string, params: Readonly<Record<string, unknown>> = {}, options?: ErrorOptions) {
    super(`bridge: ${code}`, options);
    this.code = code;
    this.params = params;
  }
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** Reads {code, params} from a parsed reply; null when it isn't one. */
export function bridgeErrorFromJSON(v: unknown, cause?: unknown): BridgeError | null {
  if (!isRecord(v) || typeof v['code'] !== 'string' || v['code'] === '') return null;
  const params = isRecord(v['params']) ? v['params'] : {};
  return new BridgeError(v['code'], params, cause === undefined ? undefined : { cause });
}

/**
 * Turns any rejection of a bound call into a BridgeError: a BridgeError stays as it is; an Error whose message (or a
 * string that) is the error JSON becomes that error; anything else is `internal`, with the original as its cause.
 */
export function toBridgeError(err: unknown): BridgeError {
  if (err instanceof BridgeError) return err;
  const text = err instanceof Error ? err.message : typeof err === 'string' ? err : undefined;
  if (text !== undefined) {
    try {
      const parsed = bridgeErrorFromJSON(JSON.parse(text), err);
      if (parsed) return parsed;
    } catch {
      // not JSON: a transport failure
    }
  }
  return new BridgeError(BridgeCodeInternal, {}, { cause: err });
}

/** The bridge code of a rejection, or undefined when it isn't a BridgeError. */
export function bridgeErrorCode(err: unknown): string | undefined {
  return err instanceof BridgeError ? err.code : undefined;
}
