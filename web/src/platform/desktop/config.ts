// The desktop app's bridge.js config (docs/m2/03 §5.1, §5.2): `window.__ISSHONI_DESKTOP__`, a frozen object that the
// app's asset handler writes before the module entry runs (internal/desktop/assets.BridgeConfig). detectPlatform()
// reads it synchronously; only a config that passes readDesktopConfig makes the page a DesktopPlatform.
import type { ClientOS } from '../../protocol/types.gen';

export interface DesktopConfig {
  v: 1;
  /** "wails"; internal/desktop/e2ehost sets "http" (docs/m2/03 §7.4). */
  transport: 'wails' | 'http';
  app: { version: string; os: ClientOS; osBuild: number; arch: string };
  /** The active server; null before the first server is added. */
  server: { id: string; origin: string; name: string } | null;
  /** The derived id of every server in servers.json, for the storage cleanup (docs/m2/03 §14.3). */
  serverIds: string[];
  /** A dev or e2e build (never a release). */
  dev: boolean;
}

/** BridgeConfig.V. */
export const DesktopConfigVersion = 1;

/** The derived local id (docs/m2/03 §8.1): 8 characters of lowercase Crockford base32. */
const SERVER_ID = /^[0-9a-hjkmnp-tv-z]{8}$/;

const CLIENT_OS: readonly string[] = ['windows', 'macos', 'linux', 'ios', 'android', 'chromeos', 'other'];

/** http origins are allowed only in dev and e2e builds, and only on loopback (docs/m2/03 §5.3, §8.1). */
const LOOPBACK_HOSTS: readonly string[] = ['localhost', '127.0.0.1', '[::1]'];

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

const isString = (v: unknown): v is string => typeof v === 'string';
const isUint = (v: unknown): v is number => typeof v === 'number' && Number.isInteger(v) && v >= 0;

/** A server origin as Go normalizes it: scheme, host and optional port only. */
function isServerOrigin(v: unknown, dev: boolean): v is string {
  if (!isString(v)) return false;
  let u: URL;
  try {
    u = new URL(v);
  } catch {
    return false;
  }
  if (`${u.protocol}//${u.host}` !== v) return false;
  if (u.protocol === 'https:') return true;
  return u.protocol === 'http:' && dev && LOOPBACK_HOSTS.includes(u.hostname);
}

function readApp(v: unknown): DesktopConfig['app'] | null {
  if (!isRecord(v)) return null;
  const { version, os, osBuild, arch } = v;
  if (!isString(version) || version === '' || !isString(os) || !CLIENT_OS.includes(os)) return null;
  if (!isUint(osBuild) || !isString(arch)) return null;
  return { version, os: os as ClientOS, osBuild, arch };
}

function readServer(v: unknown, dev: boolean): DesktopConfig['server'] | undefined {
  if (v === null) return null;
  if (!isRecord(v)) return undefined;
  const { id, origin, name } = v;
  if (!isString(id) || !SERVER_ID.test(id) || !isServerOrigin(origin, dev) || !isString(name)) return undefined;
  return { id, origin, name };
}

/**
 * Validates bridge.js's config: v must be 1 and every field present with its type (server may be null). Anything
 * else (no config in a browser, a wrong version, a missing field, a field of another type) is null, and the page is a
 * BrowserPlatform. The result is a fresh copy, never the global itself.
 */
export function readDesktopConfig(raw: unknown): DesktopConfig | null {
  if (!isRecord(raw) || raw['v'] !== DesktopConfigVersion) return null;
  const { transport, dev, serverIds } = raw;
  if ((transport !== 'wails' && transport !== 'http') || typeof dev !== 'boolean') return null;
  const app = readApp(raw['app']);
  const server = readServer(raw['server'], dev);
  if (app === null || server === undefined) return null;
  if (!Array.isArray(serverIds) || !serverIds.every((id): id is string => isString(id) && SERVER_ID.test(id))) {
    return null;
  }
  return { v: DesktopConfigVersion, transport, app, server, serverIds: [...serverIds], dev };
}
