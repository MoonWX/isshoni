// The debug overlay (05 §10.7, §16.6): Shift+D or ?debug=1 shows it in the layout, it says per tile what the page
// receives and what the server forwards, with the PCs above and what the page sends below, and it never shows an
// IP address, also when the browser's report has them.
import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { summarize, type PCReport, type StatsSample } from '../lib/stats/summarize';
import type { ServerStats } from '../protocol/types.gen';
import { FakeMediaStream, FakeMediaStreamTrack, installFakeMedia, installFakeMediaElement } from '../test/fakeMedia';
import { createTestServices, renderWithApp } from '../test/render';
import { DebugPanel } from './DebugOverlay';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo } from './testing';
import { ViewerLayout } from './ViewerLayout';

const MINE = shareInfo('s_mine', 'u_alex', 0, { connectionId: 'c_me', kind: 'screen' });
const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2, { kind: 'tab' });

/** IPv4 and IPv6 addresses, as an overlay must never show them. */
const IP = /\b\d{1,3}(?:\.\d{1,3}){3}\b|\b[0-9a-f]{1,4}(?::[0-9a-f]{0,4}){2,7}\b/i;

/**
 * The getStats() report of a sub PC that receives Bea's window and Cy's tab, at `at` ms with `bytes` received per
 * stream so far, with the candidates' addresses in it as a browser has them.
 */
function subReport(at: number, frames: number): PCReport {
  const report = new Map<string, Record<string, unknown>>([
    ['T1', { type: 'transport', selectedCandidatePairId: 'CP1' }],
    [
      'CP1',
      {
        type: 'candidate-pair',
        localCandidateId: 'L1',
        remoteCandidateId: 'R1',
        currentRoundTripTime: 0.012,
        availableOutgoingBitrate: 2_400_000,
      },
    ],
    ['L1', { type: 'local-candidate', candidateType: 'host', protocol: 'udp', address: '192.0.2.10', port: 50000 }],
    ['R1', { type: 'remote-candidate', candidateType: 'host', protocol: 'udp', ip: '198.51.100.7', port: 7882 }],
    ['C1', { type: 'codec', mimeType: 'video/H264', sdpFmtpLine: 'packetization-mode=1;profile-level-id=42e01f' }],
    [
      'V1',
      {
        type: 'inbound-rtp',
        kind: 'video',
        mid: '0',
        timestamp: at,
        bytesReceived: at * 300,
        packetsLost: 4,
        frameWidth: 1280,
        frameHeight: 720,
        framesPerSecond: 29.97,
        framesDecoded: frames,
        framesDropped: 2,
        keyFramesDecoded: 3,
        freezeCount: 1,
        totalFreezesDuration: 0.5,
        pliCount: 1,
        nackCount: 7,
        codecId: 'C1',
        decoderImplementation: 'ExternalDecoder',
        powerEfficientDecoder: true,
      },
    ],
    [
      'A1',
      {
        type: 'inbound-rtp',
        kind: 'audio',
        mid: '1',
        timestamp: at,
        bytesReceived: at * 4,
        packetsLost: 0,
        audioLevel: 0.21,
        concealedSamples: 48,
        totalSamplesReceived: 9_600,
      },
    ],
    ['V2', { type: 'inbound-rtp', kind: 'video', mid: '2', timestamp: at, bytesReceived: at * 50, packetsLost: 0 }],
  ]);
  const byMid: Record<string, string> = { '0': 's_bea', '1': 's_bea', '2': 's_cy' };
  return { pc: 'sub', gen: 1, state: 'connected', report, shareOf: (t) => (t.mid ? byMid[t.mid] : undefined) };
}

/** What the collector makes of two reports 2 s apart. */
function sampled(): StatsSample {
  const first = summarize([subReport(1_000, 30)], 1_000);
  return summarize([subReport(3_000, 90)], 3_000, first.counters).sample;
}

const SERVER: ServerStats = {
  downlinkEstimate: 24_000_000,
  subs: [
    { shareId: 's_bea', kind: 'video', layer: 'high', bitrate: 2_600_000, lossPct: 0.2, dropped: 0 },
    { shareId: 's_bea', kind: 'audio', bitrate: 32_000, lossPct: 0, dropped: 0 },
    { shareId: 's_cy', kind: 'video', layer: 'low', bitrate: 400_000, lossPct: 1.5, dropped: 3 },
  ],
  layers: [{ shareId: 's_mine', kind: 'video', rid: 'f', bitrate: 2_400_000, lossPct: 0 }],
};

let viewer: ViewerServices;

beforeEach(() => {
  installFakeMedia();
  viewer = createViewer();
});

