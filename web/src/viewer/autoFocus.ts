// Focus (05 §12.2): which share is on the stage and which one is heard. A pure reducer (state, event) → state, so
// every rule is unit-tested without a browser; viewerStore holds its state and feeds it room.state changes.
//
// The rules (owner decision, 05 §24.1):
// - While focusMode is 'auto', the stage shows the newest share of another person.
// - A click, tap, Enter/Space or number key on a tile (userFocus) focuses it and sets focusMode 'manual'. From then
//   on a newly started share does NOT take the stage or the sound: the room shows a "bo started sharing [Watch]"
//   toast instead. The pick holds until that share ends; then auto-focus resumes with the newest live share.
// - ?focus=<shareId> (push links) counts as a pick once that share is live (focusParam; the 5 s wait is the
//   caller's timer, which ends it with shareId null).
// - A share that replaces another of the same user (a re-publish after a restart, 01 §10.6) takes over its tile
//   position, the focus and the audio.
// - A share of this user is never focused automatically and never made audible by focus: its own preview has no
//   sound, and hearing one's own share from another device would echo.
//
// Audio follows focus (05 §12.3): focusing a share makes it the audible one. The speaker button of another tile
// (S47) sets audibleShareId without moving the focus; that choice stays until the user focuses a tile, or its share
// ends. An automatic focus change moves the audio only when the audio was following the focus.

/** What the reducer needs to know about a share. viewerStore's ViewerShare extends it. */
export interface FocusShare {
  readonly id: string;
  readonly userId: string;
  /** ShareInfo.startedAt (RFC 3339 UTC): the newest share is the auto-focus target. */
  readonly startedAt: string;
  /** A share of this user, from this page or another of their devices. */
  readonly own: boolean;
}

export type FocusMode = 'auto' | 'manual';

export interface FocusState<S extends FocusShare = FocusShare> {
  /**
   * The shares that have a tile (live or stalled), in tile order: newest first, except that a share replacing
   * another keeps the replaced share's place.
   */
  readonly shares: readonly S[];
  readonly focusedShareId: string | null;
  readonly focusMode: FocusMode;
  /** The one share whose audio plays, or null. */
  readonly audibleShareId: string | null;
  /** A ?focus= share that isn't live yet. */
  readonly pendingFocusParam: string | null;
}

export type FocusEvent<S extends FocusShare = FocusShare> =
  /** A share is live or stalled: a new one, or new details of a known one (which keeps its place). */
  | { readonly type: 'shareLive'; readonly share: S }
  /** A share left room.state. */
  | { readonly type: 'shareEnded'; readonly shareId: string }
  /** A new share whose `replaces` names another share (01 §10.6). */
  | { readonly type: 'shareReplaced'; readonly share: S; readonly replaces: string }
  /** The user picked a tile. */
  | { readonly type: 'userFocus'; readonly shareId: string }
  /** ?focus=<shareId>; null when the wait for it ended (05 §18: 5 s). */
  | { readonly type: 'focusParam'; readonly shareId: string | null };

export function initialFocusState<S extends FocusShare = FocusShare>(): FocusState<S> {
  return { shares: [], focusedShareId: null, focusMode: 'auto', audibleShareId: null, pendingFocusParam: null };
}

function startedMs(share: FocusShare): number {
  const ms = Date.parse(share.startedAt);
  return Number.isNaN(ms) ? 0 : ms;
}

/** Newest first; shares that started in the same millisecond are ordered by id, so the order is stable. */
function isNewer(a: FocusShare, b: FocusShare): boolean {
  const d = startedMs(a) - startedMs(b);
  return d !== 0 ? d > 0 : a.id < b.id;
}

function insertByAge<S extends FocusShare>(shares: readonly S[], share: S): S[] {
  const at = shares.findIndex((s) => isNewer(share, s));
  const out = [...shares];
  out.splice(at < 0 ? out.length : at, 0, share);
  return out;
}

/** The share auto-focus shows: the newest one of another person. */
export function autoFocusTarget(shares: readonly FocusShare[]): string | null {
  let best: FocusShare | null = null;
  for (const s of shares) {
    if (!s.own && (best === null || isNewer(s, best))) best = s;
  }
  return best?.id ?? null;
}

