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
// - A re-published share (`replaces`, 01 §10.6) gets what the share it replaces had, when both are of the same user:
//   the stage, the way it got there (a pick stays a pick) and the sound. That share has usually ended by then: after
//   a server restart the first room.state lists no shares, and the re-published ones are `starting` (no tile) before
//   they are live. So the shares that ended are remembered with what they had (`ended`). A pick made in between
//   wins: it empties that memory. A replaced share that still has its tile also gives its place among the tiles.
// - A share of this user is never focused automatically and never made audible by focus: its own preview has no
//   sound, and hearing one's own share from another device would echo.
//
// Audio follows focus (05 §12.3): focusing a share makes it the audible one. The speaker button of another tile
// (S47) sets audibleShareId without moving the focus; that choice stays until the user focuses a tile, or its share
// ends. An automatic focus change moves the audio only when the audio was following the focus.

/** How many ended shares are remembered for a re-publish; the oldest are forgotten first. */
export const MAX_ENDED_SHARES = 32;

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

/** A share that ended, with what it had just before: a re-publish that names it gets that back (01 §10.6). */
export interface EndedShare {
  readonly id: string;
  readonly userId: string;
  /** How it was on the stage: by itself ('auto') or by a pick ('manual'); null when it was not on the stage. */
  readonly focus: FocusMode | null;
  /** Its sound was the one playing. */
  readonly audible: boolean;
}

export interface FocusState<S extends FocusShare = FocusShare> {
  /**
   * The shares that have a tile (live or stalled), in tile order: newest first, except that a share replacing
   * one that still has its tile takes that share's place.
   */
  readonly shares: readonly S[];
  readonly focusedShareId: string | null;
  readonly focusMode: FocusMode;
  /** The one share whose audio plays, or null. */
  readonly audibleShareId: string | null;
  /** A ?focus= share that isn't live yet. */
  readonly pendingFocusParam: string | null;
  /** The shares that ended, oldest first, at most MAX_ENDED_SHARES. A pick empties it. */
  readonly ended: readonly EndedShare[];
}

export type FocusEvent<S extends FocusShare = FocusShare> =
  /** A share is live or stalled: a new one, or new details of a known one (which keeps its place). */
  | { readonly type: 'shareLive'; readonly share: S }
  /**
   * Shares left room.state. Those that one snapshot drops come in one event: each is remembered as it was before
   * any of them went (after a server restart they all go at once).
   */
  | { readonly type: 'shareEnded'; readonly shareIds: readonly string[] }
  /**
   * A share that is live or stalled now and whose `replaces` names another share (01 §10.6), one that still has its
   * tile or one that ended. Sent before the shareEnded of the same snapshot, so a share that goes in that snapshot
   * still gives its place.
   */
  | { readonly type: 'shareReplaced'; readonly share: S; readonly replaces: string }
  /** The user picked a tile. */
  | { readonly type: 'userFocus'; readonly shareId: string }
  /** ?focus=<shareId>; null when the wait for it ended (05 §18: 5 s). */
  | { readonly type: 'focusParam'; readonly shareId: string | null };

