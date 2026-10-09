// The system's "now playing" (05 §12.8): navigator.mediaSession.metadata is {title: "bo's window", artist:
// "<room>"} while a share is heard.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { i18n } from '../i18n';
import { attachMediaSession } from './mediaSession';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo } from './testing';

/** MediaMetadata as browsers have it, for jsdom: it keeps what it was made with. */
class FakeMetadata {
  readonly title: string;
  readonly artist: string;
  constructor(init: MediaMetadataInit = {}) {
    this.title = init.title ?? '';
    this.artist = init.artist ?? '';
  }
}
const Metadata = FakeMetadata as unknown as typeof MediaMetadata;

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2, { kind: 'tab', label: 'Movie night' });

let viewer: ViewerServices;
let session: { metadata: MediaMetadata | null };
let writes: number;
let detach: (() => void) | undefined;
const t = i18n.t.bind(i18n);

const attach = (artist: () => string = () => 'Lounge'): void => {
  detach = attachMediaSession(viewer, { t, artist, session, Metadata });
};
const playing = (): { title: string; artist: string } | null =>
  session.metadata ? { title: session.metadata.title, artist: session.metadata.artist } : null;

beforeEach(() => {
  viewer = createViewer();
  writes = 0;
  let metadata: MediaMetadata | null = null;
  session = {
    get metadata() {
      return metadata;
    },
    set metadata(next) {
      writes++;
      metadata = next;
    },
  };
});

afterEach(() => {
  detach?.();
  detach = undefined;
  viewer.dispose();
});

describe('attachMediaSession', () => {
  it('names the audible share and the room', () => {
    attach();
    expect(playing()).toBeNull();
    syncRoom(viewer, room(BEA), SELF);
    expect(playing()).toEqual({ title: "Bea's window", artist: 'Lounge' });
  });

  it('follows the sound: focus, the speaker button, and silence', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    attach();
    expect(playing()).toEqual({ title: 'Cy: Movie night', artist: 'Lounge' });

    viewer.store.getState().setAudible('s_bea');
    expect(playing()).toEqual({ title: "Bea's window", artist: 'Lounge' });
    viewer.store.getState().setAudible(null);
    expect(playing()).toBeNull();
    viewer.store.getState().focusShare('s_cy');
    expect(playing()).toEqual({ title: 'Cy: Movie night', artist: 'Lounge' });
  });

  it('clears when the share that was heard ends and nothing else is', () => {
    syncRoom(viewer, room(BEA), SELF);
    attach();
    syncRoom(viewer, room(), SELF);
    expect(playing()).toBeNull();
  });

  it('follows a new title of the same share (a label, a name that arrived)', () => {
    syncRoom(viewer, room(BEA), SELF);
    attach();
    syncRoom(viewer, room({ ...BEA, label: 'Holiday photos' }), SELF);
    expect(playing()).toEqual({ title: 'Bea: Holiday photos', artist: 'Lounge' });
  });

  it('writes only when what it says changes', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    attach();
    expect(writes).toBe(1);
    // New watchers, a volume change, a tile scrolled away: nothing to tell the system.
    syncRoom(viewer, room({ ...BEA, watchers: [{ userId: 'u_cy', video: 'low', audio: 'off' }] }, CY), SELF);
    viewer.store.getState().setVolume(0.4);
    viewer.store.getState().setVisible('s_bea', true);
    expect(writes).toBe(1);
  });

  it('reads the room’s name with every change', () => {
    let name = 'Lounge';
    syncRoom(viewer, room(BEA, CY), SELF);
    attach(() => name);
    name = 'Movie room';
    viewer.store.getState().setAudible('s_bea');
    expect(playing()).toEqual({ title: "Bea's window", artist: 'Movie room' });
  });

  it('clears the metadata and stops with the detach', () => {
    syncRoom(viewer, room(BEA), SELF);
    attach();
    detach?.();
    detach = undefined;
    expect(playing()).toBeNull();
    syncRoom(viewer, room(BEA, CY), SELF);
    expect(playing()).toBeNull();
  });

  it('does nothing in a browser without the Media Session API (jsdom has none)', () => {
    syncRoom(viewer, room(BEA), SELF);
    expect('mediaSession' in navigator).toBe(false);
    const off = attachMediaSession(viewer, { t, artist: () => 'Lounge' });
    off();
    // With a session but no MediaMetadata to make: nothing either.
    const none = attachMediaSession(viewer, { t, artist: () => 'Lounge', session });
    expect(writes).toBe(0);
    none();
    attachMediaSession(viewer, { t, artist: () => 'Lounge', session: null, Metadata })();
    expect(writes).toBe(0);
  });
});
