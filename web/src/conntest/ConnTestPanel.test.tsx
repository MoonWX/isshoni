// ConnTestPanel (05 §14.2): rows, status, fix text for admins and "Send this to your admin" for everyone else,
// "Couldn't finish the test", the links and "Copy result". The test itself is a double here (runConnTest.test.ts
// covers it).
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { StrictMode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { AppServices } from '../app/context';
import { LocalError } from '../lib/errors';
import { queryKeys } from '../protocol/queryKeys';
import { ApiError } from '../protocol/rest';
import { meFixture } from '../test/msw';
import { createTestServices, renderWithApp } from '../test/render';
import { ConnTestPanel, type ConnTestPanelProps } from './ConnTestPanel';
import { DOCS_URL } from './links';
import type { ConnTestResult, runConnTest } from './runConnTest';
import { resultFixture, type ResultFixtureOptions } from './testing';

type Runner = typeof runConnTest;

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function services(who: 'admin' | 'user' = 'admin'): AppServices {
  const s = createTestServices();
  s.queryClient.setQueryData(queryKeys.me, meFixture({ admin: who === 'admin' }));
  return s;
}

/** Renders the panel for an admin or a member and runs the test once, with the given result. */
async function showResult(
  opts: ResultFixtureOptions | ConnTestResult,
  who: 'admin' | 'user' = 'admin',
  props: Partial<ConnTestPanelProps> = {},
) {
  const result = 'probes' in opts ? opts : resultFixture(opts);
  const runner = vi.fn<Runner>(() => Promise.resolve(result));
  const view = renderWithApp(<ConnTestPanel runner={runner} {...props} />, { services: services(who) });
  fireEvent.click(screen.getByRole('button', { name: 'Test my connection' }));
  await screen.findByRole('button', { name: 'Test again' });
  await screen.findAllByRole('list');
  return { ...view, runner, result };
}

/** The rows as [label, state] pairs. */
function rows(): string[][] {
  const list = screen.getAllByRole('list')[0];
  if (!list) throw new Error('no rows');
  return within(list)
    .getAllByRole('listitem')
    .map((li) => [...li.querySelectorAll(':scope > span:not([aria-hidden])')].map((s) => s.textContent));
}

const udpBlocked: ResultFixtureOptions = { udp: 'timeout' };
const allBlocked: ResultFixtureOptions = { udp: 'timeout', tcp443: 'timeout', tcp7882: 'timeout' };

afterEach(() => {
  vi.useRealTimers();
  Reflect.deleteProperty(navigator, 'clipboard');
});

describe('ConnTestPanel: running the test', () => {
  it('waits for the button, shows that it is testing, then the rows and the status', async () => {
    const run = deferred<ConnTestResult>();
    const runner = vi.fn<Runner>(() => run.promise);
    const app = services();
    renderWithApp(<ConnTestPanel runner={runner} />, { services: app });

    const panel = screen.getByRole('region', { name: 'Connection test' });
    expect(within(panel).getByText(/checks whether video can travel/)).toBeInTheDocument();
    expect(runner).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: 'Test my connection' }));
    expect(await screen.findByText(/Testing your connection…/)).toBeInTheDocument();
    expect(runner).toHaveBeenCalledTimes(1);
    expect(runner.mock.calls[0]?.[0]).toBe(app.platform);
    expect(runner.mock.calls[0]?.[1]?.signal).toBeInstanceOf(AbortSignal);
    expect(panel).toHaveAttribute('aria-busy', 'true');
    // "Test again" is disabled while a run is in progress: another click starts nothing.
    const button = screen.getByRole('button', { name: 'Test my connection' });
    expect(button).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(button);
    expect(runner).toHaveBeenCalledTimes(1);

    await act(async () => {
      run.resolve(resultFixture());
      await run.promise;
    });
    expect(await screen.findByText('Friends get the best quality.')).toBeInTheDocument();
    expect(rows()).toEqual([
      ['Media over UDP (port 7882)', 'Works'],
      ['Media over TCP (port 443)', 'Works'],
      ['Media over TCP (port 7882)', 'Works'],
      ['Round trip', '24 ms, good'],
    ]);
    expect(panel).toHaveAttribute('aria-busy', 'false');
    expect(screen.queryByText(/Testing your connection/)).not.toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'How to fix it' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Copy result' })).toBeInTheDocument();
  });

  it('autoStart runs the test once when the panel shows, also under StrictMode', async () => {
    const runner = vi.fn<Runner>(() => Promise.resolve(resultFixture()));
    renderWithApp(
      <StrictMode>
        <ConnTestPanel autoStart runner={runner} />
      </StrictMode>,
      { services: services() },
    );
    expect(await screen.findByText('Friends get the best quality.')).toBeInTheDocument();
    expect(runner).toHaveBeenCalledTimes(1);
  });

  it('"Test again" runs it again and replaces the result', async () => {
    const { runner } = await showResult(udpBlocked);
    expect(screen.getByText('Works, but video may stutter on weak networks.')).toBeInTheDocument();
    const second = deferred<ConnTestResult>();
    runner.mockReturnValueOnce(second.promise);
    fireEvent.click(screen.getByRole('button', { name: 'Test again' }));
    // While it runs: the old result is gone, and the button keeps its name but takes no click.
    expect(await screen.findByText(/Testing your connection…/)).toBeInTheDocument();
    expect(screen.queryByText('Works, but video may stutter on weak networks.')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Copy result' })).not.toBeInTheDocument();
    const again = screen.getByRole('button', { name: 'Test again' });
    expect(again).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(again);
    expect(runner).toHaveBeenCalledTimes(2);

    await act(async () => {
      second.resolve(resultFixture());
      await second.promise;
    });
    expect(await screen.findByText('Friends get the best quality.')).toBeInTheDocument();
    expect(runner).toHaveBeenCalledTimes(2);
  });

  it('tells the host about every result and announces the outcome to screen readers', async () => {
    const onResult = vi.fn();
    const { result, services: app } = await showResult(allBlocked, 'admin', { onResult });
    expect(onResult).toHaveBeenCalledTimes(1);
    expect(onResult.mock.calls[0]?.[0]).toBe(result);
    expect(onResult.mock.calls[0]?.[1]).toMatchObject({ status: 'red', codes: ['no_media'] });
    expect(app.ui.getState().announcements.polite?.text).toBe("Friends can't receive video yet.");
  });

  it('makes "Test again" the main action unless the result is green', async () => {
    const green = await showResult({});
    expect(screen.getByRole('button', { name: 'Test again' }).className).toMatch(/secondary/);
    green.unmount();
    await showResult(udpBlocked);
    expect(screen.getByRole('button', { name: 'Test again' }).className).toMatch(/primary/);
  });

  it('stops the running test when the panel goes away', async () => {
    let signal: AbortSignal | undefined;
    const runner = vi.fn<Runner>((_platform, opts) => {
      signal = opts?.signal;
      return new Promise<ConnTestResult>(() => undefined);
    });
    const { unmount } = renderWithApp(<ConnTestPanel runner={runner} />, { services: services() });
    fireEvent.click(screen.getByRole('button', { name: 'Test my connection' }));
    await waitFor(() => {
      expect(signal).toBeDefined();
    });
    expect(signal?.aborted).toBe(false);
    unmount();
    expect(signal?.aborted).toBe(true);
  });

  it('says so when the test could not run at all', async () => {
    const runner = vi.fn<Runner>(() => Promise.reject(new LocalError('webrtc_failed')));
    renderWithApp(<ConnTestPanel runner={runner} />, { services: services() });
    fireEvent.click(screen.getByRole('button', { name: 'Test my connection' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      "The test couldn't run. Couldn't set up the video connection.",
    );
    expect(screen.queryByRole('list')).not.toBeInTheDocument();
    runner.mockResolvedValueOnce(resultFixture());
    fireEvent.click(screen.getByRole('button', { name: 'Test again' }));
    expect(await screen.findByText('Friends get the best quality.')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('a signed-out reply clears the session, so the guards send the user to the login page', async () => {
    const runner = vi.fn<Runner>(() => Promise.reject(new ApiError({ status: 401, code: 'unauthenticated' })));
    const app = services();
    renderWithApp(<ConnTestPanel runner={runner} />, { services: app });
    fireEvent.click(screen.getByRole('button', { name: 'Test my connection' }));
    await waitFor(() => {
      expect(app.queryClient.getQueryData(queryKeys.me)).toBeNull();
    });
  });
});

describe('ConnTestPanel: rows', () => {
  it('hides a TCP transport the server has turned off (off mode: no TCP 443)', async () => {
    await showResult({ tcp443: 'disabled', server: { tcpPorts: [7882] } });
    expect(rows()).toEqual([
      ['Media over UDP (port 7882)', 'Works'],
      ['Media over TCP (port 7882)', 'Works'],
      ['Round trip', '24 ms, good'],
    ]);
    expect(screen.getByText('Friends get the best quality.')).toBeInTheDocument();
  });

  it('shows UDP turned off as ✗ with the config key, and counts it as UDP ✗', async () => {
    await showResult({ udp: 'disabled', server: { udpPort: 0 } });
    expect(rows()[0]).toEqual(['Media over UDP', 'Turned off']);
    const note = screen.getByText(/UDP media is turned off in the server config/);
    expect(note).toHaveTextContent('UDP media is turned off in the server config (listen.ice_udp).');
    expect(within(note).getByText('listen.ice_udp').tagName).toBe('CODE');
    expect(screen.getByText('Works, but video may stutter on weak networks.')).toBeInTheDocument();
    // Nothing to open in a firewall: the port isn't listening.
    expect(screen.queryByRole('heading', { name: 'How to fix it' })).not.toBeInTheDocument();
  });

  it('shows ✗ rows and the ports the server uses', async () => {
    await showResult({ ...allBlocked, server: { udpPort: 5000, tcpPorts: [443, 5001] } });
    expect(rows()).toEqual([
      ['Media over UDP (port 5000)', 'Blocked'],
      ['Media over TCP (port 443)', 'Blocked'],
      ['Media over TCP (port 5001)', 'Blocked'],
    ]);
    expect(screen.getByText("Friends can't receive video yet.")).toBeInTheDocument();
  });

  it.each([
    [{ rtt: { udp: 80 } }, '80 ms, good'],
    [{ rtt: { udp: 143.6 } }, '144 ms, OK'],
    [{ rtt: { udp: 7.4 } }, '7.4 ms, good'],
    [{ rtt: { udp: 350 } }, '350 ms, high latency'],
    [{ rtt: { udp: undefined, tcp443: undefined, tcp7882: undefined } }, 'Not measured'],
  ])('round trip %j → %s', async (opts, want) => {
    await showResult(opts);
    expect(rows().at(-1)).toEqual(['Round trip', want]);
  });
});

describe('ConnTestPanel: fix text', () => {
  it('an admin sees the fix text for the provider, with commands and links', async () => {
    await showResult({ ...udpBlocked, server: { provider: 'hetzner', container: 'docker' } });
    const fix = screen.getByRole('region', { name: 'How to fix it' });
    const items = within(fix)
      .getAllByRole('listitem')
      .map((li) => li.querySelector('p')?.textContent);
    expect(items).toEqual([
      'Open UDP port 7882.',
      'In the Hetzner Cloud console, check the Cloud Firewall attached to this server: allow incoming UDP 7882, TCP 443 and 7882, and TCP 80 for certificates.',
      'On the server, if it uses ufw (Ubuntu, Debian):',
      'On the server, if it uses firewalld (Fedora, RHEL):',
      'This server has its public address on its own network interface, so no router or NAT is in the way.',
      'Check that compose publishes 7882:7882/udp; published ports bypass ufw.',
      "If this network blocks UDP (some offices and hotels do), test from home or from your phone's mobile data.",
    ]);
    expect(within(fix).getByText('sudo ufw allow 7882/udp')).toBeInTheDocument();
    expect(
      within(fix).getByText('sudo firewall-cmd --permanent --add-port=7882/udp && sudo firewall-cmd --reload'),
    ).toBeInTheDocument();
    expect(within(fix).getByText('7882:7882/udp').tagName).toBe('CODE');

    const guide = within(fix).getByRole('link', { name: 'Firewall guide for this provider' });
    expect(guide).toHaveAttribute('href', `${DOCS_URL}install/vps#hetzner`);
    expect(within(fix).getByRole('link', { name: 'Publishing ports from Docker' })).toHaveAttribute(
      'href',
      `${DOCS_URL}troubleshooting#ct-docker_ports`,
    );
    // The result's own troubleshooting section.
    const help = screen.getByRole('region', { name: 'Troubleshooting' });
    expect(within(help).getByRole('link', { name: 'UDP is blocked' })).toHaveAttribute(
      'href',
      `${DOCS_URL}troubleshooting#ct-udp_blocked`,
    );
    // 05 §20: external links never pass the opener or the referrer.
    for (const link of screen.getAllByRole('link')) {
      expect(link).toHaveAttribute('rel', 'noopener noreferrer');
      expect(link).toHaveAttribute('target', '_blank');
    }
    expect(screen.queryByText('Send this to your admin:')).not.toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Copy result' })).toHaveLength(1);
  });

  it('a non-admin sees the rows and "Send this to your admin" instead of fix text', async () => {
    await showResult(
      { ...udpBlocked, server: { provider: 'hetzner', container: 'docker', nat: 'port_forward' } },
      'user',
    );
    expect(rows()[0]).toEqual(['Media over UDP (port 7882)', 'Blocked']);
    expect(screen.getByText('Works, but video may stutter on weak networks.')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'How to fix it' })).not.toBeInTheDocument();
    expect(screen.queryByText(/Hetzner|ufw|firewall|Open UDP port|own network/i)).not.toBeInTheDocument();
    const send = screen.getByText('Send this to your admin:');
    expect(within(send.parentElement as HTMLElement).getByRole('button', { name: 'Copy result' })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Copy result' })).toHaveLength(1);
  });

  it('a non-admin with a green result is not asked to send anything', async () => {
    await showResult({}, 'user');
    expect(screen.queryByText('Send this to your admin:')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Copy result' })).toBeInTheDocument();
  });

  it('no public address: one line, and its own troubleshooting section', async () => {
    await showResult({
      ...allBlocked,
      server: { publicIpKnown: false, publicIpPrivate: true, provider: 'aws', container: 'docker' },
    });
    const fix = screen.getByRole('region', { name: 'How to fix it' });
    const items = within(fix).getAllByRole('listitem');
    expect(items).toHaveLength(1);
    expect(items[0]?.querySelector('p')).toHaveTextContent(
      "The server doesn't know its public address, so browsers can't reach its media ports. Set public_ip in " +
        '/etc/isshoni/isshoni.toml (Docker: ISSHONI_PUBLIC_IP in .env) and restart.',
    );
    expect(within(fix).getByText('public_ip').tagName).toBe('CODE');
    const links = screen.getAllByRole('link', { name: "The server doesn't know its public address" });
    expect(links.length).toBeGreaterThan(0);
    for (const link of links) expect(link).toHaveAttribute('href', `${DOCS_URL}troubleshooting#ct-no_public_ip`);
    expect(screen.queryByText(/security group|ufw/)).not.toBeInTheDocument();
  });

  it('behind a home router: admins get the own-network hint on a green result too', async () => {
    const admin = await showResult({ server: { nat: 'port_forward' } });
    expect(screen.getByText('Friends get the best quality.')).toBeInTheDocument();
    expect(
      screen.getByText("You may be testing from the server's own network. Test again from your phone's mobile data."),
    ).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'How to fix it' })).not.toBeInTheDocument();
    admin.unmount();
    await showResult({ server: { nat: 'port_forward' } }, 'user');
    expect(screen.queryByText(/own network/)).not.toBeInTheDocument();
  });

  it('links a green result with TCP 443 blocked or a high round trip to its section', async () => {
    await showResult({ tcp443: 'timeout', rtt: { udp: 420 } });
    expect(screen.getByText('Friends get the best quality.')).toBeInTheDocument();
    const help = screen.getByRole('region', { name: 'Troubleshooting' });
    expect(
      within(help)
        .getAllByRole('link')
        .map((a) => [a.textContent, a.getAttribute('href')]),
    ).toEqual([
      ["TCP port 443 doesn't carry media", `${DOCS_URL}troubleshooting#ct-tcp443_blocked`],
      ['High latency', `${DOCS_URL}troubleshooting#ct-high_rtt`],
    ]);
  });
});

describe('ConnTestPanel: probes that could not run', () => {
  it('shows "not tested" rows and "Couldn\'t finish the test", without a status or fix text', async () => {
    const { services: app } = await showResult({ udp: 'server', tcp443: 'server', tcp7882: 'server', server: null });
    expect(rows()).toEqual([
      ['Media over UDP', 'Not tested'],
      ['Media over TCP (port 443)', 'Not tested'],
      ['Media over TCP (port 7882)', 'Not tested'],
    ]);
    expect(screen.getByText("Couldn't finish the test. Try again in a minute.")).toBeInTheDocument();
    expect(screen.queryByText(/Friends|Works, but/)).not.toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'How to fix it' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Test again' })).toBeEnabled();
    expect(app.ui.getState().announcements.polite?.text).toBe("Couldn't finish the test. Try again in a minute.");
  });

  it('keeps the status when the untested probe cannot change it', async () => {
    await showResult({ tcp443: 'rate_limited' });
    expect(rows()[1]).toEqual(['Media over TCP (port 443)', 'Not tested']);
    expect(screen.getByText('Friends get the best quality.')).toBeInTheDocument();
    expect(screen.getByText("Couldn't finish the test. Try again in a minute.")).toBeInTheDocument();
  });

  it('respects Retry-After: says how long, and "Test again" waits that long', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { runner } = await showResult({
      udp: 'rate_limited',
      tcp443: 'rate_limited',
      tcp7882: 'rate_limited',
      server: null,
      retryAfterSec: 30,
    });
    expect(screen.getByText("Couldn't finish the test. Try again in 30 seconds.")).toBeInTheDocument();
    const again = screen.getByRole('button', { name: 'Test again' });
    expect(again).toBeDisabled();
    await act(() => vi.advanceTimersByTimeAsync(29_000));
    expect(again).toBeDisabled();
    await act(() => vi.advanceTimersByTimeAsync(1_000));
    expect(again).toBeEnabled();

    runner.mockResolvedValueOnce(resultFixture());
    fireEvent.click(again);
    expect(await screen.findByText('Friends get the best quality.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Test again' })).toBeEnabled();
  });

  it('uses the singular for one second', async () => {
    await showResult({ udp: 'rate_limited', server: null, retryAfterSec: 0.4 });
    expect(screen.getByText("Couldn't finish the test. Try again in 1 second.")).toBeInTheDocument();
  });
});

describe('ConnTestPanel: Copy result', () => {
  it('copies the result as JSON', async () => {
    const writeText = vi.fn<(text: string) => Promise<void>>(() => Promise.resolve());
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    const { result } = await showResult(udpBlocked);
    fireEvent.click(screen.getByRole('button', { name: 'Copy result' }));
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument();
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(JSON.parse(writeText.mock.calls[0]?.[0] ?? '')).toEqual(result);
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
  });

  it('shows the text to copy by hand when the browser refuses the clipboard', async () => {
    const writeText = vi.fn<(text: string) => Promise<void>>(() => Promise.reject(new Error('denied')));
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    const { result } = await showResult(allBlocked, 'user');
    fireEvent.click(screen.getByRole('button', { name: 'Copy result' }));
    const field = await screen.findByRole<HTMLTextAreaElement>('textbox', {
      name: "Couldn't copy for you. Select this text and copy it:",
    });
    expect(field).toHaveAttribute('readonly');
    expect(JSON.parse(field.value)).toEqual(result);
  });
});
