// An app's name for people (docs/m2/02-device-auth.md §10.1): link.app.<clientKind>.<os>, "isshoni for Windows".
// The approval page and the Devices page's app rows (account/DevicesPage.tsx) both name apps this way; their folders
// load the `link` namespace (i18n/lazy/link.ts).
import type { TFunction } from 'i18next';

/** The apps that have a name of their own: clientKind → the OSes (02 §5.2's values). */
const NAMED: Readonly<Record<string, readonly string[]>> = {
  desktop: ['windows', 'macos', 'linux'],
  mobile: ['ios', 'android'],
};

/** "isshoni for Windows"; "An isshoni app" for a kind and OS without a name (a newer app than this page). */
export function appName(t: TFunction, clientKind: string, os: string | undefined): string {
  if (os !== undefined && NAMED[clientKind]?.includes(os) === true) return t(`link.app.${clientKind}.${os}`);
  return t('link.app.other');
}

/** "isshoni for Windows 0.2.0"; the name alone when the version is unknown. */
export function appWithVersion(
  t: TFunction,
  clientKind: string,
  os: string | undefined,
  version: string | undefined,
): string {
  const app = appName(t, clientKind, os);
  return version === undefined || version === '' ? app : t('link.app.withVersion', { app, version });
}
