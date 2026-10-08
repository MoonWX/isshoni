// The media registry (05 §6.1): shareId → that share's received tracks. Media tracks are not kept in Zustand stores
// (they are live browser objects, not state to diff): SubscriberPC writes them here as the server's offers map them
// (05 §10.1), and tiles read them through useShareMedia(shareId) (viewer/context.ts), which re-renders one tile when
// its share's tracks change.
import { createEmitter } from '../lib/emitter';
import { TrackKindVideo, type TrackKind } from '../protocol/types.gen';

/** The tracks received for one share. A share has at most one video and one audio track (01 §9 rule 4). */
export interface ShareMedia {
  readonly video?: MediaStreamTrack;
  readonly audio?: MediaStreamTrack;
}

/** What get() returns for a share without tracks: one frozen object, so snapshots compare equal. */
export const NO_MEDIA: ShareMedia = Object.freeze({});

export interface MediaRegistry {
  /**
   * The share's tracks. The same object is returned until that share's tracks change (a stable snapshot for
   * useSyncExternalStore); NO_MEDIA when it has none.
   */
  get(shareId: string): ShareMedia;
  /**
   * Sets the share's track of one kind, replacing the previous one: the SFU reuses transceivers, so a share can get
   * a track that carried another share before (05 §10.1). Setting the track it already has changes nothing.
   */
  set(shareId: string, kind: TrackKind, track: MediaStreamTrack): void;
  /** Drops the share's track of one kind, or both when kind is omitted. */
  delete(shareId: string, kind?: TrackKind): void;
  /** Drops every share that is not in shareIds: shares that ended disappear from room.state (05 §10.1). */
  retain(shareIds: Iterable<string>): void;
  /** Drops everything (the sub PC was closed or replaced). */
  clear(): void;
  /** Calls fn whenever the share's tracks change. Returns the function that stops it. */
  subscribe(shareId: string, fn: () => void): () => void;
  /** The shares that have at least one track. */
  shareIds(): string[];
}

export function createMediaRegistry(): MediaRegistry {
  const media = new Map<string, ShareMedia>();
  // One event per shareId. A listener that throws doesn't stop the others (lib/emitter).
  const changes = createEmitter<Record<string, undefined>>();

  const put = (shareId: string, next: ShareMedia): void => {
    if (next.video === undefined && next.audio === undefined) media.delete(shareId);
    else media.set(shareId, Object.freeze(next));
    changes.emit(shareId, undefined);
  };

  const remove = (shareId: string, kind?: TrackKind): void => {
    const cur = media.get(shareId);
    if (!cur) return;
    if (kind === undefined) {
      put(shareId, {});
      return;
    }
    if (cur[kind] === undefined) return;
    const other = kind === TrackKindVideo ? cur.audio : cur.video;
    if (other === undefined) put(shareId, {});
    else put(shareId, kind === TrackKindVideo ? { audio: other } : { video: other });
  };

  return {
    get: (shareId) => media.get(shareId) ?? NO_MEDIA,
    set(shareId, kind, track) {
      const cur = media.get(shareId) ?? NO_MEDIA;
      if (cur[kind] === track) return;
      put(shareId, { ...cur, [kind]: track });
    },
    delete: remove,
    retain(shareIds) {
      const keep = new Set(shareIds);
      for (const id of [...media.keys()]) {
        if (!keep.has(id)) remove(id);
      }
    },
    clear() {
      for (const id of [...media.keys()]) remove(id);
    },
    subscribe: (shareId, fn) => changes.on(shareId, fn),
    shareIds: () => [...media.keys()],
  };
}
