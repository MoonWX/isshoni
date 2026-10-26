// The approval flow of /link (docs/m2/02-device-auth.md §5.3, §10.1) as a small state machine over two requests,
// both with the browser's cookie session (UserSession, §6.4):
//   POST /api/v1/device/lookup {userCode}                     → 200 DeviceLookup  → confirm
//   POST /api/v1/device/decide {userCode, approve, deviceName?} → 204             → linked | denied
// and their errors: 404 device_code_invalid → invalid (the code field again); 429 rate_limited → rate_limited;
// 401 → the session is gone (the page signs this tab out, and the login page brings the friend back with the code);
// 422 {deviceName} → under the name field; anything else → the lookup's error state with Try again, or for a
// decide, a message on the confirm card, whose Link button is the way to try again.
//
// No request ever approves on its own: decide runs only from the Link and Don't link buttons, and while one runs a
// second call sends nothing (a double click is one decide; the server would answer a repeat with 204 anyway).
import { useCallback, useEffect, useRef, useState } from 'react';

import {
  CodeDeviceCodeInvalid,
  CodeRateLimited,
  CodeValidationFailed,
  type DeviceDecideRequest,
  type DeviceLookup,
  type DeviceLookupRequest,
} from '../protocol/api.gen';
import { api, ApiError, CodeUnknown, isApiError, isSignedOutError } from '../protocol/rest';
import { formatCodeInput, normalizeUserCode } from './userCode';

/** What the page shows (02 §10.1's states). `code` is always the normalized user code. */
export type LinkState =
  /** No code, or a malformed one: the code field, prefilled with what the link had. */
  | { readonly kind: 'enter'; readonly draft: string }
  | { readonly kind: 'loading'; readonly code: string }
  | { readonly kind: 'confirm'; readonly code: string; readonly lookup: DeviceLookup }
  | { readonly kind: 'linked' }
  | { readonly kind: 'denied' }
  /** 404 device_code_invalid: wrong, used or expired. The code field again. */
  | { readonly kind: 'invalid' }
  /** 429: the device-lookup bucket (10, then 1 per minute) is empty. */
  | { readonly kind: 'rate_limited'; readonly minutes: number }
  /** The lookup couldn't finish (offline, a server error): the reason with Try again. */
  | { readonly kind: 'error'; readonly error: unknown };

export type Decision = 'approve' | 'deny';

export interface DeviceLink {
  readonly state: LinkState;
  /** The decide request that runs now: its button shows a spinner. */
  readonly deciding: Decision | null;
  /** Why the last decide failed while the confirm card stays (offline, a server error); null otherwise. */
  readonly decideError: unknown;
  /** The field code of a name the server refused (422 {deviceName}: required, too_long, invalid); null otherwise. */
  readonly nameError: string | null;
  /** Looks a normalized code up: the enter and invalid states' Continue. */
  readonly lookup: (code: string) => void;
  /** The confirm card's Link or Don't link; deviceName only when the friend changed the app's proposal. */
  readonly decide: (code: string, decision: Decision, deviceName?: string) => void;
  /** The error state's Try again: the lookup once more. */
  readonly retry: () => void;
}

/**
 * The first state for the page's ?code=: loading for a valid code (the lookup starts at once), else the code field
 * with what the link had in it, cleaned up as the field would show it.
 */
export function initialLinkState(rawCode: string | null): LinkState {
  const code = rawCode === null ? null : normalizeUserCode(rawCode);
  if (code !== null) return { kind: 'loading', code };
  return { kind: 'enter', draft: rawCode === null ? '' : formatCodeInput(rawCode) };
}

const LOOKUP_FIELDS = ['userCode', 'clientKind', 'os', 'deviceName', 'appVersion', 'requestIp', 'network'] as const;

/**
 * Whether a 2xx body is a DeviceLookup as far as the page reads it. A captive portal or a proxy can answer 200 with
 * a page of its own, and api() resolves undefined for a 2xx without JSON.
 */
function isLookup(body: unknown): body is DeviceLookup {
  if (typeof body !== 'object' || body === null) return false;
  const b = body as Record<string, unknown>;
  return LOOKUP_FIELDS.every((field) => typeof b[field] === 'string');
}

/** Whole minutes to wait for a 429, at least 1 (the bucket refills one token a minute). */
function minutesOf(err: ApiError): number {
  return Math.max(1, Math.ceil((err.retryAfterSec ?? 60) / 60));
}