export function initialFocusState<S extends FocusShare = FocusShare>(): FocusState<S> {
  return {
    shares: [],
    focusedShareId: null,
    focusMode: 'auto',
    audibleShareId: null,
    pendingFocusParam: null,
    ended: [],
  };
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

/**
 * Focuses a share because the user picked it. The pick also empties `ended`: a share that comes back later takes
 * neither the stage nor the sound from what the user chose. Returns state itself when the pick changes nothing.
 */
function pick<S extends FocusShare>(state: FocusState<S>, share: S): FocusState<S> {
  const audible = share.own ? state.audibleShareId : share.id;
  if (
    state.focusedShareId === share.id &&
    state.focusMode === 'manual' &&
    state.audibleShareId === audible &&
    state.pendingFocusParam === null &&
    state.ended.length === 0
  ) {
    return state;
  }
  return {
    ...state,
    focusedShareId: share.id,
    focusMode: 'manual',
    audibleShareId: audible,
    pendingFocusParam: null,
    ended: state.ended.length === 0 ? state.ended : [],
  };
}

/**
 * The speaker button chose what is heard (05 §12.3): no share that comes back takes the sound from that choice.
 * viewerStore.setAudible stores the result with the new audibleShareId.
 */
export function withoutRememberedSound(ended: readonly EndedShare[]): readonly EndedShare[] {
  return ended.some((e) => e.audible) ? ended.map((e) => (e.audible ? { ...e, audible: false } : e)) : ended;
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

function sharesEnded<S extends FocusShare>(state: FocusState<S>, shareIds: readonly string[]): FocusState<S> {
  const gone = state.shares.filter((s) => shareIds.includes(s.id));
  if (gone.length === 0) return state;
  const shares = state.shares.filter((s) => !shareIds.includes(s.id));
  // What each had before any of them went: the focus must not look as if it had passed from one to the next.
  const was = gone.map((s): EndedShare => ({
    id: s.id,
    userId: s.userId,
    focus: state.focusedShareId === s.id ? state.focusMode : null,
    audible: state.audibleShareId === s.id,
  }));
  const next = { ...state, shares, ended: [...state.ended, ...was].slice(-MAX_ENDED_SHARES) };
  if (was.some((e) => e.focus !== null)) {
    // The pick (if it was one) ends with its share: auto-focus resumes.
    const focused = autoFocusTarget(shares);
    return {
      ...next,
      focusedShareId: focused,
      focusMode: 'auto',
      audibleShareId: audioAfterAutoFocus(state, shares, focused),
    };
  }
  if (was.some((e) => e.audible)) {
    // The share heard through its speaker button ended: the audio goes back to the focused share.
    const focused = shares.find((s) => s.id === state.focusedShareId);
    return { ...next, audibleShareId: focused && !focused.own ? focused.id : null };
  }
  return next;
}

/**
 * A re-publish of a share that ended earlier (the server restarted, or its sharer was gone for a while). The user
 * chose nothing else since then: a pick empties `ended`, and the speaker button drops the remembered sound.
 * - The stage: the share gets it back when it had it, as a pick if it was one, unless a pick that came back before
 *   it holds the stage. Like any share it takes an empty stage. It does not take the stage for being the newest:
 *   it is not a share that just started.
 * - The sound: the share gets it back with the stage, or alone when it had the sound without the stage (the
 *   speaker button). A stage it had without the sound moves the sound only when nothing is heard.
 * Its place among the tiles is the one of its age: the old tile is gone.
 */
function shareReturned<S extends FocusShare>(state: FocusState<S>, share: S, was: EndedShare): FocusState<S> {
  const next = { ...state, shares: insertByAge(state.shares, share), ended: state.ended.filter((e) => e !== was) };
  if (state.pendingFocusParam === share.id) return pick(next, share);
  const auto = state.focusMode === 'auto';
  const stage = auto && (was.focus !== null || (state.focusedShareId === null && !share.own));
  const sound = was.audible ? stage || was.focus === null : stage && state.audibleShareId === null && !share.own;
  if (!stage && !sound) return next;
  return {
    ...next,
    focusedShareId: stage ? share.id : state.focusedShareId,
    focusMode: stage && was.focus !== null ? was.focus : state.focusMode,
    audibleShareId: sound ? share.id : state.audibleShareId,
  };
}

function shareReplaced<S extends FocusShare>(state: FocusState<S>, share: S, replaces: string): FocusState<S> {
  if (state.shares.some((s) => s.id === share.id)) return shareLive(state, share);
  // Only a share this page saw, from the same user, hands anything over (01 §10.6: the server can't verify old ids
  // after a restart, so the client checks). Anything else is just a new share.
  const at = state.shares.findIndex((s) => s.id === replaces);
  const old = state.shares[at];
  if (old !== undefined) {
    if (old.userId !== share.userId) return shareLive(state, share);
    // The replaced share still has its tile: the new one goes in its place, with the stage and the sound it has.
    // The old tile goes when room.state drops that share: in this snapshot as a rule, later when the server still
    // holds the sharer's old connection.
    const shares = [...state.shares];
    shares.splice(at, 0, share);
    const next = {
      ...state,
      shares,
      focusedShareId: state.focusedShareId === replaces ? share.id : state.focusedShareId,
      audibleShareId: state.audibleShareId === replaces ? share.id : state.audibleShareId,
    };
    return state.pendingFocusParam === share.id ? pick(next, share) : next;
  }
  const was = state.ended.find((e) => e.id === replaces);
  if (was === undefined || was.userId !== share.userId) return shareLive(state, share);
  return shareReturned(state, share, was);
}

export function focusReducer<S extends FocusShare>(state: FocusState<S>, event: FocusEvent<S>): FocusState<S> {
  switch (event.type) {
    case 'shareLive':
      return shareLive(state, event.share);
    case 'shareEnded':
      return sharesEnded(state, event.shareIds);
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
