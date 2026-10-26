// @vitest-environment node
// appLinkFor (docs/m2/02-device-auth.md §10.2, §12.1) against the deep-link cases that Go's deeplink tests use too
// (internal/client/deeplink/testdata/links.json): every `build` case, and every valid `parse` case's origin and room
// must give the case's link, as deeplink.Build does.
import { readFile } from 'node:fs/promises';

import { beforeAll, describe, expect, it } from 'vitest';

import { APP_LINK_SCHEME, appLinkFor, validRoomId } from './appLink';

const LINKS_JSON = new URL('../../../internal/client/deeplink/testdata/links.json', import.meta.url);

interface BuildCase {
  name: string;
  origin: string;
  roomId: string;
  link: string;
}

interface ParseCase {
  name: string;
  ok: boolean;
  origin?: string;
  roomId?: string;
  built?: string;
}

interface LinksFile {
  build: BuildCase[];
  parse: ParseCase[];
}

let links: LinksFile;

beforeAll(async () => {
  links = JSON.parse(await readFile(LINKS_JSON, 'utf8')) as LinksFile;
});

describe('appLinkFor: links.json', () => {
  it('has the cases', () => {
    expect(links.build.length).toBeGreaterThanOrEqual(10);
    expect(links.parse.filter((c) => c.ok).length).toBeGreaterThanOrEqual(10);
  });

  it('builds every build case as deeplink.Build does', () => {
    const got = links.build.map((c) => ({
      name: c.name,
      link: appLinkFor(c.origin, c.roomId === '' ? undefined : c.roomId),
    }));
    expect(got).toEqual(links.build.map((c) => ({ name: c.name, link: c.link })));
  });

  it('builds the link of every valid parse case from its origin and room', () => {
    const valid = links.parse.filter((c) => c.ok);
    const got = valid.map((c) => ({ name: c.name, link: appLinkFor(c.origin ?? '', c.roomId) }));
    expect(got).toEqual(valid.map((c) => ({ name: c.name, link: c.built })));
  });
});

describe('appLinkFor', () => {
  it('uses the scheme the app registers', () => {
    expect(APP_LINK_SCHEME).toBe('isshoni');
    expect(appLinkFor('https://watch.example.com')).toBe('isshoni://watch.example.com/');
  });

  it('treats an empty room id as none', () => {
    expect(appLinkFor('https://watch.example.com', '')).toBe('isshoni://watch.example.com/');
  });

  it.each([
    ['an IDN in Unicode', 'https://bücher.example', 'isshoni://xn--bcher-kva.example/'],
    ['surrounding white space', '  https://watch.example.com  ', 'isshoni://watch.example.com/'],
    ['no scheme (https, as in Go)', 'watch.example.com:8443', 'isshoni://watch.example.com:8443/'],
    ['an upper-case IPv6 literal', 'https://[2001:DB8::0:1]', 'isshoni://[2001:db8::1]/'],
  ])('canonicalizes %s', (_name, origin, link) => {
    expect(appLinkFor(origin)).toBe(link);
  });

  it.each([
    ['a path', 'https://watch.example.com/link'],
    ['an empty query', 'https://watch.example.com/?'],
    ['an empty fragment', 'https://watch.example.com#'],
    ['userinfo', 'https://user@watch.example.com'],
    ['an empty userinfo', 'https://@watch.example.com'],
    ['port 0', 'https://watch.example.com:0'],
    ['port 65536', 'https://watch.example.com:65536'],
    ['a port with a leading zero', 'https://watch.example.com:08443'],
    ['an empty port', 'https://watch.example.com:'],
    ['a trailing dot', 'https://watch.example.com.'],
    ['a numeric last label', 'https://127.1'],
    ['a hex last label', 'https://a.0x7f'],
    ['a fullwidth IPv4 address', 'https://１２７.０.０.１'],
    ['the unspecified IPv4 address', 'https://0.0.0.0'],
    ['the unspecified IPv6 address', 'https://[::]'],
    ['an IPv4 multicast address', 'https://224.0.0.1'],
    ['an IPv6 multicast address', 'https://[ff02::1]'],
    ['an IPv4-mapped multicast address', 'https://[::ffff:224.0.0.1]'],
    ['an IPv6 literal without its bracket', 'https://[2001:db8::1'],
    ['a space inside', 'https://watch example.com'],
    ['a bidi control', 'https://watch.example.com‮'],
    ['a websocket origin', 'wss://watch.example.com'],
    ['more than 2048 bytes', `https://${'a'.repeat(2050)}.example`],
  ])('has no link for %s', (_name, origin) => {
    expect(appLinkFor(origin, 'lounge')).toBe('');
  });
});

describe('validRoomId', () => {
  it.each([
    ['lounge', true],
    ['k3m9p2qxw7ht', true],
    ['a'.repeat(64), true],
    ['a'.repeat(65), false],
    ['', false],
    ['lounge.x', false],
    ['lounge/x', false],
    ['löunge', false],
  ])('%s → %s', (id, want) => {
    expect(validRoomId(id)).toBe(want);
  });
});
