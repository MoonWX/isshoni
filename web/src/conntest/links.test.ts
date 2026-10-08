// The connection test's links into the project site (05 §14.2, 06 §10.4): codes.json lists the `ct-` codes that
// 06's anchor test reads, and every code has its link text.
import { describe, expect, it } from 'vitest';

import en from '../i18n/en.json';
import codes from './codes.json';
import { CLOUD_PROVIDERS, CT_CODES, DOCS_URL, troubleshootingUrl, vpsGuideUrl } from './links';

describe('codes.json', () => {
  it('lists the ct- codes of 05 §14.2', () => {
    expect(codes).toEqual([
      'udp_blocked',
      'no_media',
      'no_public_ip',
      'tcp443_blocked',
      'high_rtt',
      'nat_port_forward',
      'nat_cgnat',
      'mac_local_network',
      'docker_ports',
    ]);
  });

  it('is the list the code uses: a JSON array of the bare codes, in the same order', () => {
    expect(Array.isArray(codes)).toBe(true);
    expect(codes).toEqual([...CT_CODES]);
    expect(new Set(codes).size).toBe(codes.length);
    for (const code of codes) {
      // An anchor id without escaping, and no `ct-` prefix of its own.
      expect(code).toMatch(/^[a-z][a-z0-9_]*$/);
    }
  });

  it('has a link text for every code, and no text for a code that does not exist', () => {
    const { title, ...texts } = en.conntest.help;
    expect(typeof title).toBe('string');
    expect(Object.keys(texts).sort()).toEqual([...CT_CODES].sort());
    for (const text of Object.values(texts)) expect(text).not.toBe('');
  });
});

describe('links', () => {
  it('point at the project site', () => {
    expect(DOCS_URL).toMatch(/^https:\/\/[a-z0-9.-]+\/isshoni\/$/);
    expect(troubleshootingUrl('udp_blocked')).toBe(`${DOCS_URL}troubleshooting#ct-udp_blocked`);
    expect(troubleshootingUrl('no_public_ip')).toBe(`${DOCS_URL}troubleshooting#ct-no_public_ip`);
    expect(vpsGuideUrl('hetzner')).toBe(`${DOCS_URL}install/vps#hetzner`);
  });

  it('every troubleshooting and provider link is a plain https URL with its anchor', () => {
    for (const code of CT_CODES) {
      const url = new URL(troubleshootingUrl(code));
      expect([url.protocol, url.pathname, url.hash]).toEqual(['https:', '/isshoni/troubleshooting', `#ct-${code}`]);
    }
    for (const provider of CLOUD_PROVIDERS) {
      const url = new URL(vpsGuideUrl(provider));
      expect([url.protocol, url.pathname, url.hash]).toEqual(['https:', '/isshoni/install/vps', `#${provider}`]);
    }
  });
});
