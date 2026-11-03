// The /download page's links (docs/m2/06-windows-release.md §7.5, §7.6, §16 "Unit"): the installer of the server's
// own release or the latest release, the PowerShell one-liner in the form slice R10 (WA49) recorded, and the
// isshoni:// link through appLinkFor.
import { describe, expect, it } from 'vitest';

import {
  desktopLinks,
  desktopOrder,
  PROJECT_SITE_URL,
  ps1CommandFor,
  PS1_NEEDS_TLS12_PREFIX,
  PS1_TLS12_PREFIX,
  RELEASES_URL,
  roomParam,
  SITE_LINKS,
  windowsInstallerName,
} from './links';

const ORIGIN = 'https://watch.example.com';
const ROOM = 'k3j9x2m4p7q8';

/** 06 §7.5's two exact commands for ORIGIN; WA49 recorded the bare one (PS1_NEEDS_TLS12_PREFIX false). */
const BARE_COMMAND = 'irm https://watch.example.com/install-desktop.ps1 | iex';
const PREFIXED_COMMAND =
  '[Net.ServicePointManager]::SecurityProtocol=[Net.ServicePointManager]::SecurityProtocol -bor 3072; ' +
  'irm https://watch.example.com/install-desktop.ps1 | iex';

describe('desktopLinks', () => {
  it('links a release server to its own release: the exact installer and its tag page', () => {
    const links = desktopLinks('0.2.0', ORIGIN);
    expect(links.release).toBe(true);
    expect(links.version).toBe('0.2.0');
    expect(links.windowsInstallerUrl).toBe(
      'https://github.com/MoonWX/isshoni/releases/download/v0.2.0/isshoni-desktop_0.2.0_windows_amd64_setup.exe',
    );
    expect(links.releasePageUrl).toBe('https://github.com/MoonWX/isshoni/releases/tag/v0.2.0');
  });

  it('takes a release candidate as a release', () => {
    const links = desktopLinks('0.2.0-rc.1', ORIGIN);
    expect(links.release).toBe(true);
    expect(links.windowsInstallerUrl).toBe(
      'https://github.com/MoonWX/isshoni/releases/download/v0.2.0-rc.1/isshoni-desktop_0.2.0-rc.1_windows_amd64_setup.exe',
    );
    expect(links.releasePageUrl).toBe('https://github.com/MoonWX/isshoni/releases/tag/v0.2.0-rc.1');
  });

  it.each([
    ['a dev build', '0.0.0-dev'],
    ['a CI build', '0.0.0-ci.42'],
    ['the e2e build', '0.0.0-e2e.local'],
    ['an empty version', ''],
    ['a v prefix', 'v0.2.0'],
    ['another pre-release', '0.2.0-beta.1'],
    ['two parts', '0.2'],
  ])('links %s to the latest release', (_name, version) => {
    const links = desktopLinks(version, ORIGIN);
    expect(links.release).toBe(false);
    expect(links.version).toBe(version);
    expect(links.windowsInstallerUrl).toBe('https://github.com/MoonWX/isshoni/releases/latest');
    expect(links.releasePageUrl).toBe('https://github.com/MoonWX/isshoni/releases/latest');
  });

  it('gives the PowerShell command with the server origin, exactly in the form WA49 recorded', () => {
    expect(PS1_NEEDS_TLS12_PREFIX).toBe(false);
    expect(desktopLinks('0.2.0', ORIGIN).ps1Command).toBe(BARE_COMMAND);
    expect(desktopLinks('0.0.0-dev', ORIGIN).ps1Command).toBe(BARE_COMMAND);
    expect(ps1CommandFor(`${ORIGIN}/`)).toBe(BARE_COMMAND);
    expect(ps1CommandFor('http://127.0.0.1:8080')).toBe('irm http://127.0.0.1:8080/install-desktop.ps1 | iex');
    // The other form, the day a WF switches PS1_NEEDS_TLS12_PREFIX: the prefix is 06 §7.5's exactly.
    expect(PS1_TLS12_PREFIX + BARE_COMMAND).toBe(PREFIXED_COMMAND);
  });

  it('opens the app through appLinkFor: https only, with a valid room', () => {
    expect(desktopLinks('0.2.0', ORIGIN).openAppUrl).toBe('isshoni://watch.example.com/');
    expect(desktopLinks('0.2.0', ORIGIN, ROOM).openAppUrl).toBe(`isshoni://watch.example.com/r/${ROOM}`);
    // A dev server on http://localhost has no link (02 §12.1): the page hides the button.
    expect(desktopLinks('0.0.0-dev', 'http://localhost:5173').openAppUrl).toBe('');
    expect(desktopLinks('0.0.0-dev', 'http://localhost').openAppUrl).toBe('');
    expect(desktopLinks('0.0.0-dev', 'http://localhost', ROOM).openAppUrl).toBe('');
  });

  it.each([
    ['too long (65 characters)', 'a'.repeat(65)],
    ['a dot', 'k3j9.x2m4'],
    ['a slash', `${ROOM}/x`],
    ['a space', 'k3j9 x2m4'],
    ['not ASCII', 'k3j9x2m4é'],
    ['empty', ''],
  ])('gives the plain link for a room id with %s', (_name, room) => {
    expect(desktopLinks('0.2.0', ORIGIN, room).openAppUrl).toBe('isshoni://watch.example.com/');
  });

  it('takes a room id of 64 characters', () => {
    const room = 'A_-9'.repeat(16);
    expect(desktopLinks('0.2.0', ORIGIN, room).openAppUrl).toBe(`isshoni://watch.example.com/r/${room}`);
  });

  it('names the installer as 06 §2.1 does', () => {
    expect(windowsInstallerName('0.2.0')).toBe('isshoni-desktop_0.2.0_windows_amd64_setup.exe');
    expect(RELEASES_URL).toBe('https://github.com/MoonWX/isshoni/releases');
  });
});

