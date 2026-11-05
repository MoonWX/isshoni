// The endpoint mute (docs/m2/03 §12.1, 05 §11.2, §15): in the desktop app, endpoint loopback re-captures isshoni's
// own playback, so while a share is live and 05's FriendsHear.engine is `endpoint`, viewerStore.audioMutedForShare
// turns every subscription's audio off (layerPolicy's rule 8). It reverts when the engine leaves `endpoint` or the
// share ends; 05's viewer-muted banner explains it. In the browser there is no bridge and nothing to do.
//
// The room runtime attaches it once per page (rooms/runtime.ts), with the viewer's store, so the mute holds on every
// page of the app, not only while the room page's viewer is mounted (README §3 item 87's move). No React here.
import type { FriendsHear, NativeBridge } from '../platform/desktop/bridge';
import type { ViewerStore } from './viewerStore';

/** Whether a FriendsHear mutes the page's viewer. */
export function mutesViewer(fh: Pick<FriendsHear, 'live' | 'engine'>): boolean {
  return fh.live && fh.engine === 'endpoint';
}

/**
 * Follows 05's reports (the audio.report event, then a pull of Audio.FriendsHear) into the store's
 * audioMutedForShare. Returns the function that stops it; the mute ends with it.
 */
export function attachEndpointMute(store: ViewerStore, bridge: Pick<NativeBridge, 'on' | 'audio'>): () => void {
  let alive = true;
  let seq = -1;
  const take = (fh: unknown): void => {
    if (!alive || typeof fh !== 'object' || fh === null) return;
    const report = fh as FriendsHear;
    if (typeof report.seq !== 'number' || report.seq < seq) return;
    seq = report.seq;
    store.getState().setAudioMutedForShare(mutesViewer(report));
  };
  const off = bridge.on('audio.report', take);
  bridge.audio.friendsHear().then(take, () => undefined);
  return () => {
    alive = false;
    off();
    store.getState().setAudioMutedForShare(false);
  };
}
