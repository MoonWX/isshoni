// The connection banner (05 §7.1) on a real SignalClient against the fake signaling server: nothing for a blip,
// "Reconnecting…" after 2 s and recovery, "Can't reach the server" with its buttons after 30 s, "Server restarting…".
import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '../app/App';
import { BackoffMaxMs } from '../protocol/signal-client';
import { makeError } from '../protocol/testing';
import { ConnectionBanner } from './ConnectionBanner';
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

describe('ConnectionBanner (05 §7.1)', () => {
  it('is an empty polite live region while the connection is fine', async () => {
    const region = await renderConnected();
    expect(region).toHaveAttribute('aria-live', 'polite');
    expect(region).toBeEmptyDOMElement();
  });

  it('shows nothing for a blip, "Reconnecting…" after 2 s, and clears on recovery', async () => {
    const region = await renderConnected();
    const restore = refuseConnections(h.server);
    drop();
    expect(region).toBeEmptyDOMElement();
    await advance(1_999);
    expect(region).toBeEmptyDOMElement();
    await advance(1);
    expect(region).toHaveTextContent('Reconnecting…');
    expect(within(region).queryByRole('button')).not.toBeInTheDocument();

    // It stays through the failed attempts in between.
    await advance(10_000);
    expect(region).toHaveTextContent('Reconnecting…');

    restore();
    await advance(BackoffMaxMs);
    expect(runtime.signal.state).toBe('ready');
    expect(region).toBeEmptyDOMElement();
  });

  it('a drop that recovers within 2 s never shows', async () => {
    const region = await renderConnected();
    drop();
    await advance(500);
    expect(runtime.signal.state).toBe('ready');
    await advance(2_000);
    expect(region).toBeEmptyDOMElement();
  });

  it('after 30 s: "Can’t reach the server" with Retry now, which skips the wait', async () => {
    const region = await renderConnected();
    const restore = refuseConnections(h.server);
    drop();
    await advance(29_999);
    expect(region).toHaveTextContent('Reconnecting…');
    await advance(1);
    expect(region).toHaveTextContent("Can't reach the server. Retrying…");
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
    expect(region).toBeEmptyDOMElement();
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
    expect(region).toBeEmptyDOMElement();
  });

  it('reads "Server restarting…" at once after a server.shutdown, until ready', async () => {
    const region = await renderConnected();
    const restore = refuseConnections(h.server);
    act(() => {
      h.server.shutdown(1_000);
    });
    expect(region).toHaveTextContent('Server restarting…');
    expect(region.querySelector('[data-kind="restarting"]')).toBeInTheDocument();
    await advance(8_000);
    expect(region).toHaveTextContent('Server restarting…');
    restore();
    await advance(BackoffMaxMs);
    expect(runtime.signal.state).toBe('ready');
    expect(region).toBeEmptyDOMElement();
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
    expect(region).toBeEmptyDOMElement();
  });
});
