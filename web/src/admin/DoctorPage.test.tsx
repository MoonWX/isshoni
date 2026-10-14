// /admin/doctor (05 §15.3, 04 §13) against MSW: the last report (or none yet), "Run doctor" with doctor_busy, the
// checks rendered from the catalog, the connection test and the bandwidth calculator.
import { screen, waitFor, within } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import type { DoctorReport } from '../protocol/api.gen';
import { apiError } from '../test/msw';
import {
  bandwidthEstimate,
  doctorCheck,
  doctorReport,
  gate,
  on,
  renderAdmin,
  serve,
  typeInto,
} from './testing/harness';

/** A development server's report (the e2e config): public_ip skipped, a UDP warning, the ports local only. */
const DEV_REPORT: DoctorReport = doctorReport(
  [
    doctorCheck(),
    doctorCheck({ id: 'public_ip', status: 'skip', code: 'public_ip.loopback', params: undefined }),
    doctorCheck({
      id: 'udp_buffers',
      status: 'warn',
      code: 'udp_buffers.low',
      params: { rmemMax: 212992, wmemMax: 212992, want: 8388608 },
      fixCode: 'udp_buffers.fix_sysctl',
    }),
    doctorCheck({
      id: 'ports',
      status: 'info',
      code: 'ports.listening',
      params: { listening: ['18080/tcp', '17882/udp'] },
      localOnly: true,
    }),
    doctorCheck({
      id: 'future',
      status: 'fail',
      code: 'future.broken',
      message: 'something new broke',
      fixCode: 'future.fix_it',
      fix: 'do the new thing',
    }),
  ],
  { env: { ...doctorReport([]).env, provider: 'unknown' } },
);

function doctorApi(report: DoctorReport | null) {
  const gets = on('get', '/api/v1/admin/doctor', () =>
    report === null ? apiError(404, { code: 'not_found' }) : HttpResponse.json(report),
  );
  const bandwidth = serve('/api/v1/admin/bandwidth', () => bandwidthEstimate());
  return { gets, bandwidth };
}

async function openDoctor(report: DoctorReport | null) {
  const api = doctorApi(report);
  const page = renderAdmin({ path: '/admin/doctor' });
  await screen.findByRole('heading', { level: 1, name: 'Doctor' });
  return { ...api, ...page };
}

const checks = () => screen.getByRole('list', { name: 'Checks' });
const check = (name: string) => within(checks()).getByRole('listitem', { name });

