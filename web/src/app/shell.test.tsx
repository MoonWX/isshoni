import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { I18nextProvider } from 'react-i18next';
import { createMemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { i18n } from '../i18n';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderWithApp } from '../test/render';
import { Announcer } from './Announcer';
import { App } from './App';
import { AppErrorBoundary } from './ErrorBoundary';
import { AppScreenView, Fatal, VersionMismatch } from './screens';
import { Toasts } from './Toasts';
import type { AppScreen } from './uiStore';

const withI18n = (ui: React.ReactNode) => render(<I18nextProvider i18n={i18n}>{ui}</I18nextProvider>);

describe('app-level screens (05 §4)', () => {
  it.each<[AppScreen, string, RegExp]>([
    [{ kind: 'offline' }, "Can't reach the server", /keeps trying/],
    [{ kind: 'needsHttps' }, 'isshoni needs HTTPS', /TLS setup/],
    [{ kind: 'notSetUp' }, "This server isn't set up yet", /open the link it prints/],
    [{ kind: 'unsupported' }, "This browser can't run isshoni", /Chrome, Edge, Firefox or Safari/],
    [{ kind: 'fatal' }, 'Something went wrong', /Reload the page/],
    [{ kind: 'fatal', reason: 'account_disabled' }, 'Your account was disabled', /An admin turned off your account/],
    [{ kind: 'fatal', reason: 'too_many_connections' }, 'isshoni is open in too many tabs', /Close the isshoni tabs/],
    [{ kind: 'fatal', reason: 'media' }, "Can't connect media", /failed twice/],
    [{ kind: 'versionMismatch' }, 'This page is out of date', /Reload to get the matching version/],
  ])('%j → %s', (screenValue, title, body) => {
    withI18n(<AppScreenView screen={screenValue} platform={createTestPlatform()} />);
    const heading = screen.getByRole('heading', { level: 1 });
    expect(heading).toHaveTextContent(title);
    expect(heading).toHaveFocus();
    expect(screen.getByText(body)).toBeInTheDocument();
  });

  it('Fatal shows the code as small print and reloads through the given action', async () => {
    const onReload = vi.fn();
    withI18n(<Fatal code="not_found" onReload={onReload} />);
    expect(screen.getByText('Error: not_found')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Reload' }));
    expect(onReload).toHaveBeenCalledOnce();
  });

  it('VersionMismatch: reload when the page is old; the admin when the server is older', async () => {
    const reload = vi.fn();
    const { unmount } = withI18n(<VersionMismatch serverVersion="0.4.0" actions={{ reload }} />);
    expect(screen.getByText(`This page: ${__ISSHONI_VERSION__} · Server: 0.4.0`)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Reload' }));
    expect(reload).toHaveBeenCalledOnce();
    unmount();
    withI18n(<VersionMismatch serverVersion="0.2.0" serverOlder actions={{ reload }} />);
    expect(screen.getByText(/Ask your admin to update the server/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Reload' })).not.toBeInTheDocument();
  });

  it('VersionMismatch after a reload that did not help', () => {
    withI18n(<VersionMismatch stillStale actions={{}} />);
    expect(screen.getByText(/Reloading didn't help/)).toBeInTheDocument();
    expect(screen.getByText(`This page: ${__ISSHONI_VERSION__} · Server: ?`)).toBeInTheDocument();
  });
});

describe('the shell (App)', () => {
  const router = () => createMemoryRouter([{ path: '*', element: <h1>page</h1> }]);

  it('renders the router, and an app-level screen from uiStore instead of it', () => {
    const services = createTestServices();
    render(<App services={services} router={router()} />);
    expect(screen.getByRole('heading', { name: 'page' })).toBeInTheDocument();
    act(() => {
      services.ui.getState().showScreen({ kind: 'fatal', reason: 'too_many_connections' });
    });
    expect(screen.queryByRole('heading', { name: 'page' })).not.toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('isshoni is open in too many tabs');
  });

  it('mounts the toasts region, the live regions and the update pill (stub: nothing visible)', () => {
    const services = createTestServices();
    render(<App services={services} router={router()} />);
    expect(screen.getByRole('region', { name: 'Notifications' })).toBeInTheDocument();
    expect(screen.getByTestId('announcer-polite')).toHaveAttribute('aria-live', 'polite');
    expect(screen.getByTestId('announcer-assertive')).toHaveAttribute('aria-live', 'assertive');
  });

  it('the app error boundary shows Fatal when a render outside the router throws', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const Boom = () => {
      throw new Error('render failed');
    };
    withI18n(
      <AppErrorBoundary>
        <Boom />
      </AppErrorBoundary>,
    );
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Something went wrong');
    spy.mockRestore();
  });
});

describe('Toasts', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('lists toasts as plain list items and announces them through the live regions; action and ✕ dismiss', async () => {
    const { services } = renderWithApp(
      <>
        <Announcer />
        <Toasts />
      </>,
    );
    const run = vi.fn();
    act(() => {
      services.ui.getState().toast({ kind: 'info', message: 'bo started sharing', action: { label: 'Watch', run } });
      services.ui.getState().toast({ kind: 'error', message: 'Room is full' });
    });
    const region = screen.getByRole('region', { name: 'Notifications' });
    const items = within(within(region).getByRole('list')).getAllByRole('listitem');
    expect(items.map((li) => li.textContent)).toEqual([
      expect.stringContaining('bo started sharing'),
      expect.stringContaining('Room is full'),
    ]);
    expect(within(region).queryByRole('status')).not.toBeInTheDocument();
    expect(within(region).queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByTestId('announcer-polite')).toHaveTextContent('bo started sharing');
    expect(screen.getByTestId('announcer-assertive')).toHaveTextContent('Room is full');
    await userEvent.click(screen.getByRole('button', { name: 'Watch' }));
    expect(run).toHaveBeenCalledOnce();
    expect(within(region).queryByText('bo started sharing')).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(services.ui.getState().toasts).toEqual([]);
  });

  it('leaves after its duration, paused while hovered', () => {
    vi.useFakeTimers();
    const { services } = renderWithApp(<Toasts />);
    act(() => {
      services.ui.getState().toast({ kind: 'info', message: 'hello', durationMs: 1000 });
      services.ui.getState().toast({ kind: 'info', message: 'sticky', durationMs: null });
    });
    const item = screen.getByText('hello').closest('li');
    act(() => {
      vi.advanceTimersByTime(400);
    });
    act(() => {
      item?.dispatchEvent(new Event('pointerenter'));
    });
    act(() => {
      vi.advanceTimersByTime(5000);
    });
    expect(screen.getByText('hello')).toBeInTheDocument();
    act(() => {
      item?.dispatchEvent(new Event('pointerleave'));
    });
    // 600 ms were left when the pointer arrived.
    act(() => {
      vi.advanceTimersByTime(599);
    });
    expect(screen.getByText('hello')).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(screen.queryByText('hello')).not.toBeInTheDocument();
    expect(screen.getByText('sticky')).toBeInTheDocument();
  });

  describe('while an element is fullscreen (05 §12.5)', () => {
    /** What the browser reports as the fullscreen element; jsdom has no fullscreen of its own. */
    let fullscreen: Element | null = null;
    const enter = (el: Element | null, event = 'fullscreenchange'): void => {
      act(() => {
        fullscreen = el;
        document.dispatchEvent(new Event(event));
      });
    };
    const region = () => screen.getByRole('region', { name: 'Notifications' });

    afterEach(() => {
      fullscreen = null;
      Reflect.deleteProperty(document, 'fullscreenElement');
      Reflect.deleteProperty(document, 'webkitFullscreenElement');
    });

    it('the region is inside that element, where the browser draws it, and comes back afterwards', () => {
      Object.defineProperty(document, 'fullscreenElement', { get: () => fullscreen, configurable: true });
      const { services, container } = renderWithApp(
        <>
          <div data-testid="stage" />
          <Toasts />
        </>,
      );
      const stage = screen.getByTestId('stage');
      act(() => {
        services.ui.getState().toast({ kind: 'info', message: 'bo started sharing', durationMs: null });
      });
      expect(stage).not.toContainElement(region());

      enter(stage);
      expect(stage).toContainElement(region());
      expect(within(region()).getByText('bo started sharing')).toBeInTheDocument();
      expect(screen.getAllByRole('region', { name: 'Notifications' })).toHaveLength(1);

      enter(null);
      expect(stage).not.toContainElement(region());
      expect(container).toContainElement(region());
      expect(within(region()).getByText('bo started sharing')).toBeInTheDocument();
    });

    it('also where only the prefixed API exists', () => {
      Object.defineProperty(document, 'webkitFullscreenElement', { get: () => fullscreen, configurable: true });
      renderWithApp(
        <>
          <div data-testid="stage" />
          <Toasts />
        </>,
      );
      enter(screen.getByTestId('stage'), 'webkitfullscreenchange');
      expect(screen.getByTestId('stage')).toContainElement(region());
    });

    it('stays where it is for a fullscreen video, which shows no children', () => {
      Object.defineProperty(document, 'fullscreenElement', { get: () => fullscreen, configurable: true });
      const { container } = renderWithApp(<Toasts />);
      const video = container.appendChild(document.createElement('video'));
      enter(video);
      expect(video).not.toContainElement(region());
      expect(container).toContainElement(region());
    });
  });
});

describe('Announcer (05 §16.6)', () => {
  it('writes each announcement into its live region', () => {
    const { services } = renderWithApp(<Announcer />);
    act(() => {
      services.ui.getState().announce('alex started sharing');
      services.ui.getState().announce('Sharing stopped', 'assertive');
    });
    expect(screen.getByTestId('announcer-polite')).toHaveTextContent('alex started sharing');
    expect(screen.getByTestId('announcer-assertive')).toHaveTextContent('Sharing stopped');
  });
});
