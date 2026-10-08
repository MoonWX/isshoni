import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { StrictMode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { BrowserPwa, VERSION_POLL_MS } from '../platform/browser/pwa';
import type { PwaProvider } from '../platform/types';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderWithApp } from '../test/render';
import { Announcer } from './Announcer';
import { AppProviders } from './App';
import { UpdatePill } from './UpdatePill';

/** A PwaProvider whose update the test makes ready. */
function fakePwa() {
  const listeners = new Set<() => void>();
  const applyUpdate = vi.fn();
  const pwa: PwaProvider = {
    installState: () => 'none',
    promptInstall: () => Promise.resolve('unavailable'),
    onUpdateReady: (fn) => {
      listeners.add(fn);
      return () => {
        listeners.delete(fn);
      };
    },
    applyUpdate,
  };
  return {
    pwa,
    applyUpdate,
    listeners,
    makeReady: () => {
      act(() => {
        for (const fn of [...listeners]) fn();
      });
    },
  };
}

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('UpdatePill (05 §16.2)', () => {
  it('shows nothing until an update is ready', () => {
    const { pwa } = fakePwa();
    renderWithApp(<UpdatePill />, { services: createTestServices({ platform: createTestPlatform({ pwa }) }) });
    expect(screen.queryByRole('region', { name: 'App update' })).not.toBeInTheDocument();
    expect(screen.queryByText('Update ready')).not.toBeInTheDocument();
  });

  it('offers "Update ready · Reload" when the platform reports an update, and tells screen readers', () => {
    const { pwa, makeReady } = fakePwa();
    const services = createTestServices({ platform: createTestPlatform({ pwa }) });
    renderWithApp(
      <>
        <UpdatePill />
        <Announcer />
      </>,
      { services },
    );
    makeReady();
    const pill = screen.getByRole('region', { name: 'App update' });
    expect(pill).toHaveTextContent('Update ready');
    expect(screen.getByRole('button', { name: 'Reload' })).toBeInTheDocument();
    expect(services.ui.getState().updateReady).toBe(true);
    expect(screen.getByTestId('announcer-polite')).toHaveTextContent(
      'A new version of isshoni is ready. Reload to update.',
    );
  });

  it('Reload applies the update through the platform, once', async () => {
    const { pwa, applyUpdate, makeReady } = fakePwa();
    renderWithApp(<UpdatePill />, { services: createTestServices({ platform: createTestPlatform({ pwa }) }) });
    makeReady();
    const button = screen.getByRole('button', { name: 'Reload' });
    await userEvent.click(button);
    expect(applyUpdate).toHaveBeenCalledOnce();
    expect(button).toHaveAttribute('aria-busy', 'true');
    await userEvent.click(button);
    expect(applyUpdate).toHaveBeenCalledOnce();
  });

  it('shows for an update another part flagged in uiStore, and reloads through the platform without a PWA', async () => {
    const reload = vi.fn();
    const platform = createTestPlatform({ pwa: null, versionActions: () => ({ reload }) });
    const { services } = renderWithApp(<UpdatePill />, { services: createTestServices({ platform }) });
    expect(screen.queryByText('Update ready')).not.toBeInTheDocument();
    act(() => {
      services.ui.getState().setUpdateReady(true);
    });
    await userEvent.click(screen.getByRole('button', { name: 'Reload' }));
    expect(reload).toHaveBeenCalledOnce();
  });

  it('while sharing it only says that an update waits: a reload would end the share', () => {
    const { pwa, makeReady } = fakePwa();
    const services = createTestServices({ platform: createTestPlatform({ pwa }) });
    const { rerender } = render(
      <AppProviders services={services}>
        <UpdatePill sharing />
      </AppProviders>,
    );
    makeReady();
    expect(screen.getByRole('region', { name: 'App update' })).toHaveTextContent(
      'Update ready. Reload after you stop sharing.',
    );
    expect(screen.queryByRole('button', { name: 'Reload' })).not.toBeInTheDocument();

    rerender(
      <AppProviders services={services}>
        <UpdatePill sharing={false} />
      </AppProviders>,
    );
    expect(screen.getByRole('button', { name: 'Reload' })).toBeInTheDocument();
  });

  it('announces once, also under StrictMode with the update ready before it mounts, and unsubscribes', () => {
    const { pwa, listeners, makeReady } = fakePwa();
    const services = createTestServices({ platform: createTestPlatform({ pwa }) });
    services.ui.getState().setUpdateReady(true);
    const announce = vi.spyOn(services.ui.getState(), 'announce');
    const { unmount } = render(
      <StrictMode>
        <AppProviders services={services}>
          <UpdatePill />
        </AppProviders>
      </StrictMode>,
    );
    expect(screen.getByText('Update ready')).toBeInTheDocument();
    expect(listeners.size).toBe(1);
    makeReady(); // the platform reports what uiStore already knew
    expect(announce).toHaveBeenCalledOnce();
    unmount();
    expect(listeners.size).toBe(0);
  });
});

describe('the pill appears when version.json changes (S38 acceptance)', () => {
  const versionJson = (shell: string) => Response.json({ version: __ISSHONI_VERSION__, protocol: 1, shell });

  it('with the browser’s real provider: a new shell hash on the server makes the update ready', async () => {
    vi.useFakeTimers();
    let shell = 'aaaaaaaaaaaa';
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(versionJson(shell))),
    );
    const reload = vi.fn();
    // No service worker here (as in a browser without one): the changed file is the whole signal.
    const pwa = new BrowserPwa({ serviceWorker: null, reload });
    try {
      renderWithApp(<UpdatePill />, { services: createTestServices({ platform: createTestPlatform({ pwa }) }) });
      await act(async () => {
        await pwa.register();
        await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
      });
      expect(screen.queryByText('Update ready')).not.toBeInTheDocument();

      shell = 'bbbbbbbbbbbb'; // the server was updated
      await act(async () => {
        await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);
      });
      expect(screen.getByRole('region', { name: 'App update' })).toHaveTextContent('Update ready');

      vi.useRealTimers();
      await userEvent.click(screen.getByRole('button', { name: 'Reload' }));
      expect(reload).toHaveBeenCalledOnce();
    } finally {
      pwa.dispose();
    }
  });

  it('shows for a listener that mounts after the update became ready (boot was still loading)', async () => {
    vi.useFakeTimers();
    let shell = 'aaaaaaaaaaaa';
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(versionJson(shell))),
    );
    const pwa = new BrowserPwa({ serviceWorker: null, reload: vi.fn() });
    try {
      await pwa.register();
      await vi.advanceTimersByTimeAsync(0);
      shell = 'bbbbbbbbbbbb';
      await vi.advanceTimersByTimeAsync(VERSION_POLL_MS);

      renderWithApp(<UpdatePill />, { services: createTestServices({ platform: createTestPlatform({ pwa }) }) });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(screen.getByText('Update ready')).toBeInTheDocument();
    } finally {
      pwa.dispose();
    }
  });
});
