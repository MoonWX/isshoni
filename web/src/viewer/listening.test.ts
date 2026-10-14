// "Now listening to bo" (05 §16.6): the polite live region follows the sound to a share that was there before, and
// leaves a new share's sound to its room.event.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { attachListeningAnnouncer } from './listening';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo } from './testing';

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2, { kind: 'tab' });
const DEE = shareInfo('s_dee', 'u_cy', 3, { kind: 'screen' });
/** Alex's own share from another device: watched like anyone's, but it is "your share". */
const OTHER_DEVICE = shareInfo('s_laptop', 'u_alex', 4, { connectionId: 'c_laptop' });

let viewer: ViewerServices;
let said: string[];
let off: () => void;

beforeEach(() => {
  viewer = createViewer();
  said = [];
  off = attachListeningAnnouncer(viewer.store, (text) => said.push(text));
});

afterEach(() => {
  off();
  viewer.dispose();
});

describe('attachListeningAnnouncer', () => {
  it('says whose sound plays when it moves to a share that was there, not for a new share', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    // Cy's tab is the newest and takes the stage and the sound: its share.started was announced.
    expect(viewer.store.getState().audibleShareId).toBe('s_cy');
    expect(said).toEqual([]);

    viewer.store.getState().focusShare('s_bea');
    expect(said).toEqual(['Now listening to Bea']);
    viewer.store.getState().toggleAudible('s_cy');
    expect(said).toEqual(['Now listening to Bea', 'Now listening to Cy']);

    // The share that was heard ends: the sound goes back to the stage's, which was there.
    syncRoom(viewer, room(BEA), SELF);
    expect(viewer.store.getState().audibleShareId).toBe('s_bea');
    expect(said.at(-1)).toBe('Now listening to Bea');
  });

  it('calls this user’s share from another device “your share”, and stops when detached', () => {
    syncRoom(viewer, room(BEA, OTHER_DEVICE), SELF);
    viewer.store.getState().toggleAudible('s_laptop');
    viewer.store.getState().toggleAudible('s_bea');
    expect(said).toEqual(['Now listening to your share', 'Now listening to Bea']);

    off();
    syncRoom(viewer, room(BEA, OTHER_DEVICE, DEE), SELF);
    viewer.store.getState().toggleAudible('s_laptop');
    expect(said).toHaveLength(2);
  });
});