describe('DoctorPage: the report', () => {
  it('says that there is no report yet before the first run (404), and runs doctor on request', async () => {
    const { user } = await openDoctor(null);
    expect(
      await screen.findByText(
        'No report yet. The server runs doctor 20 seconds after it starts, or you can run it now.',
      ),
    ).toBeInTheDocument();
    const running = gate();
    const posts = on('post', '/api/v1/admin/doctor', async () => {
      await running.opened;
      return HttpResponse.json(DEV_REPORT);
    });
    await user.click(screen.getByRole('button', { name: 'Run doctor' }));
    expect(await screen.findByText('Running every check. This can take up to two minutes.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Run doctor' })).toHaveAttribute('aria-busy', 'true');
    running.open();
    expect(await screen.findByRole('list', { name: 'Checks' })).toBeInTheDocument();
    expect(posts).toEqual([expect.objectContaining({ url: '/api/v1/admin/doctor', body: {} })]);
    expect(screen.getByRole('button', { name: 'Run doctor again' })).toBeEnabled();
  });

  it('renders each check from the catalog, with its status, its fix and its links', async () => {
    await openDoctor(DEV_REPORT);
    await screen.findByRole('list', { name: 'Checks' });
    expect(screen.getByText('Ran')).toHaveTextContent('on isshoni 0.3.0');
    const summary = screen.getByText('1 problem').parentElement as HTMLElement;
    expect(summary).toHaveTextContent('1 problem1 warning1 ok1 note1 not checked');

    // README F04: the development server's public_ip is skipped, with its own text and nothing to fix.
    const publicIp = check('Public IP');
    expect(publicIp).toHaveTextContent('Not checked');
    expect(publicIp).toHaveTextContent(
      'Not checked: network.include_loopback is on and network.stun_servers is empty (a development setup, which ' +
        'needs no public address).',
    );
    expect(within(publicIp).queryByText('Fix')).not.toBeInTheDocument();
    expect(within(publicIp).queryByRole('link')).not.toBeInTheDocument();

    const udp = check('UDP buffers');
    expect(udp).toHaveTextContent('Warning');
    expect(udp).toHaveTextContent('net.core.rmem_max is 212992, isshoni wants 8388608.');
    expect(udp).toHaveTextContent('Raise the limits:');
    expect(within(udp).getByText(/^printf 'net\.core\.rmem_max=8388608/)).toBeInTheDocument();
    expect(within(udp).getByRole('button', { name: 'Copy' })).toBeInTheDocument();
    expect(within(udp).getByRole('link', { name: 'More about this check' })).toHaveAttribute(
      'href',
      'https://moonwx.github.io/isshoni/troubleshooting#doctor-udp_buffers',
    );

    // "local only" checks are labeled.
    const ports = check('Ports');
    expect(within(ports).getByText('This machine only')).toBeInTheDocument();
    expect(ports).toHaveTextContent('Listening on 18080/tcp and 17882/udp.');
    expect(ports).toHaveTextContent('This only checks this machine. The connection test below checks from outside.');
    expect(within(check('Config')).queryByText('This machine only')).not.toBeInTheDocument();

    // A check this build doesn't know (a newer server's): its id, and the server's English.
    const future = check('future');
    expect(future).toHaveTextContent('something new broke');
    expect(future).toHaveTextContent('do the new thing');
  });

  it('keeps the report while doctor is busy, and offers the button again after the wait', async () => {
    const { user } = await openDoctor(DEV_REPORT);
    await screen.findByRole('list', { name: 'Checks' });
    on('post', '/api/v1/admin/doctor', () => apiError(429, { code: 'doctor_busy', retryAfter: 1 }));
    await user.click(screen.getByRole('button', { name: 'Run doctor again' }));
    expect(await screen.findByText('Doctor ran a moment ago. You can run it again in 1 second.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Run doctor again' })).toBeDisabled();
    expect(checks()).toBeInTheDocument();
    await waitFor(
      () => {
        expect(screen.getByRole('button', { name: 'Run doctor again' })).toBeEnabled();
      },
      { timeout: 3000 },
    );
    expect(screen.queryByText(/^Doctor ran a moment ago/)).not.toBeInTheDocument();
  });

  it('says why a run failed', async () => {
    const { user } = await openDoctor(DEV_REPORT);
    await screen.findByRole('list', { name: 'Checks' });
    on('post', '/api/v1/admin/doctor', () => apiError(503, { code: 'server_shutdown', retryAfter: 5 }));
    await user.click(screen.getByRole('button', { name: 'Run doctor again' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/^Doctor couldn't run\./);
    expect(checks()).toBeInTheDocument();
  });

  it('has the connection test', async () => {
    await openDoctor(DEV_REPORT);
    const panel = await screen.findByRole('region', { name: 'Connection test' });
    expect(within(panel).getByRole('button', { name: 'Test my connection' })).toBeInTheDocument();
  });
});

describe('DoctorPage: the bandwidth calculator', () => {
  const calculator = () => screen.getByRole('region', { name: 'Bandwidth calculator' });

  it('starts with the default session and shows the server’s estimate', async () => {
    const { bandwidth } = await openDoctor(DEV_REPORT);
    await waitFor(() => {
      expect(calculator()).toHaveTextContent('5 people, 2 sharing, for 2 hours:');
    });
    expect(bandwidth[0]?.url).toBe(
      '/api/v1/admin/bandwidth?people=5&sharing=2&thumbnails=8&quality=1080p60&preset=auto&hours=2',
    );
    const result = within(calculator()).getByRole('status');
    expect(result).toHaveTextContent('The server sends41.54 Mbps43.62 Mbps on the wire');
    expect(result).toHaveTextContent('Per session39.3 GB');
    expect(result).toHaveTextContent('To each person8.43 Mbps8.13 Mbps to someone who shares');
    expect(result).toHaveTextContent('The server receives16.86 Mbps');
  });

  it('asks again with what the admin typed', async () => {
    const { user, bandwidth } = await openDoctor(DEV_REPORT);
    await waitFor(() => {
      expect(bandwidth).toHaveLength(1);
    });
    const form = within(calculator());
    await typeInto(user, form.getByLabelText('People'), '10');
    await typeInto(user, form.getByLabelText('Sharing at once'), '10');
    await user.selectOptions(form.getByLabelText('Quality'), '1440p60');
    await user.selectOptions(form.getByLabelText('Preset'), 'movie');
    await typeInto(user, form.getByLabelText('Hours'), '1.5');
    await user.click(form.getByRole('button', { name: 'Calculate' }));
    await waitFor(() => {
      expect(bandwidth).toHaveLength(2);
    });
    expect(bandwidth[1]?.url).toBe(
      '/api/v1/admin/bandwidth?people=10&sharing=10&thumbnails=8&quality=1440p60&preset=movie&hours=1.5',
    );
  });

  it('checks the ranges before asking', async () => {
    const { user, bandwidth } = await openDoctor(DEV_REPORT);
    await waitFor(() => {
      expect(bandwidth).toHaveLength(1);
    });
    const form = within(calculator());
    await typeInto(user, form.getByLabelText('People'), '3');
    await typeInto(user, form.getByLabelText('Sharing at once'), '4');
    await typeInto(user, form.getByLabelText('Hours'), '0');
    await user.click(form.getByRole('button', { name: 'Calculate' }));
    expect(form.getByLabelText('Sharing at once')).toHaveAccessibleDescription('From 0 to the number of people.');
    expect(form.getByLabelText('Hours')).toHaveAccessibleDescription('More than 0, up to 8784.');
    expect(form.getByLabelText('People')).not.toHaveAttribute('aria-invalid');
    expect(form.getByLabelText('Sharing at once')).toHaveFocus();
    expect(bandwidth).toHaveLength(1);
  });

  it('marks the field the server refuses', async () => {
    const { user } = await openDoctor(DEV_REPORT);
    await screen.findByRole('region', { name: 'Bandwidth calculator' });
    on('get', '/api/v1/admin/bandwidth', () => apiError(400, { code: 'bad_request', params: { field: 'thumbnails' } }));
    const form = within(calculator());
    await typeInto(user, form.getByLabelText('Thumbnails per person'), '9');
    await user.click(form.getByRole('button', { name: 'Calculate' }));
    await waitFor(() => {
      expect(form.getByLabelText('Thumbnails per person')).toHaveAccessibleDescription('From 0 to 100.');
    });
  });
});
