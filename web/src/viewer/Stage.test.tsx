// Stage (05 §12.1, §16.6): the focused share as a region named "Now watching: …", its state, who shares what and
// how many watch, and a toolbar for the controls later slices add.
import { act, render, screen, within } from '@testing-library/react';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  FakeMediaStream,
  FakeMediaStreamTrack,
  installFakeMedia,
  installFakeMediaElement,
  type FakeMediaElementControl,
} from '../test/fakeMedia';
import { ViewerContext } from './context';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { Stage } from './Stage';
import { room, SELF, shareInfo, status } from './testing';
import type { ViewerShare } from './viewerStore';

const track = (kind: 'audio' | 'video') => new FakeMediaStreamTrack(kind) as unknown as MediaStreamTrack;
const watcher = (userId: string) => ({ userId, video: 'high' as const, audio: 'on' as const });

let media: FakeMediaElementControl;
let viewer: ViewerServices;

beforeEach(() => {
  installFakeMedia();
  media = installFakeMediaElement();
  viewer = createViewer();
});

afterEach(() => {
  media.restore();
  vi.unstubAllGlobals();
});

function shares(...infos: Parameters<typeof room>): Record<string, ViewerShare> {
  syncRoom(viewer, room(...infos), SELF);
  return Object.fromEntries(viewer.store.getState().shares.map((s) => [s.id, s]));
}

const inViewer = (ui: ReactNode) => <ViewerContext.Provider value={viewer}>{ui}</ViewerContext.Provider>;

const video = (): HTMLVideoElement => {
  const el = document.querySelector('video');
  if (!el) throw new Error('no <video>');
  return el;
};

describe('Stage', () => {
  it('shows the empty state without a share', () => {
    render(inViewer(<Stage share={null} empty={<p>Nobody is sharing yet.</p>} />));
    const stage = screen.getByRole('region', { name: 'Stage' });
    expect(within(stage).getByText('Nobody is sharing yet.')).toBeInTheDocument();
    expect(document.querySelector('video')).toBeNull();
    expect(screen.queryByRole('toolbar')).not.toBeInTheDocument();
  });

  it('is a region named after the share, with its video, sharer, kind and watchers', () => {
    const { s_a: share } = shares(shareInfo('s_a', 'u_bea', 1, { watchers: [watcher('u_alex'), watcher('u_cy')] }));
    const v = track('video');
    viewer.registry.set('s_a', 'video', v);
    viewer.registry.set('s_a', 'audio', track('audio'));
    render(inViewer(<Stage share={share ?? null} />));

    const stage = screen.getByRole('region', { name: "Now watching: Bea's window" });
    expect(within(stage).getByText('Bea')).toBeInTheDocument();
    expect(within(stage).getByText('Window')).toBeInTheDocument();
    expect(within(stage).getByText('2 watching')).toBeInTheDocument();
    // Muted like every video (the sound comes from audioOut's one <audio>), and only the video track.
    expect(video().muted).toBe(true);
    expect((video().srcObject as unknown as FakeMediaStream).getTracks()).toEqual([v]);
    expect(within(stage).getByRole('status')).toBeEmptyDOMElement();
  });

  it('announces the share’s state in a live region', () => {
    const { s_a: share } = shares(shareInfo('s_a', 'u_bea', 1));
    render(inViewer(<Stage share={share ?? null} />));
    const live = screen.getByRole('status');
    expect(live).toHaveTextContent('Connecting…');

    act(() => {
      viewer.registry.set('s_a', 'video', track('video'));
    });
    expect(live).toBeEmptyDOMElement();
    act(() => {
      viewer.store.getState().applyStatus([status('s_a', { video: 'low', reason: 'bandwidth' })]);
    });
    expect(live).toHaveTextContent('Lower quality (your connection)');
    act(() => {
      viewer.store.getState().applyStatus([status('s_a', { video: 'off', reason: 'codec' })]);
    });
    expect(live).toHaveTextContent(/still getting its video decoder/);
  });

  it('says "Connection unstable" over a stalled share', () => {
    const { s_a: share } = shares(shareInfo('s_a', 'u_bea', 1, { status: 'stalled' }));
    viewer.registry.set('s_a', 'video', track('video'));
    render(inViewer(<Stage share={share ?? null} />));
    expect(screen.getByRole('status')).toHaveTextContent('Connection unstable');
    expect(video().srcObject).not.toBeNull();
  });

  it('enlarges the local preview of a share this page publishes', () => {
    const { s_mine: share } = shares(shareInfo('s_mine', 'u_alex', 1, { connectionId: 'c_me', kind: 'screen' }));
    const preview = new FakeMediaStream([new FakeMediaStreamTrack('video')]) as unknown as MediaStream;
    render(inViewer(<Stage share={share ?? null} preview={preview} />));
    expect(screen.getByRole('region', { name: 'Your preview: Your screen' })).toBeInTheDocument();
    expect(video().srcObject).toBe(preview);
    expect(screen.getByText('You')).toBeInTheDocument();
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  it('has a toolbar only when it has controls', () => {
    const { s_a: share } = shares(shareInfo('s_a', 'u_bea', 1));
    const { rerender } = render(inViewer(<Stage share={share ?? null} />));
    expect(screen.queryByRole('toolbar')).not.toBeInTheDocument();
    rerender(inViewer(<Stage share={share ?? null} controls={<button type="button">Fullscreen</button>} />));
    const toolbar = screen.getByRole('toolbar', { name: 'Stage controls' });
    expect(within(toolbar).getByRole('button', { name: 'Fullscreen' })).toBeInTheDocument();
  });

  it('gives each share its own video element, so no frame of the previous one is left', () => {
    const all = shares(shareInfo('s_a', 'u_bea', 1), shareInfo('s_b', 'u_cy', 2, { kind: 'tab' }));
    const a = track('video');
    const b = track('video');
    viewer.registry.set('s_a', 'video', a);
    viewer.registry.set('s_b', 'video', b);
    const { rerender } = render(inViewer(<Stage share={all['s_a'] ?? null} />));
    const first = video();
    rerender(inViewer(<Stage share={all['s_b'] ?? null} />));
    expect(screen.getByRole('region', { name: "Now watching: Cy's tab" })).toBeInTheDocument();
    expect(video()).not.toBe(first);
    expect(first.srcObject).toBeNull();
    expect((video().srcObject as unknown as FakeMediaStream).getTracks()).toEqual([b]);
  });
});
