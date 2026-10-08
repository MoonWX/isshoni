// verdict.ts (05 §14.2, §19.1): rows, the status table, "not tested" probes, the round trip and the result codes.
import { describe, expect, it } from 'vitest';

import { probeVerdict, resultFixture, type ProbeShort } from './testing';
import { DEFAULT_MEDIA_PORT, rowState, rttLabelOf, verdictOf, type ConnTestStatus } from './verdict';

describe('rowState', () => {
  it.each([
    ['ok', 'ok'],
    ['timeout', 'failed'],
    ['failed', 'failed'],
    ['disabled', 'off'],
    ['server', 'not_tested'],
    ['rate_limited', 'not_tested'],
  ] as const)('%s → %s', (short, want) => {
    expect(rowState(probeVerdict('udp', short))).toBe(want);
  });

  it('a transport without a verdict is not tested', () => {
    expect(rowState(undefined)).toBe('not_tested');
  });
});

describe('verdictOf: the status table', () => {
  // | UDP | any TCP | status |
  const table: [ProbeShort, ProbeShort, ProbeShort, ConnTestStatus][] = [
    ['ok', 'ok', 'ok', 'green'],
    ['ok', 'timeout', 'ok', 'green'],
    ['ok', 'timeout', 'timeout', 'green'],
    ['ok', 'disabled', 'disabled', 'green'],
    ['timeout', 'ok', 'ok', 'amber'],
    ['timeout', 'ok', 'timeout', 'amber'],
    ['timeout', 'timeout', 'ok', 'amber'],
    ['failed', 'disabled', 'ok', 'amber'],
    ['timeout', 'timeout', 'timeout', 'red'],
    ['failed', 'failed', 'failed', 'red'],
    ['timeout', 'disabled', 'timeout', 'red'],
    ['timeout', 'disabled', 'disabled', 'red'],
  ];
  it.each(table)('UDP %s, TCP 443 %s, TCP 7882 %s → %s', (udp, tcp443, tcp7882, want) => {
    const v = verdictOf(resultFixture({ udp, tcp443, tcp7882 }));
    expect(v.status).toBe(want);
    expect(v.incomplete).toBe(false);
  });
});

describe('verdictOf: rows', () => {
  it('shows the three transports with the ports the server gave', () => {
    const v = verdictOf(resultFixture({ udp: 'ok', tcp443: 'timeout', tcp7882: 'ok' }));
    expect(v.rows).toEqual([
      { transport: 'udp', state: 'ok', port: 7882 },
      { transport: 'tcp443', state: 'failed', port: 443 },
      { transport: 'tcp7882', state: 'ok', port: 7882 },
    ]);
    expect(v.ports).toEqual({ udp: 7882, tcp: 7882 });
  });

  it('uses changed ports (listen.ice_udp, listen.ice_tcp)', () => {
    const v = verdictOf(resultFixture({ server: { udpPort: 5000, tcpPorts: [443, 5001] } }));
    expect(v.rows.map((r) => r.port)).toEqual([5000, 443, 5001]);
    expect(v.ports).toEqual({ udp: 5000, tcp: 5001 });
  });

  it('falls back to 7882 when the server did not say', () => {
    const v = verdictOf(resultFixture({ udp: 'disabled', tcp443: 'disabled', tcp7882: 'disabled', server: null }));
    expect(v.ports).toEqual({ udp: DEFAULT_MEDIA_PORT, tcp: DEFAULT_MEDIA_PORT });
    expect(v.rows).toEqual([{ transport: 'udp', state: 'off' }]);
  });

  it('hides a disabled TCP row', () => {
    const v = verdictOf(resultFixture({ tcp443: 'disabled', server: { tcpPorts: [7882] } }));
    expect(v.rows.map((r) => r.transport)).toEqual(['udp', 'tcp7882']);
    expect(v.status).toBe('green');
    expect(v.codes).toEqual([]);
  });

  it('shows a disabled UDP row as ✗ ("off", without a port) and counts it as UDP ✗', () => {
    const amber = verdictOf(resultFixture({ udp: 'disabled', server: { udpPort: 0 } }));
    expect(amber.rows[0]).toEqual({ transport: 'udp', state: 'off' });
    expect(amber.udp).toBe('off');
    expect(amber.status).toBe('amber');
    // UDP is off, not blocked: the firewall section of the site would mislead.
    expect(amber.codes).toEqual([]);

    const red = verdictOf(resultFixture({ udp: 'disabled', tcp443: 'disabled', tcp7882: 'timeout' }));
    expect(red.status).toBe('red');
    expect(red.codes).toEqual(['no_media']);
  });
});

describe('verdictOf: probes that were not tested', () => {
  it.each(['server', 'rate_limited'] as const)('%s: neither ✓ nor ✗, and the result is incomplete', (short) => {
    const v = verdictOf(resultFixture({ tcp443: short }));
    expect(v.rows[1]).toEqual({ transport: 'tcp443', state: 'not_tested', port: 443 });
    expect(v.incomplete).toBe(true);
  });

  it('is left out of the table: a status is given only when the untested probe cannot change it', () => {
    // Green needs only UDP ✓.
    expect(verdictOf(resultFixture({ tcp443: 'server', tcp7882: 'rate_limited' })).status).toBe('green');
    // Amber needs UDP ✗ and one TCP ✓.
    expect(verdictOf(resultFixture({ udp: 'timeout', tcp443: 'server', tcp7882: 'ok' })).status).toBe('amber');
    // Red would claim that nothing works; the untested TCP probe might.
    const open = verdictOf(resultFixture({ udp: 'timeout', tcp443: 'rate_limited', tcp7882: 'timeout' }));
    expect(open.status).toBeNull();
    expect(open.codes).toEqual([]);
    expect(open.noPublicIp).toBe(false);
    // Without the UDP probe there is no status at all, whatever TCP did.
    expect(verdictOf(resultFixture({ udp: 'rate_limited' })).status).toBeNull();
    expect(verdictOf(resultFixture({ udp: 'server', tcp443: 'timeout', tcp7882: 'timeout' })).status).toBeNull();
  });

  it('nothing tested: no status, no codes, no round trip', () => {
    const v = verdictOf(
      resultFixture({
        udp: 'rate_limited',
        tcp443: 'rate_limited',
        tcp7882: 'server',
        server: null,
        retryAfterSec: 30,
      }),
    );
    expect(v).toMatchObject({ status: null, incomplete: true, codes: [], noPublicIp: false });
    expect(v.rows.map((r) => r.state)).toEqual(['not_tested', 'not_tested', 'not_tested']);
    expect('rttMs' in v).toBe(false);
  });
});

