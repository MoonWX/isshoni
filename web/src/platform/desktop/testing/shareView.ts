// ShareView fixtures for tests (docs/m2/03 §6.3): the share controller's view as Share.Current() and the share.state
// event carry it, live by default, of a window whose picker name is "Minecraft".
import type { ShareView } from '../bridge';

export function shareView(over: Partial<ShareView> = {}): ShareView {
  return {
    state: 'live',
    shareId: 's_q7m2k9x4w3bd8c1a',
    connectionId: 'c_k3v9q2m7xw4pa8d1',
    roomId: 'lounge',
    kind: 'window',
    label: 'Minecraft',
    preset: 'game',
    withAudio: true,
    audioOn: true,
    border: false,
    pill: true,
    minimized: false,
    watching: 3,
    liveSince: '2026-10-21T12:00:00.000Z',
    hint: null,
    encoder: 'nvenc',
    hw: true,
    layers: [
      {
        rid: 'f',
        width: 1920,
        height: 1080,
        fps: 60,
        targetKbps: 8000,
        sentKbps: 7712,
        captureToSendMs: { p50: 14, p95: 22 },
      },
      {
        rid: 'q',
        width: 640,
        height: 360,
        fps: 15,
        targetKbps: 300,
        sentKbps: 296,
        captureToSendMs: { p50: 12, p95: 19 },
      },
    ],
    server: null,
    endReason: null,
    error: null,
    ...over,
  };
}

/** The same share while C2 starts it: no share id yet, nothing measured. */
export function startingView(over: Partial<ShareView> = {}): ShareView {
  return shareView({ state: 'starting', shareId: '', liveSince: null, watching: 0, encoder: '', hw: false, ...over });
}