/** The states both requests' errors lead to, or null for an error each handles its own way. */
function sharedOutcome(err: unknown): LinkState | null {
  if (isApiError(err, CodeDeviceCodeInvalid)) return { kind: 'invalid' };
  // The page checks the code before it sends it, so this is a code the server reads differently: it doesn't work.
  if (isApiError(err, CodeValidationFailed) && err.fields?.['userCode'] !== undefined) return { kind: 'invalid' };
  if (isApiError(err, CodeRateLimited)) return { kind: 'rate_limited', minutes: minutesOf(err) };
  return null;
}

/**
 * The flow for one visit of /link. `rawCode` is the page's ?code= as it arrived (read once); `onSignedOut(code)`
 * runs when a request answers 401, with the code the friend was looking at.
 */
export function useDeviceLink(rawCode: string | null, onSignedOut: (code: string) => void): DeviceLink {
  const [first] = useState(() => initialLinkState(rawCode));
  const [state, setState] = useState<LinkState>(first);
  const [deciding, setDeciding] = useState<Decision | null>(null);
  const [decideError, setDecideError] = useState<unknown>(null);
  const [nameError, setNameError] = useState<string | null>(null);

  /** The page is mounted: answers that arrive later are dropped. */
  const live = useRef(false);
  /** Counts lookups: only the newest one's answer is shown. */
  const generation = useRef(0);
  /** A decide is out (or has succeeded): no second one. */
  const decideOut = useRef(false);
  /** The code of the lookup that failed, for retry(). */
  const failedCode = useRef<string | null>(null);
  /** The first lookup was sent (once per mount, also when an effect runs twice). */
  const started = useRef(false);
  const signedOut = useRef(onSignedOut);
  useEffect(() => {
    signedOut.current = onSignedOut;
  });

  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);

  /** Sends a lookup and shows its answer, unless a newer lookup was sent meanwhile. */
  const send = useCallback((code: string) => {
    const gen = ++generation.current;
    const body: DeviceLookupRequest = { userCode: code };
    const current = (): boolean => live.current && gen === generation.current;
    api<unknown>('POST', '/api/v1/device/lookup', body).then(
      (answer) => {
        if (!current()) return;
        if (!isLookup(answer)) {
          // Not the server's answer: the lookup couldn't finish, which says nothing about the code.
          failedCode.current = code;
          setState({ kind: 'error', error: new ApiError({ status: 200, code: CodeUnknown }) });
          return;
        }
        decideOut.current = false;
        setDecideError(null);
        setNameError(null);
        setState({ kind: 'confirm', code, lookup: answer });
      },
      (err: unknown) => {
        if (!current()) return;
        if (isSignedOutError(err)) {
          signedOut.current(code);
          return;
        }
        const next = sharedOutcome(err);
        if (next === null) failedCode.current = code;
        setState(next ?? { kind: 'error', error: err });
      },
    );
  }, []);

  useEffect(() => {
    if (started.current) return;
    started.current = true;
    if (first.kind === 'loading') send(first.code);
  }, [first, send]);

  const lookup = useCallback(
    (code: string) => {
      setState({ kind: 'loading', code });
      send(code);
    },
    [send],
  );

  const retry = useCallback(() => {
    const code = failedCode.current;
    if (code !== null) lookup(code);
  }, [lookup]);

  const decide = useCallback((code: string, decision: Decision, deviceName?: string) => {
    if (decideOut.current) return;
    decideOut.current = true;
    setDeciding(decision);
    setDecideError(null);
    setNameError(null);
    const body: DeviceDecideRequest = {
      userCode: code,
      approve: decision === 'approve',
      ...(deviceName !== undefined ? { deviceName } : {}),
    };
    api<undefined>('POST', '/api/v1/device/decide', body).then(
      () => {
        if (!live.current) return;
        // decideOut stays set: the code is decided, and nothing on the page can send another.
        setDeciding(null);
        setState({ kind: decision === 'approve' ? 'linked' : 'denied' });
      },
      (err: unknown) => {
        if (!live.current) return;
        decideOut.current = false;
        setDeciding(null);
        if (isSignedOutError(err)) {
          signedOut.current(code);
          return;
        }
        const field = isApiError(err, CodeValidationFailed) ? err.fields?.['deviceName'] : undefined;
        if (field !== undefined) {
          setNameError(field);
          return;
        }
        const next = sharedOutcome(err);
        if (next !== null) {
          setState(next);
          return;
        }
        setDecideError(err);
      },
    );
  }, []);

  return { state, deciding, decideError, nameError, lookup, decide, retry };
}