describe('verdictOf: the server does not know its public address', () => {
  it('every probe that ran failed → noPublicIp, and the code no_public_ip instead of no_media', () => {
    const v = verdictOf(
      resultFixture({
        udp: 'timeout',
        tcp443: 'disabled',
        tcp7882: 'timeout',
        server: { publicIpKnown: false, publicIpPrivate: true },
      }),
    );
    expect(v.status).toBe('red');
    expect(v.noPublicIp).toBe(true);
    expect(v.codes).toEqual(['no_public_ip']);
  });

  it('a known address → no_media', () => {
    const v = verdictOf(resultFixture({ udp: 'timeout', tcp443: 'timeout', tcp7882: 'timeout' }));
    expect(v.noPublicIp).toBe(false);
    expect(v.codes).toEqual(['no_media']);
  });

  it('a probe ✓ → the normal result (an IPv6-only server, or one reached on its own network)', () => {
    const server = { publicIpKnown: false, publicIpPrivate: true };
    const amber = verdictOf(resultFixture({ udp: 'timeout', server }));
    expect(amber).toMatchObject({ status: 'amber', noPublicIp: false, codes: ['udp_blocked'] });
    expect(verdictOf(resultFixture({ server }))).toMatchObject({ status: 'green', noPublicIp: false, codes: [] });
  });

  it('no probe ran at all (every transport off): not the address', () => {
    const v = verdictOf(
      resultFixture({ udp: 'disabled', tcp443: 'disabled', tcp7882: 'disabled', server: { publicIpKnown: false } }),
    );
    expect(v).toMatchObject({ status: 'red', noPublicIp: false, codes: ['no_media'] });
  });
});

describe('verdictOf: codes', () => {
  it('udp_blocked: UDP ✗ while TCP ✓', () => {
    expect(verdictOf(resultFixture({ udp: 'timeout' })).codes).toEqual(['udp_blocked']);
    expect(verdictOf(resultFixture({ udp: 'failed', tcp443: 'timeout' })).codes).toEqual(['udp_blocked']);
  });

  it('tcp443_blocked: TCP 443 ✗ while UDP ✓ (a green result)', () => {
    const v = verdictOf(resultFixture({ tcp443: 'timeout' }));
    expect(v.status).toBe('green');
    expect(v.codes).toEqual(['tcp443_blocked']);
    // Not when TCP 443 is off or was not tested.
    expect(verdictOf(resultFixture({ tcp443: 'disabled' })).codes).toEqual([]);
    expect(verdictOf(resultFixture({ tcp443: 'server' })).codes).toEqual([]);
  });

  it('high_rtt: above 200 ms, next to whatever else applies', () => {
    expect(verdictOf(resultFixture({ rtt: { udp: 200 } })).codes).toEqual([]);
    expect(verdictOf(resultFixture({ rtt: { udp: 200.1 } })).codes).toEqual(['high_rtt']);
    expect(verdictOf(resultFixture({ udp: 'timeout', rtt: { tcp443: 350, tcp7882: 340 } })).codes).toEqual([
      'udp_blocked',
      'high_rtt',
    ]);
  });
});

describe('verdictOf: round trip', () => {
  it("is the UDP probe's when UDP works, else the fastest working TCP probe's", () => {
    expect(verdictOf(resultFixture({ rtt: { udp: 24, tcp443: 9, tcp7882: 12 } }))).toMatchObject({
      rttMs: 24,
      rttLabel: 'good',
    });
    expect(verdictOf(resultFixture({ udp: 'timeout', rtt: { tcp443: 140, tcp7882: 95 } }))).toMatchObject({
      rttMs: 95,
      rttLabel: 'ok',
    });
    // UDP works but could not be measured: a TCP probe's value is better than none.
    expect(verdictOf(resultFixture({ rtt: { udp: undefined, tcp443: 40, tcp7882: 60 } })).rttMs).toBe(40);
  });

  it('is absent when nothing was measured', () => {
    const none = verdictOf(resultFixture({ udp: 'timeout', tcp443: 'timeout', tcp7882: 'timeout' }));
    expect('rttMs' in none).toBe(false);
    expect('rttLabel' in none).toBe(false);
    const unmeasured = verdictOf(resultFixture({ rtt: { udp: undefined, tcp443: undefined, tcp7882: undefined } }));
    expect('rttMs' in unmeasured).toBe(false);
  });

  it.each([
    [0, 'good'],
    [80, 'good'],
    [80.1, 'ok'],
    [200, 'ok'],
    [200.1, 'high'],
    [1500, 'high'],
  ] as const)('%d ms → %s', (ms, want) => {
    expect(rttLabelOf(ms)).toBe(want);
  });
});
