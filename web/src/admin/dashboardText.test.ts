// dashboardText.ts (05 §15.3, 04 §11.4): the NAT kind's name on the dashboard. README F22: a symmetric NAT that the
// server marks dockerBridge is a container bridge's, which doctor's public_ip lets pass (04 §13.2), and reads as
// that; every other kind, and a symmetric NAT without the flag, reads as before. The documents are the server's own
// golden files (internal/protocol/api/testdata), so the text follows what the wire carries.
import { describe, expect, it } from 'vitest';

import { i18n } from '../i18n';
import type { NATKind, OpsDashboard } from '../protocol/api.gen';
import { natName } from './dashboardText';

const t = i18n.t.bind(i18n);

const GOLDEN_DIR = '../../../internal/protocol/api/testdata/';
const goldens = import.meta.glob<OpsDashboard>('../../../internal/protocol/api/testdata/OpsDashboard*.json', {
  eager: true,
  import: 'default',
});

/** testdata/<name>.json: a dashboard document as the server encodes it. */
function golden(name: string): OpsDashboard {
  const doc = goldens[`${GOLDEN_DIR}${name}.json`];
  if (!doc) throw new Error(`no golden ${name}.json`);
  return doc;
}

const SYMMETRIC = "symmetric, friends probably can't connect";
const BRIDGE = "symmetric in the container's bridge network, which doesn't matter: friends reach the published ports";

describe('natName', () => {
  it("names a container bridge's symmetric NAT as such, not as one that friends can't get through", () => {
    const { server } = golden('OpsDashboard.docker-bridge');
    expect(server).toMatchObject({ nat: 'symmetric', dockerBridge: true });
    expect(natName(server, t)).toBe(BRIDGE);
    expect(t('admin.dashboard.server.nat', { nat: natName(server, t) })).toBe(`NAT: ${BRIDGE}`);
  });

  it('keeps the symmetric name without the flag', () => {
    expect(natName({ nat: 'symmetric' }, t)).toBe(SYMMETRIC);
    expect(natName({ nat: 'symmetric', dockerBridge: false }, t)).toBe(SYMMETRIC);
  });

  // The rule lets only symmetric NAT pass, so the flag never qualifies another kind.
  it.each([
    ['none', 'none'],
    ['one_to_one', '1:1, a cloud network or a container'],
    ['port_forward', 'a router forwards the ports'],
    ['cgnat_likely', 'carrier-grade NAT likely, no public address'],
    ['unknown', 'unknown'],
  ] as const)('names %s as before, with or without the flag', (nat, name) => {
    expect(natName({ nat }, t)).toBe(name);
    expect(natName({ nat, dockerBridge: true }, t)).toBe(name);
  });

  it("shows a kind this build doesn't know as it is", () => {
    expect(natName({ nat: 'double' as NATKind, dockerBridge: true }, t)).toBe('double');
  });

  it('reads the documents without the key as before', () => {
    for (const [name, nat] of [
      ['OpsDashboard', 'none'],
      ['OpsDashboard.source-failed', 'unknown'],
    ] as const) {
      const { server } = golden(name);
      expect(Object.hasOwn(server, 'dockerBridge'), name).toBe(false);
      expect(natName(server, t), name).toBe(nat);
    }
  });
});
