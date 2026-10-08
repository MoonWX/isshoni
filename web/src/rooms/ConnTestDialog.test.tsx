// "Test my connection" in the room: the connection test's panel is a chunk of its own, and the dialog is offered
// when the server may be out of reach. A chunk that can't be loaded must not take the page down.
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useEffect } from 'react';
import { beforeAll, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '../app/App';
import { server, signedIn } from '../test/msw';
import { renderWithApp } from '../test/render';
import { ConnTestDialog } from './ConnTestDialog';
import { PRELOAD_TIMEOUT_MS, preloadLazyChunks } from './testing/page';

function StubPanel() {
  return <p>the connection test</p>;
}

// The real panel's module, loaded before any test waits for it: a first import can take longer than a query's
// timeout on a busy machine.
beforeAll(preloadLazyChunks, PRELOAD_TIMEOUT_MS);

describe('ConnTestDialog', () => {
  it('loads the connection test’s panel when it opens, and starts no test by itself', async () => {
    server.use(signedIn());
    const onClose = vi.fn();
    renderWithApp(<ConnTestDialog open onClose={onClose} />);
    const dialog = screen.getByRole('dialog', { name: 'Test my connection' });
    // The real panel, with its button still to press.
    expect(await within(dialog).findByRole('button', { name: 'Test my connection' })).toBeEnabled();
    expect(within(dialog).queryByText(/Testing your connection/)).not.toBeInTheDocument();

    await userEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('shows a spinner while the panel loads', async () => {
    let arrive: (panel: typeof StubPanel) => void = () => undefined;
    const load = () =>
      new Promise<typeof StubPanel>((resolve) => {
        arrive = resolve;
      });
    renderWithApp(<ConnTestDialog open onClose={() => undefined} load={load} />);
    expect(screen.getByRole('status')).toHaveTextContent('Loading');
    arrive(StubPanel);
    expect(await screen.findByText('the connection test')).toBeInTheDocument();
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });

  it('says so when the panel can’t be loaded, keeps the page, and loads it on Try again', async () => {
    const load = vi
      .fn<() => Promise<typeof StubPanel>>()
      .mockRejectedValueOnce(new TypeError('Failed to fetch dynamically imported module'))
      .mockResolvedValue(StubPanel);
    renderWithApp(
      <>
        <h1>the room</h1>
        <ConnTestDialog open onClose={() => undefined} load={load} />
      </>,
    );
    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't load the connection test.");
    // No error screen: what is around the dialog is still there.
    expect(screen.getByRole('heading', { name: 'the room' })).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText('the connection test')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(load).toHaveBeenCalledTimes(2);
  });

  it('loads nothing while it is closed', () => {
    const load = vi.fn(() => Promise.resolve(StubPanel));
    renderWithApp(<ConnTestDialog open={false} onClose={() => undefined} load={load} />);
    expect(load).not.toHaveBeenCalled();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  // The page keeps the dialog mounted and closes it with `open` (05 §16.6: the focus goes back to what opened it).
  describe('closed and opened again', () => {
    /** Renders the dialog, and renders it again with another `open`. */
    function renderDialog(load: () => Promise<typeof StubPanel>) {
      const ui = (open: boolean) => <ConnTestDialog open={open} onClose={() => undefined} load={load} />;
      const { rerender, services } = renderWithApp(ui(true));
      return {
        setOpen: (open: boolean): void => {
          rerender(<AppProviders services={services}>{ui(open)}</AppProviders>);
        },
      };
    }

    it('keeps no panel while closed: a test that runs ends with the dialog, and the next opening starts anew', async () => {
      let mounts = 0;
      let unmounts = 0;
      function CountedPanel() {
        useEffect(() => {
          mounts++;
          return () => {
            unmounts++;
          };
        }, []);
        return <p>the connection test</p>;
      }
      const { setOpen } = renderDialog(() => Promise.resolve(CountedPanel));
      expect(await screen.findByText('the connection test')).toBeInTheDocument();
      expect([mounts, unmounts]).toEqual([1, 0]);

      setOpen(false);
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(screen.queryByText('the connection test')).not.toBeInTheDocument();
      expect([mounts, unmounts]).toEqual([1, 1]);

      // The panel's code is there already: no spinner the second time.
      setOpen(true);
      expect(screen.getByText('the connection test')).toBeInTheDocument();
      expect(screen.queryByRole('status')).not.toBeInTheDocument();
      expect([mounts, unmounts]).toEqual([2, 1]);
    });

    it('tries the load again, without the error of the opening before', async () => {
      const load = vi
        .fn<() => Promise<typeof StubPanel>>()
        .mockRejectedValueOnce(new TypeError('Failed to fetch dynamically imported module'))
        .mockResolvedValue(StubPanel);
      const { setOpen } = renderDialog(load);
      expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't load the connection test.");

      setOpen(false);
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      setOpen(true);
      // Loading again, not the old message with its "Try again".
      expect(screen.queryByText(/Couldn't load the connection test/)).not.toBeInTheDocument();
      expect(screen.getByRole('status')).toHaveTextContent('Loading');
      expect(await screen.findByText('the connection test')).toBeInTheDocument();
      expect(load).toHaveBeenCalledTimes(2);
    });
  });
});