describe('roomParam', () => {
  it.each([
    ['one valid room', `?room=${ROOM}`, ROOM],
    ['a valid room among other parameters', `?x=1&room=${ROOM}`, ROOM],
    ['no room', '', undefined],
    ['an empty room', '?room=', undefined],
    ['a room that is too long', `?room=${'a'.repeat(65)}`, undefined],
    ['a room with other characters', '?room=k3j9x2m4p7q%21', undefined],
    ['a second value', `?room=${ROOM}&room=${ROOM}`, undefined],
    ['a second, different value', `?room=${ROOM}&room=lounge`, undefined],
  ])('reads %s', (_name, search, room) => {
    expect(roomParam(new URLSearchParams(search))).toBe(room);
  });

  it('feeds desktopLinks the room, and an invalid ?room= gives the plain link', () => {
    const link = (search: string): string =>
      desktopLinks('0.2.0', ORIGIN, roomParam(new URLSearchParams(search))).openAppUrl;
    expect(link(`?room=${ROOM}`)).toBe(`isshoni://watch.example.com/r/${ROOM}`);
    expect(link('?room=no.pe')).toBe('isshoni://watch.example.com/');
    expect(link(`?room=${ROOM}&room=${ROOM}`)).toBe('isshoni://watch.example.com/');
    expect(link('')).toBe('isshoni://watch.example.com/');
  });
});

describe('the project site', () => {
  it("links 06 §17's site anchors", () => {
    expect(PROJECT_SITE_URL).toBe('https://moonwx.github.io/isshoni/');
    expect(SITE_LINKS).toEqual({
      download: 'https://moonwx.github.io/isshoni/download',
      smartScreen: 'https://moonwx.github.io/isshoni/download#windows-smartscreen',
      smartAppControl: 'https://moonwx.github.io/isshoni/download#windows-smart-app-control',
      powerShell: 'https://moonwx.github.io/isshoni/download#windows-powershell',
      gpuDriver: 'https://moonwx.github.io/isshoni/troubleshooting#desktop-gpu-driver',
      mediaFeaturePack: 'https://moonwx.github.io/isshoni/troubleshooting#desktop-media-feature-pack',
    });
  });
});

describe('desktopOrder', () => {
  it.each([
    ['windows', ['windows', 'macos', 'linux']],
    ['macos', ['macos', 'windows', 'linux']],
    ['linux', ['linux', 'windows', 'macos']],
    ['android', ['windows', 'macos', 'linux']],
    ['ios', ['windows', 'macos', 'linux']],
    ['chromeos', ['windows', 'macos', 'linux']],
    ['other', ['windows', 'macos', 'linux']],
  ] as const)('puts %s first when it is a desktop', (os, order) => {
    expect(desktopOrder(os)).toEqual(order);
  });
});