/** The audible share after the focus moved from one share to another by itself (not by a pick). */
function audioAfterAutoFocus<S extends FocusShare>(state: FocusState<S>, shares: readonly S[], focused: string | null) {
  const audible = state.audibleShareId;
  const followsFocus = audible === null || audible === state.focusedShareId || !shares.some((s) => s.id === audible);
  return followsFocus ? focused : audible;
}

/** Focuses a share because the user picked it. Returns state itself when the pick changes nothing. */
function pick<S extends FocusShare>(state: FocusState<S>, share: S): FocusState<S> {
  const audible = share.own ? state.audibleShareId : share.id;
  if (
    state.focusedShareId === share.id &&
    state.focusMode === 'manual' &&
    state.audibleShareId === audible &&
    state.pendingFocusParam === null
  ) {
    return state;
  }
  return { ...state, focusedShareId: share.id, focusMode: 'manual', audibleShareId: audible, pendingFocusParam: null };
}

function shareLive<S extends FocusShare>(state: FocusState<S>, share: S): FocusState<S> {
  const at = state.shares.findIndex((s) => s.id === share.id);
  if (at >= 0) {
    // Known: new details (status, watchers, …). Its place, the focus and the audio stay.
    if (state.shares[at] === share) return state;
    const shares = [...state.shares];
    shares[at] = share;
    return { ...state, shares };
  }
  const shares = insertByAge(state.shares, share);
  const next = { ...state, shares };
  if (state.pendingFocusParam === share.id) return pick(next, share);
  if (state.focusMode === 'manual') return next;
  const focused = autoFocusTarget(shares);
  if (focused === state.focusedShareId) return next;
  return { ...next, focusedShareId: focused, audibleShareId: audioAfterAutoFocus(state, shares, focused) };
}

function shareEnded<S extends FocusShare>(state: FocusState<S>, shareId: string): FocusState<S> {
  if (!state.shares.some((s) => s.id === shareId)) return state;
  const shares = state.shares.filter((s) => s.id !== shareId);
  if (state.focusedShareId === shareId) {
    // The pick (if it was one) ends with its share: auto-focus resumes.
    const focused = autoFocusTarget(shares);
    return {
      ...state,
      shares,
      focusedShareId: focused,
      focusMode: 'auto',
      audibleShareId: audioAfterAutoFocus(state, shares, focused),
    };
  }
  if (state.audibleShareId === shareId) {
    // The share heard through its speaker button ended: the audio goes back to the focused share.
    const focused = shares.find((s) => s.id === state.focusedShareId);
    return { ...state, shares, audibleShareId: focused && !focused.own ? focused.id : null };
  }
  return { ...state, shares };
}

function shareReplaced<S extends FocusShare>(state: FocusState<S>, share: S, replaces: string): FocusState<S> {
  const at = state.shares.findIndex((s) => s.id === replaces);
  const old = state.shares[at];
  // Only a share this page saw, from the same user, hands over its place (01 §10.6: the server can't verify old
  // ids after a restart, so the client checks). Anything else is just a new share.
  if (old === undefined || old.userId !== share.userId || state.shares.some((s) => s.id === share.id)) {
    return shareLive(state, share);
  }
  const shares = [...state.shares];
  shares[at] = share;
  const next = {
    ...state,
    shares,
    focusedShareId: state.focusedShareId === replaces ? share.id : state.focusedShareId,
    audibleShareId: state.audibleShareId === replaces ? share.id : state.audibleShareId,
  };
  return state.pendingFocusParam === share.id ? pick(next, share) : next;
}

export function focusReducer<S extends FocusShare>(state: FocusState<S>, event: FocusEvent<S>): FocusState<S> {
  switch (event.type) {
    case 'shareLive':
      return shareLive(state, event.share);
    case 'shareEnded':
      return shareEnded(state, event.shareId);
    case 'shareReplaced':
      return shareReplaced(state, event.share, event.replaces);
    case 'userFocus': {
      const share = state.shares.find((s) => s.id === event.shareId);
      // Picking the focused tile again still brings the audio back to it (05 §12.3).
      return share ? pick(state, share) : state;
    }
    case 'focusParam': {
      if (event.shareId === null) {
        return state.pendingFocusParam === null ? state : { ...state, pendingFocusParam: null };
      }
      const share = state.shares.find((s) => s.id === event.shareId);
      if (share) return pick(state, share);
      return state.pendingFocusParam === event.shareId ? state : { ...state, pendingFocusParam: event.shareId };
    }
  }
}
