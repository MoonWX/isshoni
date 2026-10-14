// /admin, the dashboard (05 §15.3, 04 §11.4) against MSW: the snapshot's parts, the alerts, the setup checklist,
// and the poll: every 2 s while the page is visible, none while it is hidden, and at once when it is shown again.
import { screen, within } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { Alert, OpsDashboard, ServerInfo, TLSInfo } from '../protocol/api.gen';
import { apiError, meFixture } from '../test/msw';
import { DASHBOARD_POLL_MS } from './adminApi';
import {
  auditEntry,
  dashboardAccounts,
  on,
  opsDashboard,
  renderAdmin,
  serve,
  settingsResponse,
  without,
} from './testing/harness';

/** Serves the dashboard (a fresh generatedAt on every request) and the settings. */
function dashboardApi(snapshot: () => OpsDashboard = () => opsDashboard(), setupWizardDone = true) {
  let n = 0;
  const gets = serve('/api/v1/admin/dashboard', () => {
    n += 1;
    return { ...snapshot(), generatedAt: new Date(Date.UTC(2026, 8, 29, 20, 14, n)).toISOString() };
  });
  serve('/api/v1/admin/settings', () => settingsResponse({ setupWizardDone }));
  return gets;
}

async function openDashboard(snapshot?: () => OpsDashboard, setupWizardDone = true) {
  const gets = dashboardApi(snapshot, setupWizardDone);
  const page = renderAdmin({ path: '/admin' });
  await screen.findByRole('heading', { level: 1, name: 'Dashboard' });
  await screen.findByText('Sending now');
  return { gets, ...page };
}

/** The fixture with some of the server object's fields replaced. */
function withServer(server: Partial<ServerInfo>, overrides: Partial<OpsDashboard> = {}): () => OpsDashboard {
  return () => {
    const d = opsDashboard(overrides);
    return { ...d, server: { ...d.server, ...server } };
  };
}

/** The texts of the alerts, as the page shows them (the first paragraph of each). */
function alertTexts(): (string | undefined)[] {
  const items = within(screen.getByRole('region', { name: 'Alerts' })).getAllByRole('listitem');
  return items.map((li) => li.querySelector('p')?.textContent ?? undefined);
}

/** The ten certificate codes of 04 §8.7: none of them is text an admin reads. */
const CERTIFICATE_CODE =
  /tls\.(acme_unreachable|dns_missing|dns_wrong|rate_limited|ip_rejected|caa_forbids|acme_failed|cert_unreadable|cert_invalid|cert_expired)/;

/** What the server sends for a section whose source failed (Go's zero values, 04 §11.4). */
const ZERO_SERVER: Partial<ServerInfo> = {
  startedAt: '0001-01-01T00:00:00.000Z',
  origin: '',
  tls: { mode: '' as ServerInfo['tls']['mode'], names: [], ready: false },
  publicIpv4: '',
  publicIpv6: '',
  nat: '' as ServerInfo['nat'],
  advertised: [],
};

function sourceFailed(...sources: string[]): Alert[] {
  return sources.map((source) => ({ code: 'dashboard.source_failed', severity: 'warn', params: { source } }));
}

let visibility: DocumentVisibilityState = 'visible';

function setVisibility(state: DocumentVisibilityState): void {
  visibility = state;
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility });
  document.dispatchEvent(new Event('visibilitychange', { bubbles: true }));
}

/** Waits, in real time, until cond holds (the timers that the poll uses may be fake). */
async function until(cond: () => boolean, ms = 3000): Promise<void> {
  const end = performance.now() + ms;
  while (!cond()) {
    if (performance.now() > end) throw new Error('timed out');
    await new Promise((resolve) => {
      globalThis.setTimeout(resolve, 10);
    });
  }
}

afterEach(() => {
  vi.useRealTimers();
  setVisibility('visible');
});

