// The links of the /download page (docs/m2/06-windows-release.md §7.5, §7.6): which installer a friend gets, the
// PowerShell one-liner for this server, and the isshoni:// link that opens the app set up for it.
//
// A server knows the matching desktop version because it is its own version: one tag builds the server, the SPA and
// the app (06 D13, D14), so /api/v1/info's server.version names the release whose Windows asset matches. A dev, CI
// or e2e build has no such release and links to the latest one. Nothing here is fetched: the page builds every URL
// from server.version and server.publicUrl.

import { appLinkFor, validRoomId } from '../lib/appLink';
import type { ClientOS } from '../protocol/types.gen';

/** The project's GitHub releases (06 §17's URL patterns). */
export const RELEASES_URL = 'https://github.com/MoonWX/isshoni/releases';

/** The project site (06 D11) and the anchors of it that the page links to (06 §12.2, §13, §17 "Site anchors"). */
export const PROJECT_SITE_URL = 'https://moonwx.github.io/isshoni/';
export const SITE_LINKS = {
  /** The project site's own download page: every app, and the macOS and Linux ones once they come. */
  download: `${PROJECT_SITE_URL}download`,
  /** The two browser warnings and SmartScreen's "Windows protected your PC", with what to click and why. */
  smartScreen: `${PROJECT_SITE_URL}download#windows-smartscreen`,
  /** Why an unsigned 0.x build can't run where Smart App Control is on, and what to do instead. */
  smartAppControl: `${PROJECT_SITE_URL}download#windows-smart-app-control`,
  /** The one-liner: what it checks, the OpenSSH Client it needs, what each error means. */
  powerShell: `${PROJECT_SITE_URL}download#windows-powershell`,
  /** The GPU drivers isshoni needs for hardware encoding (04 §19). */
  gpuDriver: `${PROJECT_SITE_URL}troubleshooting#desktop-gpu-driver`,
  /** Windows N and KN editions: the Media Feature Pack (04 §19). */
  mediaFeaturePack: `${PROJECT_SITE_URL}troubleshooting#desktop-media-feature-pack`,
} as const;

/**
 * A release version as the server's /install-desktop.ps1 prefill takes it (06 §7.4): x.y.z or x.y.z-rc.N. Anything
 * else (0.0.0-dev, 0.0.0-ci.42, 0.0.0-e2e.local) is a build without a release of its own.
 */
const RELEASE_VERSION = /^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$/;

/**
 * What a fresh Windows PowerShell 5.1 needs before `irm` when it doesn't offer TLS 1.2 by default (06 §7.1 step 2,
 * §7.5): 3072 is [Net.SecurityProtocolType]::Tls12, the number keeps the line short.
 */
export const PS1_TLS12_PREFIX =
  '[Net.ServicePointManager]::SecurityProtocol=[Net.ServicePointManager]::SecurityProtocol -bor 3072; ';

/**
 * Whether the command carries PS1_TLS12_PREFIX: the form slice R10 (WA49) recorded, which line 1 of
 * deploy/install-desktop.ps1 shows too (links.node.test.ts reads it). Bare until the owner's measurement on a clean
 * Windows 10 22H2 (WA49-R4, README §8) finds TLS 1.2 off by default; then a WF switches the script's line 1, its
 * Pester test and this constant together.
 */
export const PS1_NEEDS_TLS12_PREFIX: boolean = false;

export interface DesktopLinks {
  /** server.version is a release version (06 §7.4's regex). */
  release: boolean;
  /** server.version. */
  version: string;
  /** The exact installer asset of that release, or the latest release's page. */
  windowsInstallerUrl: string;
  /** `irm <publicUrl>/install-desktop.ps1 | iex`, in the form PS1_NEEDS_TLS12_PREFIX says. */
  ps1Command: string;
  /** …/releases/tag/v<version>, or …/releases/latest. */
  releasePageUrl: string;
  /** appLinkFor(publicUrl, roomId) (02 §10.2, §12): "" unless publicUrl is https, and then the button is hidden. */
  openAppUrl: string;
}

/** The Windows installer's asset name (06 §2.1). */
export function windowsInstallerName(version: string): string {
  return `isshoni-desktop_${version}_windows_amd64_setup.exe`;
}

/** The PowerShell one-liner that installs the app set up for the server at `origin` (06 §7.5). */
export function ps1CommandFor(origin: string): string {
  const command = `irm ${origin.replace(/\/+$/, '')}/install-desktop.ps1 | iex`;
  return PS1_NEEDS_TLS12_PREFIX ? PS1_TLS12_PREFIX + command : command;
}

/**
 * The page's links for a server of version `serverVersion` at `publicUrl` (06 §7.5). `roomId` is the page's ?room=
 * (03 §12.3 links /download?room=<id>); one that breaks 02 §12's room-id rule gives the plain server link, as
 * appLinkFor does, and the page passes one only through roomParam.
 */
export function desktopLinks(serverVersion: string, publicUrl: string, roomId?: string): DesktopLinks {
  const release = RELEASE_VERSION.test(serverVersion);
  const latest = `${RELEASES_URL}/latest`;
  return {
    release,
    version: serverVersion,
    windowsInstallerUrl: release
      ? `${RELEASES_URL}/download/v${serverVersion}/${windowsInstallerName(serverVersion)}`
      : latest,
    ps1Command: ps1CommandFor(publicUrl),
    releasePageUrl: release ? `${RELEASES_URL}/tag/v${serverVersion}` : latest,
    openAppUrl: appLinkFor(publicUrl, roomId !== undefined && validRoomId(roomId) ? roomId : undefined),
  };
}

/**
 * The room of the page's query: the one ?room= value when it follows the room-id rule, else undefined (none, an
 * invalid one, or more than one, which names no room).
 */
export function roomParam(params: URLSearchParams): string | undefined {
  const rooms = params.getAll('room');
  return rooms.length === 1 && rooms[0] !== undefined && validRoomId(rooms[0]) ? rooms[0] : undefined;
}

/** The desktop platforms of the page, in its order (06 §7.5). */
export type DesktopOS = 'windows' | 'macos' | 'linux';

/**
 * The order of the page's platform sections: the browser's own first when it reports one of them (Windows first on
 * Windows), the rest as Windows, macOS, Linux. A phone, ChromeOS or an unknown OS gets that order too: Windows is
 * the app there is.
 */
export function desktopOrder(os: ClientOS): DesktopOS[] {
  const order: DesktopOS[] = ['windows', 'macos', 'linux'];
  const own = order.find((o) => o === os);
  return own === undefined ? order : [own, ...order.filter((o) => o !== own)];
}