afterEach(() => {
  viewer.dispose();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const overlay = () => screen.getByRole('complementary', { name: 'Stats for nerds' });
const section = (name: string | RegExp) => within(overlay()).getByRole('region', { name });
/** The rows of a section as "label: value". */
const rows = (el: HTMLElement): string[] =>
  [...el.querySelectorAll('dt')].map((dt) => `${dt.textContent}: ${dt.nextElementSibling?.textContent ?? ''}`);

describe('DebugPanel', () => {
  it('says it is open while it is mounted', () => {
    const { unmount } = render(<DebugPanel viewer={viewer} onClose={() => undefined} />);
    expect(viewer.debug.getState().open).toBe(true);
    expect(within(overlay()).getByText('Waiting for the first stats…')).toBeInTheDocument();
    unmount();
    expect(viewer.debug.getState().open).toBe(false);
  });

  it('shows the PCs, each tile with what the server forwards of it, the stage first, and what the page sends', () => {
    syncRoom(viewer, room(MINE, BEA, CY), SELF);
    viewer.store.getState().focusShare('s_bea');
    render(<DebugPanel viewer={viewer} onClose={() => undefined} />);
    act(() => {
      const sample = sampled();
      viewer.debug.getState().setSample({
        ...sample,
        outbound: [
          {
            shareId: 's_mine',
            kind: 'video',
            rid: 'f',
            kbps: 2_480.4,
            bytesSent: 1,
            frameWidth: 1920,
            frameHeight: 1080,
            framesPerSecond: 30,
            qualityLimitationReason: 'bandwidth',
            encoderImplementation: 'OpenH264',
            powerEfficientEncoder: false,
          },
          { shareId: 's_mine', kind: 'audio', kbps: 64, bytesSent: 1, qualityLimitationReason: 'none' },
        ],
      });
      viewer.debug.getState().setServer(SERVER);
    });

    expect(rows(section('Connections'))).toEqual([
      'sub PC 1: connected · host udp · RTT 12 ms · out 2,400 kbit/s',
      'Downlink (server): 24,000 kbit/s',
    ]);
    // The tiles in the layout's order, the stage first; this page's own share is under Sending.
    const names = within(overlay())
      .getAllByRole('heading', { level: 3 })
      .map((h) => h.textContent);
    expect(names).toEqual(['Connections', "Bea's window", "Cy's tab", 'Sending']);
    expect(rows(section("Bea's window"))).toEqual([
      'Video: 1280×720 · 30 fps · 2,400 kbit/s · h264/42e0',
      'Decoder: ExternalDecoder (hardware)',
      'Frames: decoded 90 · dropped 2 · key frames 3 · freezes 1 (0.5 s)',
      'Network: lost 4 · PLI 1 · NACK 7',
      'Audio: 32 kbit/s · level 0.21 · concealed 0.5 % · lost 0',
      'Server video: high · 2,600 kbit/s · loss 0.2 % · dropped 0',
      'Server audio: 32 kbit/s · loss 0 % · dropped 0',
    ]);
    expect(rows(section("Cy's tab"))).toEqual([
      'Video: 400 kbit/s',
      'Network: lost 0',
      'Server video: low · 400 kbit/s · loss 1.5 % · dropped 3',
    ]);
    expect(rows(section('Sending'))).toEqual([
      'video f: 1920×1080 · 30 fps · 2,480 kbit/s · limited by bandwidth · OpenH264 (software)',
      'audio: 64 kbit/s',
      'video f at the server: 2,400 kbit/s · loss 0 %',
    ]);
  });

  it('never shows an IP address, though the browser’s report has them', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    render(<DebugPanel viewer={viewer} onClose={() => undefined} />);
    act(() => {
      viewer.debug.getState().setSample(sampled());
      viewer.debug.getState().setServer(SERVER);
    });
    expect(overlay().textContent).not.toMatch(IP);
    expect(overlay().textContent).toContain('host udp');
  });

  it('says so for a tile with nothing received yet, and for a page without a PC', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    render(<DebugPanel viewer={viewer} onClose={() => undefined} />);
    act(() => {
      viewer.debug.getState().setSample({ at: 1, intervalMs: 0, pcs: [], shares: {}, outbound: [] });
    });
    expect(within(section('Connections')).getByText('No media connection yet.')).toBeInTheDocument();
    expect(within(section("Bea's window")).getByText('Nothing received for this tile yet.')).toBeInTheDocument();
    expect(within(overlay()).queryByRole('region', { name: 'Sending' })).not.toBeInTheDocument();
  });
});

describe('the overlay in the layout', () => {
  beforeEach(() => {
    installFakeMediaElement();
  });

  const layout = () => (
    <ViewerLayout
      viewer={viewer}
      localPreviews={{ s_mine: new FakeMediaStream([new FakeMediaStreamTrack('video')]) as unknown as MediaStream }}
    />
  );

  it('Shift+D shows it and hides it; its close button hides it too', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA), SELF);
    const services = createTestServices();
    renderWithApp(layout(), { services });
    expect(screen.queryByRole('complementary', { name: 'Stats for nerds' })).not.toBeInTheDocument();

    await user.keyboard('{Shift>}D{/Shift}');
    expect(overlay()).toBeInTheDocument();
    expect(viewer.debug.getState().open).toBe(true);
    await user.keyboard('{Shift>}D{/Shift}');
    expect(screen.queryByRole('complementary', { name: 'Stats for nerds' })).not.toBeInTheDocument();
    expect(viewer.debug.getState().open).toBe(false);

    await user.keyboard('{Shift>}D{/Shift}');
    await user.click(within(overlay()).getByRole('button', { name: 'Close the stats' }));
    expect(screen.queryByRole('complementary', { name: 'Stats for nerds' })).not.toBeInTheDocument();
    expect(services.prefs.getState().debug).toBe(false);
  });

  it('?debug=1 turns it on', () => {
    const before = globalThis.location.href;
    globalThis.history.replaceState(null, '', '/r/lounge?debug=1');
    try {
      syncRoom(viewer, room(BEA), SELF);
      const services = createTestServices();
      renderWithApp(layout(), { services });
      expect(services.prefs.getState().debug).toBe(true);
      expect(overlay()).toBeInTheDocument();
    } finally {
      globalThis.history.replaceState(null, '', before);
    }
  });

  it('is not there outside the app', () => {
    syncRoom(viewer, room(BEA), SELF);
    render(layout());
    expect(screen.queryByRole('complementary', { name: 'Stats for nerds' })).not.toBeInTheDocument();
  });
});
