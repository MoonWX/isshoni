// The connection banner (05 §7.1) on a real SignalClient against the fake signaling server: nothing for a blip,
// "Reconnecting…" after 2 s and recovery, "Can't reach the server" with its buttons after 30 s, "Server restarting…";
// what of it is a live region; and the hook's first value when the clock or the store moved before its effect ran.
import { act, fireEvent, render, renderHook, screen, within } from '@testing-library/react';
import { useLayoutEffect } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '../app/App';
import { BackoffMaxMs } from '../protocol/signal-client';
import { makeError } from '../protocol/testing';
import { createConnectionStore, RECONNECTING_AFTER_MS } from './connection';
import { ConnectionBanner, useConnectionBanner } from './ConnectionBanner';
import type { RoomRuntime } from './runtime';
import { createHarness, refuseConnections, type Harness } from './testing/harness';

let h: Harness;
let runtime: RoomRuntime;

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5); // no backoff jitter
  h = createHarness();
  runtime = h.runtime;
});

afterEach(() => {
  h.close();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

/** Advances the fake clock inside act(), so the banner's timers and store updates render. */
async function advance(ms = 0): Promise<void> {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

async function renderConnected(onTestConnection?: () => void): Promise<HTMLElement> {
  render(
    <AppProviders services={h.services}>
      <ConnectionBanner onTestConnection={onTestConnection} />
    </AppProviders>,
  );
  runtime.start();
  await advance();
  expect(runtime.signal.state).toBe('ready');
  return screen.getByTestId('connection-banner');
}

function drop(): void {
  act(() => {
    h.server.drop();
  });
}

/** The live region inside the banner: its text. */
function liveRegion(region: HTMLElement): HTMLElement {
  const live = region.querySelector<HTMLElement>('[aria-live]');
  if (live === null) throw new Error('the banner has no live region');
  return live;
}

/** No banner on screen: no text, no icon, no buttons, and nothing that styles the (still mounted) root as one. */
function expectNoBanner(region: HTMLElement): void {
  expect(region).not.toHaveAttribute('data-kind');
  expect(liveRegion(region)).toBeEmptyDOMElement();
  expect(region.children).toHaveLength(1);
}

describe('ConnectionBanner (05 §7.1)', () => {
  it('is an empty polite live region while the connection is fine', async () => {
    const region = await renderConnected();
    expect(liveRegion(region)).toHaveAttribute('aria-live', 'polite');
    expectNoBanner(region);
  });

  it('keeps one live region mounted, and each text appears inside it', async () => {
    const region = await renderConnected();
    const live = liveRegion(region);
    refuseConnections(h.server);
    drop();
    await advance(2_000);
    expect(live).toHaveTextContent('Reconnecting…');
    await advance(28_000);
    expect(live).toHaveTextContent("Can't reach the server. Retrying…");
    // The same element all along: a region that is mounted together with its text is not announced.
    expect(liveRegion(region)).toBe(live);
    expect(region.querySelectorAll('[aria-live]')).toHaveLength(1);
  });

  it('shows nothing for a blip, "Reconnecting…" after 2 s, and clears on recovery', async () => {
    const region = await renderConnected();
    const restore = refuseConnections(h.server);
    drop();
    expectNoBanner(region);
    await advance(1_999);
    expectNoBanner(region);
    await advance(1);
    expect(region).toHaveTextContent('Reconnecting…');
    expect(region).toHaveAttribute('data-kind', 'reconnecting');
    expect(within(region).queryByRole('button')).not.toBeInTheDocument();

    // It stays through the failed attempts in between.
    await advance(10_000);
    expect(region).toHaveTextContent('Reconnecting…');

    restore();
    await advance(BackoffMaxMs);
    expect(runtime.signal.state).toBe('ready');
    expectNoBanner(region);
  });

  it('a drop that recovers within 2 s never shows', async () => {
    const region = await renderConnected();
    drop();
    await advance(500);
    expect(runtime.signal.state).toBe('ready');
    await advance(2_000);
    expectNoBanner(region);
  });

  it('after 30 s: "Can’t reach the server" with Retry now, which skips the wait', async () => {
    const region = await renderConnected();
    const restore = refuseConnections(h.server);
    drop();
    await advance(29_999);
    expect(region).toHaveTextContent('Reconnecting…');
    await advance(1);
    expect(liveRegion(region)).toHaveTextContent("Can't reach the server. Retrying…");
    expect(region).toHaveAttribute('data-kind', 'unreachable');
    // No connection test to open here: only Retry.
    expect(
      within(region)
        .getAllByRole('button')
        .map((b) => b.textContent),
    ).toEqual(['Retry now']);

    // Let the attempt that is due at this very moment fail, so the client waits its next 10 s.
    await advance(100);
    expect(runtime.signal.state).toBe('backoff');
    restore();
    const sockets = h.server.sockets.length;
    fireEvent.click(within(region).getByRole('button', { name: 'Retry now' }));
    expect(h.server.sockets).toHaveLength(sockets + 1);
    await advance();
    expect(runtime.signal.state).toBe('ready');
    expectNoBanner(region);
  });

  it('offers "Test my connection" when the page can open the test', async () => {
    const onTest = vi.fn();
    const region = await renderConnected(onTest);
    refuseConnections(h.server);
    drop();
    await advance(30_000);
    fireEvent.click(within(region).getByRole('button', { name: 'Test my connection' }));
    expect(onTest).toHaveBeenCalledOnce();
  });

  it('disables Retry during a rate-limit wait and counts the seconds down', async () => {
    const region = await renderConnected();
    act(() => {
      h.server.error(makeError('rate_limited', 'connection', { retryAfterMs: 45_000 }));
    });
    await advance(30_000);
    const retry = within(region).getByRole('button');
    expect(retry).toBeDisabled();
    expect(retry).toHaveTextContent('Retry in 15 seconds');
    await advance(1_000);
    expect(retry).toHaveTextContent('Retry in 14 seconds');
    await advance(13_000);
    expect(retry).toHaveTextContent('Retry in 1 second');
    // The wait is over: the client connects by itself.
    await advance(1_000);
    expect(runtime.signal.state).toBe('ready');
    expectNoBanner(region);
  });

  it('keeps the buttons out of the live region: the countdown is not read out every second', async () => {
    const region = await renderConnected(() => undefined);
    act(() => {
      h.server.error(makeError('rate_limited', 'connection', { retryAfterMs: 45_000 }));
    });
    await advance(30_000);
    const live = liveRegion(region);
    // The banner's text is the polite live region (05 §7.1)…
    expect(screen.getByText("Can't reach the server. Retrying…").closest('[aria-live]')).toBe(live);
    expect(live).toHaveAttribute('aria-live', 'polite');
    // …and the countdown, which changes every second for at least 30 s (01 §10.2), is outside every live region.
    const retry = within(region).getByRole('button', { name: 'Retry in 15 seconds' });
    for (const button of within(region).getAllByRole('button')) {
      expect(button.closest('[aria-live]')).toBeNull();
    }
    const said = live.textContent;
    await advance(3_000);
    expect(retry).toHaveTextContent('Retry in 12 seconds');
    expect(live.textContent).toBe(said);
  });

  it('reads "Server restarting…" at once after a server.shutdown, until ready', async () => {
    const region = await renderConnected();
    const restore = refuseConnections(h.server);
    act(() => {
      h.server.shutdown(1_000);
    });
    expect(region).toHaveTextContent('Server restarting…');
    expect(region).toHaveAttribute('data-kind', 'restarting');
    await advance(8_000);
    expect(region).toHaveTextContent('Server restarting…');
    restore();
    await advance(BackoffMaxMs);
    expect(runtime.signal.state).toBe('ready');
    expectNoBanner(region);
  });

  it('shows nothing once the connection is stopped: screens and the login page take over', async () => {
    const region = await renderConnected();
    refuseConnections(h.server);
    drop();
    await advance(2_000);
    expect(region).toHaveTextContent('Reconnecting…');
    act(() => {
      runtime.signal.stop();
    });
    expectNoBanner(region);
  });
});

describe('useConnectionBanner', () => {
  /**
   * Renders the hook on a store whose connection went down `downFor` ms ago, and runs `between` after the hook's
   * first value was computed (during render) and before its effect: a layout effect, which React runs first.
   */
  function renderBetween(downFor: number, between: (store: ReturnType<typeof createConnectionStore>) => void) {
    const store = createConnectionStore();
    store.getState().signalChanged('backoff', { delayMs: 60_000 });
    vi.setSystemTime(Date.now() + downFor);
    return renderHook(() => {
      const banner = useConnectionBanner(store);
      useLayoutEffect(() => {
        between(store);
      }, []);
      return banner;
    });
  }

  it('shows "Reconnecting…" when the 2 s mark passes between its first render and its effect', async () => {
    const { result } = renderBetween(RECONNECTING_AFTER_MS - 1, () => {
      vi.setSystemTime(Date.now() + 5);
    });
    // Not at the next mark (30 s) or the next store change: now.
    expect(result.current?.kind).toBe('reconnecting');
    await advance(27_000);
    expect(result.current?.kind).toBe('reconnecting');
    await advance(1_000);
    expect(result.current?.kind).toBe('unreachable');
  });

  it('shows nothing when the connection comes back between its first render and its effect', async () => {
    const { result } = renderBetween(5_000, (store) => {
      store.getState().signalChanged('ready', {});
    });
    expect(result.current).toBeNull();
    await advance(60_000);
    expect(result.current).toBeNull();
  });
});