describe('DashboardPage', () => {
  it('shows the transfer, the people and shares, and this month', async () => {
    await openDashboard();
    const stats = screen.getByRole('region', { name: 'Right now' });
    expect(within(stats).getByText('Sending now').nextElementSibling).toHaveTextContent('41.5 Mb/s');
    expect(stats).toHaveTextContent('Receiving 17.0 Mb/s');
    expect(within(stats).getByText('People online').nextElementSibling).toHaveTextContent('2');
    expect(stats).toHaveTextContent('2 connections');
    expect(within(stats).getByText('Live shares').nextElementSibling).toHaveTextContent('1');
    expect(stats).toHaveTextContent('Media: UDP 7, TCP 443 1, TCP 7882 0');
    expect(within(stats).getByText('Sent in September 2026').nextElementSibling).toHaveTextContent('412.0 GB');
    expect(stats).toHaveTextContent('On course for 428.0 GB this month');
    expect(stats).toHaveTextContent('Alert at 1,000 GB');
  });

  it('lists the live rooms with every share’s layers and viewers, and who is connected how', async () => {
    await openDashboard();
    const lounge = screen.getByRole('article', { name: 'Lounge' });
    expect(within(lounge).getByRole('link', { name: 'Lounge' })).toHaveAttribute('href', '/r/lounge');
    expect(lounge).toHaveTextContent('2 people');
    expect(lounge).toHaveTextContent("alex's window");
    expect(lounge).toHaveTextContent('h264/640c, started');

    const layers = within(lounge).getByRole('table', { name: "Layers of alex's window" });
    const high = within(layers).getByRole('row', { name: /^High/ });
    expect(high).toHaveTextContent('1920 × 1080');
    expect(high).toHaveTextContent('59.8');
    expect(high).toHaveTextContent('7.9 Mb/s');
    expect(high).toHaveTextContent('0.1%');
    expect(within(layers).getByRole('row', { name: /^Low/ })).toHaveTextContent('290 kb/s');
    expect(lounge).toHaveTextContent('Watching: 1 in high quality, 0 in low quality, 1 with sound.');
    expect(lounge).toHaveTextContent('Sending 8.3 Mb/s to them');

    const people = within(lounge).getByRole('table', { name: 'People in Lounge' });
    const sam = within(people).getByRole('row', { name: /^sam/ });
    expect(sam).toHaveTextContent('Web 0.3.0 on iOS');
    expect(sam).toHaveTextContent('over TCP 443, 61 ms');
    expect(sam).toHaveTextContent('1 share');
    expect(within(people).getByRole('row', { name: /^alex/ })).toHaveTextContent('Nothing');
  });

  it('says so when nobody is in a room', async () => {
    await openDashboard(() =>
      opsDashboard({ rooms: [{ id: 'lounge', name: 'Lounge', participants: [], shares: [] }] }),
    );
    expect(screen.getByText('Nobody is in a room right now.')).toBeInTheDocument();
    expect(screen.queryByRole('article')).not.toBeInTheDocument();
  });

  it('shows the server, its certificate and addresses, doctor’s summary and the app versions', async () => {
    await openDashboard();
    const server = screen.getByRole('region', { name: 'Server' });
    expect(server).toHaveTextContent('Version0.3.0');
    expect(server).toHaveTextContent('Addresshttps://203.0.113.7');
    expect(server).toHaveTextContent("Let's Encrypt IP certificate for 203.0.113.7");
    expect(within(server).getByText('Ready')).toBeInTheDocument();
    expect(server).toHaveTextContent('Valid until Oct 5, 2026, renews on Oct 2, 2026');
    expect(server).toHaveTextContent('NAT: none');
    expect(server).toHaveTextContent('203.0.113.7:7882 UDP');
    expect(server).toHaveTextContent('187 MB of memory');

    const doctor = screen.getByRole('region', { name: 'Doctor' });
    expect(doctor).toHaveTextContent('12 ok');
    expect(doctor).toHaveTextContent('1 warning');
    expect(doctor).toHaveTextContent('0 problems');
    expect(within(doctor).getByRole('link', { name: 'Open Doctor' })).toHaveAttribute('href', '/admin/doctor');

    const clients = screen.getByRole('region', { name: 'App versions' });
    expect(within(clients).getByRole('row', { name: /^Web/ })).toHaveTextContent('Web0.3.0 2');
  });

  it('says that doctor hasn’t run yet, and shows an outdated app', async () => {
    await openDashboard(() =>
      opsDashboard({ doctor: undefined, clients: [{ kind: 'web', version: '0.2.0', count: 1, outdated: true }] }),
    );
    expect(screen.getByRole('region', { name: 'Doctor' })).toHaveTextContent("Doctor hasn't run yet.");
    expect(within(screen.getByRole('region', { name: 'App versions' })).getByText('Outdated')).toBeInTheDocument();
  });

  it('shows the accounts and the recent security events, without IP addresses', async () => {
    const event = auditEntry({
      action: 'auth.login_failed',
      outcome: 'denied',
      actor: { kind: 'anonymous', name: 'sam' },
      target: undefined,
      ip: '198.51.100.77',
    });
    await openDashboard(() => opsDashboard({ accounts: dashboardAccounts({ securityEvents: [event] }) }));
    const accounts = screen.getByRole('region', { name: 'Accounts' });
    expect(accounts).toHaveTextContent('Registration: invite, or ask an admin.');
    expect(within(accounts).getByText('Active accounts').nextElementSibling).toHaveTextContent('6');
    expect(within(accounts).getByRole('link', { name: 'Answer them' })).toHaveAttribute('href', '/admin/approvals');
    expect(accounts).toHaveTextContent('7 signed-in browsers');
    const events = within(accounts).getByRole('table', { name: 'Security events of the last 7 days' });
    expect(within(events).getAllByRole('row')).toHaveLength(2);
    expect(events).not.toHaveTextContent('198.51.100.77');
  });

  it('leaves out a part whose source failed and says which', async () => {
    const alerts: Alert[] = [
      { code: 'dashboard.source_failed', severity: 'warn', params: { source: 'accounts' } },
      { code: 'dashboard.source_failed', severity: 'warn', params: { source: 'newthing' } },
    ];
    await openDashboard(() => opsDashboard({ accounts: undefined, alerts }));
    const list = screen.getByRole('region', { name: 'Alerts' });
    expect(list).toHaveTextContent("Warning: Couldn't read the accounts: that part of the dashboard is empty.");
    expect(list).toHaveTextContent("Couldn't read newthing: that part of the dashboard is empty.");
    expect(screen.queryByRole('region', { name: 'Accounts' })).not.toBeInTheDocument();
  });

  it('says what each alert means', async () => {
    const alerts: Alert[] = [
      { code: 'transfer.100', severity: 'error', params: { month: '2026-09', alertGb: 1000 } },
      { code: 'transfer.80', severity: 'warn', params: { month: '2026-09', alertGb: 500 } },
      { code: 'release.security_update', severity: 'warn', params: { version: '0.3.1' } },
      { code: 'release.update', severity: 'info', params: { version: '0.3.1' } },
      {
        code: 'public_ip.changed',
        severity: 'warn',
        params: { family: 'ipv4', from: '203.0.113.7', to: '203.0.113.99' },
      },
      { code: 'public_ip.changed', severity: 'warn', params: { family: 'ipv6', to: '2001:db8::9' } },
      {
        code: 'tls.renewal_failing',
        severity: 'warn',
        params: { code: 'tls.acme_unreachable', notAfter: '2026-10-05T10:00:00.000Z' },
      },
      { code: 'doctor.fail', severity: 'error', params: { fail: 2, checks: ['public_ip', 'tls'] } },
      { code: 'brand.new' as Alert['code'], severity: 'info' },
    ];
    const update = {
      latest: '0.3.1',
      url: 'https://github.com/MoonWX/isshoni/releases/tag/v0.3.1',
      security: true,
      checkedAt: '2026-09-29T09:12:00.000Z',
    };
    await openDashboard(() => {
      const d = opsDashboard({ alerts });
      return { ...d, server: { ...d.server, update } };
    });
    const items = within(screen.getByRole('region', { name: 'Alerts' })).getAllByRole('listitem');
    expect(items.map((li) => li.querySelector('p')?.textContent)).toEqual([
      'Problem: The server has sent more than the 1,000 GB transfer alert in September 2026.',
      'Warning: The server has sent more than 80% of the 500 GB transfer alert in September 2026.',
      'Warning: isshoni 0.3.1 is available (security fix).',
      'Note: isshoni 0.3.1 is available.',
      "Warning: The server's public IPv4 address changed from 203.0.113.7 to 203.0.113.99. isshoni keeps using the old one until it restarts.",
      'Warning: The server has a public IPv6 address now, 2001:db8::9. isshoni uses it after a restart.',
      expect.stringMatching(/^Warning: The certificate can't be renewed\. It is valid until Oct 5, 2026, /),
      'Problem: Doctor found 2 problems: Public IP and Certificate.',
      'Note: brand.new',
    ]);
    // The certificate's alert says what to do, in doctor's words.
    expect(items[6]).toHaveTextContent(
      "Let's Encrypt couldn't connect to 203.0.113.7 on port 80 or 443. Open TCP 80 and 443 in your cloud firewall.",
    );
    expect(within(items[2] as HTMLElement).getByRole('link', { name: 'Release notes' })).toHaveAttribute(
      'href',
      update.url,
    );
    expect(within(items[7] as HTMLElement).getByRole('link', { name: 'Open Doctor' })).toHaveAttribute(
      'href',
      '/admin/doctor',
    );
  });

  it('says what went wrong with a manual certificate, in doctor’s words, not as a code', async () => {
    await openDashboard(
      withServer({
        tls: {
          mode: 'manual',
          names: ['watch.example.com'],
          ready: false,
          lastErrorCode: 'tls.cert_unreadable',
          lastErrorAt: '2026-09-29T20:10:00.000Z',
        },
      }),
    );
    const server = screen.getByRole('region', { name: 'Server' });
    expect(server).toHaveTextContent('Your own certificate for watch.example.com');
    expect(within(server).getByText('Not ready')).toBeInTheDocument();
    expect(server).toHaveTextContent(
      "isshoni can't read tls.cert_file or tls.key_file. Check the paths, and let the user that isshoni runs as " +
        'read both, for example:',
    );
    expect(within(server).getByText('sudo chgrp isshoni <file> && sudo chmod 640 <file>')).toBeInTheDocument();
    expect(
      within(server)
        .getByText(/^Last error/)
        .querySelector('time'),
    ).toHaveAttribute('datetime', '2026-09-29T20:10:00.000Z');
    expect(server).not.toHaveTextContent(CERTIFICATE_CODE);
    // A manual certificate raises no alert: the card is where the admin reads it.
    expect(screen.queryByRole('region', { name: 'Alerts' })).not.toBeInTheDocument();
  });

  it('says why a first certificate order failed, with the address when there is one', async () => {
    const tls: TLSInfo = { mode: 'auto', names: ['watch.example.com'], ready: false, lastErrorCode: 'tls.dns_wrong' };
    const { unmount } = await openDashboard(withServer({ tls }));
    let server = screen.getByRole('region', { name: 'Server' });
    expect(server).toHaveTextContent("Let's Encrypt certificate for watch.example.com");
    expect(server).toHaveTextContent(
      "watch.example.com doesn't point to this server (203.0.113.7). Change its A record (see the DNS check).",
    );
    expect(server).not.toHaveTextContent(/Valid until/);
    expect(server).not.toHaveTextContent(CERTIFICATE_CODE);
    unmount();

    await openDashboard(withServer({ tls, publicIpv4: '' }));
    server = screen.getByRole('region', { name: 'Server' });
    expect(server).toHaveTextContent(
      "watch.example.com doesn't point to this server. Change its A record (see the DNS check).",
    );
    expect(server).not.toHaveTextContent(CERTIFICATE_CODE);
  });

  it('shows a code that is no certificate code as it is', async () => {
    await openDashboard(
      withServer({
        tls: { mode: 'auto', names: ['watch.example.com'], ready: false, lastErrorCode: 'acme.brand_new' },
      }),
    );
    expect(screen.getByRole('region', { name: 'Server' })).toHaveTextContent('acme.brand_new');
  });

  it('tells an expired certificate from one the server no longer serves', async () => {
    const renewal = (severity: Alert['severity'], notAfter: string, code = 'tls.acme_unreachable'): Alert => ({
      code: 'tls.renewal_failing',
      severity,
      params: { code, notAfter },
    });
    // The snapshots are made on 2026-09-29.
    await openDashboard(() =>
      opsDashboard({
        alerts: [
          renewal('warn', '2026-10-05T10:00:00.000Z'),
          renewal('error', '2026-10-05T10:00:00.000Z'),
          renewal('error', '2026-09-28T10:00:00.000Z'),
        ],
      }),
    );
    expect(alertTexts()).toEqual([
      expect.stringMatching(/^Warning: The certificate can't be renewed\. It is valid until Oct 5, 2026, /),
      expect.stringMatching(
        /^Problem: The certificate can't be renewed, and the server isn't using it any more\. It would be valid until Oct 5, 2026, /,
      ),
      expect.stringMatching(
        /^Problem: The certificate can't be renewed, and it is no longer valid since Sep 28, 2026, /,
      ),
    ]);
  });

  it('gives the whole fix of a failing renewal, without an address it doesn’t have', async () => {
    const alerts: Alert[] = [
      {
        code: 'tls.renewal_failing',
        severity: 'warn',
        params: { code: 'tls.rate_limited', notAfter: '2026-10-05T10:00:00.000Z' },
      },
      {
        code: 'tls.renewal_failing',
        severity: 'warn',
        params: { code: 'tls.dns_missing', notAfter: '2026-10-05T10:00:00.000Z' },
      },
    ];
    await openDashboard(
      withServer(
        { tls: { mode: 'auto', names: ['watch.example.com'], ready: true, notAfter: '2026-10-05T10:00:00.000Z' } },
        { alerts },
      ),
    );
    const items = within(screen.getByRole('region', { name: 'Alerts' })).getAllByRole('listitem');
    // The command that reads the log depends on how the server runs, which the dashboard doesn't know: the
    // sentence doesn't announce one.
    expect(items[0]).toHaveTextContent(
      "Let's Encrypt's rate limit was reached. If you reinstalled, restore certmagic/ from a backup. The server's " +
        'log says until when.',
    );
    expect(items[0]).not.toHaveTextContent(/until when:/);
    expect(within(items[0] as HTMLElement).queryByRole('button', { name: /copy/i })).not.toBeInTheDocument();
    expect(items[1]).toHaveTextContent("watch.example.com doesn't resolve. Add an A record pointing to 203.0.113.7.");
  });

  it('says "this server" in a renewal’s fix when the server has no public IPv4 address', async () => {
    const alerts: Alert[] = [
      {
        code: 'tls.renewal_failing',
        severity: 'warn',
        params: { code: 'tls.dns_missing', notAfter: '2026-10-05T10:00:00.000Z' },
      },
    ];
    await openDashboard(
      withServer(
        {
          tls: { mode: 'auto', names: ['watch.example.com'], ready: true, notAfter: '2026-10-05T10:00:00.000Z' },
          publicIpv4: '',
        },
        { alerts },
      ),
    );
    const item = within(screen.getByRole('region', { name: 'Alerts' })).getByRole('listitem');
    expect(item).toHaveTextContent("watch.example.com doesn't resolve. Add an A record pointing to this server.");
    expect(item).not.toHaveTextContent(/pointing to \./);
  });

  it('shows the server’s status as not available when its source failed, not its zero values', async () => {
    await openDashboard(withServer(ZERO_SERVER, { alerts: sourceFailed('server') }));
    const server = screen.getByRole('region', { name: 'Server' });
    expect(server).toHaveTextContent('Version0.3.0');
    expect(server).toHaveTextContent('187 MB of memory');
    expect(server).toHaveTextContent('Not available right now');
    for (const zero of [
      'Started',
      'years ago',
      'Certificate',
      'Not ready',
      'No public IPv4 address',
      'NAT',
      'None advertised',
    ]) {
      expect(server).not.toHaveTextContent(zero);
    }
  });

  it('shows the transfer as not available when its source failed', async () => {
    const transfer = { month: '', egressBytes: 0, ingressBytes: 0, alertGb: 0, egressBps: 0, ingressBps: 0 };
    await openDashboard(() => opsDashboard({ transfer, alerts: sourceFailed('transfer') }));
    const stats = screen.getByRole('region', { name: 'Right now' });
    expect(within(stats).getByText('Sending now').nextElementSibling).toHaveTextContent('Not available right now');
    expect(within(stats).getByText('Sent this month').nextElementSibling).toHaveTextContent('Not available right now');
    for (const zero of ['0 MB', '0 kb/s', 'Receiving', 'Sent in ', 'No transfer alert set']) {
      expect(stats).not.toHaveTextContent(zero);
    }
    // The other stats are there.
    expect(within(stats).getByText('People online').nextElementSibling).toHaveTextContent('2');
  });

  it('shows the rooms, the media, the process and doctor as not available when their sources failed', async () => {
    const d = opsDashboard();
    await openDashboard(() => ({
      ...d,
      server: { ...d.server, process: { cpuSeconds: 0, rssBytes: 0, numCpu: 0, goroutines: 0 } },
      media: { ingressBps: 0, egressBps: 0, downTracks: 0, peerConnections: { udp: 0, tcp443: 0, tcp7882: 0 } },
      rooms: [],
      clients: [],
      doctor: undefined,
      alerts: sourceFailed('process', 'rooms', 'media', 'doctor'),
    }));
    const stats = screen.getByRole('region', { name: 'Right now' });
    expect(within(stats).getByText('People online').nextElementSibling).toHaveTextContent('Not available right now');
    expect(within(stats).getByText('Live shares').nextElementSibling).toHaveTextContent('Not available right now');
    expect(stats).not.toHaveTextContent(/connections|Media:/);
    expect(within(stats).getByText('Sending now').nextElementSibling).toHaveTextContent('41.5 Mb/s');

    const rooms = screen.getByRole('region', { name: 'Live rooms' });
    expect(rooms).toHaveTextContent('Not available right now');
    expect(rooms).not.toHaveTextContent('Nobody is in a room right now.');
    const clients = screen.getByRole('region', { name: 'App versions' });
    expect(clients).toHaveTextContent('Not available right now');
    expect(clients).not.toHaveTextContent('Nobody is connected.');
    const doctor = screen.getByRole('region', { name: 'Doctor' });
    expect(doctor).toHaveTextContent('Not available right now');
    expect(doctor).not.toHaveTextContent("Doctor hasn't run yet.");
    expect(within(doctor).getByRole('link', { name: 'Open Doctor' })).toBeInTheDocument();
    const server = screen.getByRole('region', { name: 'Server' });
    expect(server).not.toHaveTextContent(/Process|CPU seconds/);
    expect(server).toHaveTextContent('Addresshttps://203.0.113.7');
  });

  it('names the NAT, the apps and their systems, and a connection without media yet', async () => {
    const d = opsDashboard();
    const [lounge] = d.rooms;
    if (lounge === undefined) throw new Error('no lounge');
    const [alex, sam] = lounge.participants;
    if (alex === undefined || sam === undefined) throw new Error('no alex or sam');
    const waiting = { ...alex, connections: alex.connections.map((c) => ({ ...without(c, 'transport'), rttMs: 0 })) };
    await openDashboard(() => ({
      ...d,
      server: { ...d.server, nat: 'cgnat_likely' },
      rooms: [{ ...lounge, participants: [waiting, sam] }],
      clients: [
        { kind: 'web', version: '0.3.0', count: 1, outdated: false },
        { kind: 'robot', version: '1.0.0', count: 1, outdated: false },
      ],
    }));
    expect(screen.getByRole('region', { name: 'Server' })).toHaveTextContent(
      'NAT: carrier-grade NAT likely, no public address',
    );
    const row = within(screen.getByRole('table', { name: 'People in Lounge' })).getByRole('row', { name: /^alex/ });
    expect(row).toHaveTextContent('Web 0.3.0 on macOS no media yet');
    expect(row).not.toHaveTextContent(/ms|over/);
    const clients = screen.getByRole('region', { name: 'App versions' });
    expect(within(clients).getByRole('row', { name: /^Web/ })).toBeInTheDocument();
    // A kind this build doesn't know shows as it is.
    expect(within(clients).getByRole('row', { name: /^robot/ })).toBeInTheDocument();
  });

  it('shows the setup checklist until the wizard is done', async () => {
    await openDashboard(undefined, false);
    const card = await screen.findByRole('region', { name: 'Finish setting up' });
    expect(within(card).getByRole('link', { name: 'Test the connection' })).toHaveAttribute(
      'href',
      '/admin/welcome?step=2',
    );
    expect(within(card).getByRole('link', { name: 'Invite friends' })).toHaveAttribute('href', '/admin/welcome?step=3');
    expect(within(card).getByRole('link', { name: 'Continue the setup' })).toHaveAttribute(
      'href',
      '/admin/welcome?step=2',
    );
  });

  it('shows no checklist once the wizard is done', async () => {
    await openDashboard();
    await screen.findByRole('region', { name: 'Server' });
    expect(screen.queryByRole('region', { name: 'Finish setting up' })).not.toBeInTheDocument();
  });

  it('polls every 2 s while the page is visible, stops while it is hidden, and asks at once when it is back', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] });
    const { gets } = await openDashboard();
    await until(() => gets.length === 1);

    await vi.advanceTimersByTimeAsync(DASHBOARD_POLL_MS);
    await until(() => gets.length === 2);
    await vi.advanceTimersByTimeAsync(DASHBOARD_POLL_MS);
    await until(() => gets.length === 3);

    setVisibility('hidden');
    await vi.advanceTimersByTimeAsync(10 * DASHBOARD_POLL_MS);
    // Time for a request that would have started.
    await new Promise((resolve) => {
      globalThis.setTimeout(resolve, 100);
    });
    expect(gets).toHaveLength(3);

    setVisibility('visible');
    await until(() => gets.length === 4);
    await vi.advanceTimersByTimeAsync(DASHBOARD_POLL_MS);
    await until(() => gets.length === 5);
  });

  it('keeps the last snapshot when a poll fails, and says it may be old', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] });
    const { gets } = await openDashboard();
    expect(screen.getByText('Updated')).toBeInTheDocument();
    on('get', '/api/v1/admin/dashboard', () => apiError(500, { code: 'internal', requestId: 'r1' }));
    await vi.advanceTimersByTimeAsync(DASHBOARD_POLL_MS);
    expect(await screen.findByText(/^Couldn't update: these numbers may be old\./)).toBeInTheDocument();
    expect(screen.getByRole('article', { name: 'Lounge' })).toBeInTheDocument();
    expect(gets).toHaveLength(1);

    // The next good answer brings the live line back.
    const later = on('get', '/api/v1/admin/dashboard', () => HttpResponse.json(opsDashboard()));
    await vi.advanceTimersByTimeAsync(DASHBOARD_POLL_MS);
    await until(() => later.length === 1);
    expect(await screen.findByText('Updated')).toBeInTheDocument();
    expect(screen.queryByText(/^Couldn't update/)).not.toBeInTheDocument();
  });

  it('answers a member with NotFound: the dashboard is the admins’', async () => {
    const gets = dashboardApi();
    renderAdmin({ path: '/admin', me: meFixture() });
    expect(await screen.findByRole('heading', { name: 'Nothing here' })).toBeInTheDocument();
    expect(gets).toEqual([]);
  });
});
