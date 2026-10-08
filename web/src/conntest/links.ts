// Links from the connection test into the project site (05 §14.2, 06 §10.4's anchor contract).
//
// The result codes below are the `ct-` codes: each has a section `/troubleshooting#ct-<code>` on the site. The same
// list is exported as codes.json, a JSON array of the bare codes (no `ct-` prefix) in this order, which 06's anchor
// test (TestDocsAnchors) reads; links.test.ts keeps the two in step. Codes are never renamed or reused: a site built
// from an older release must keep its anchors working.
import {
  CloudProviderAlibaba,
  CloudProviderAWS,
  CloudProviderAzure,
  CloudProviderDigitalOcean,
  CloudProviderGCP,
  CloudProviderHetzner,
  CloudProviderLinode,
  CloudProviderOracle,
  CloudProviderOVH,
  CloudProviderScaleway,
  CloudProviderTencent,
  CloudProviderUnknown,
  CloudProviderVultr,
  type CloudProvider,
} from '../protocol/api.gen';

export const CT_CODES = [
  /** UDP ✗ while a TCP transport works. */
  'udp_blocked',
  /** Every transport ✗. */
  'no_media',
  /** Every transport ✗ and the server doesn't know its public IPv4 (used instead of no_media). */
  'no_public_ip',
  /** TCP 443 ✗ while UDP works. */
  'tcp443_blocked',
  /** Round trip above 200 ms. */
  'high_rtt',
  /** The server is behind a home router that forwards ports. */
  'nat_port_forward',
  /** The server has no public address (carrier-grade or symmetric NAT). */
  'nat_cgnat',
  /** A Mac's browser needs Local Network access to reach a server on the same network. */
  'mac_local_network',
  /** A containerized server must publish its media ports. */
  'docker_ports',
] as const;

export type CtCode = (typeof CT_CODES)[number];

/** The project site (06 D11); the same value as Go's version.DocsURL (04 §15). Ends in "/". */
export const DOCS_URL = 'https://moonwx.github.io/isshoni/';

/** The troubleshooting section of a result code. */
export function troubleshootingUrl(code: CtCode): string {
  return `${DOCS_URL}troubleshooting#ct-${code}`;
}

/**
 * Every CloudProvider id of 04 §13.3. A Record, so the build fails here when `task gen` brings a new provider: it
 * then needs its fix.firewall.<provider> text (check:i18n says so too) and its install/vps section (06).
 */
const PROVIDERS: Readonly<Record<CloudProvider, true>> = {
  [CloudProviderAWS]: true,
  [CloudProviderGCP]: true,
  [CloudProviderAzure]: true,
  [CloudProviderOracle]: true,
  [CloudProviderHetzner]: true,
  [CloudProviderDigitalOcean]: true,
  [CloudProviderVultr]: true,
  [CloudProviderLinode]: true,
  [CloudProviderScaleway]: true,
  [CloudProviderOVH]: true,
  [CloudProviderAlibaba]: true,
  [CloudProviderTencent]: true,
  [CloudProviderUnknown]: true,
};

/** The provider ids this build has fix text for, in 04 §13.3's order. */
export const CLOUD_PROVIDERS = Object.keys(PROVIDERS) as readonly CloudProvider[];

/** A provider id from the server as one this build knows: a newer server's unknown id becomes "unknown". */
export function knownProvider(id: string): CloudProvider {
  return Object.hasOwn(PROVIDERS, id) ? (id as CloudProvider) : CloudProviderUnknown;
}

/** The install guide's section for a provider: where its firewall is and which ports to open. */
export function vpsGuideUrl(provider: CloudProvider): string {
  return `${DOCS_URL}install/vps#${provider}`;
}
