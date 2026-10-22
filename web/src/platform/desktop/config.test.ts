// readDesktopConfig and detectPlatform (docs/m2/03 §7.2, §22.3): a wrong v, missing fields and fields of other types
// are refused; only a valid bridge.js config makes the page a DesktopPlatform.
import { afterEach, describe, expect, it } from 'vitest';

import { BrowserPlatform } from '../browser/BrowserPlatform';
import { detectPlatform } from '../detect';
import { readDesktopConfig, type DesktopConfig } from './config';
import { DesktopPlatform } from './DesktopPlatform';

/** What internal/desktop/assets.BridgeJS writes (docs/m2/03 §5.2's example). */
function valid(): Record<string, unknown> {
  return {
    v: 1,
    transport: 'wails',
    app: { version: '0.2.0', os: 'windows', osBuild: 26100, arch: 'amd64' },
    server: { id: 'j92qv4mz', origin: 'https://watch.example.com', name: 'Friends' },
    serverIds: ['j92qv4mz'],
    dev: false,
  };
}

/** valid() with one field replaced (undefined deletes it), at a dotted path. */
function withField(path: string, value: unknown): Record<string, unknown> {
  const cfg = valid();
  const keys = path.split('.');
  const last = keys.pop() ?? '';
  let obj = cfg;
  for (const k of keys) obj = obj[k] as Record<string, unknown>;
  if (value === undefined) Reflect.deleteProperty(obj, last);
  else obj[last] = value;
  return cfg;
}

describe('readDesktopConfig', () => {
  it('accepts bridge.js as the asset handler writes it, and returns a copy', () => {
    const raw = Object.freeze(valid());
    const cfg = readDesktopConfig(raw);
    expect(cfg).toEqual<DesktopConfig>({
      v: 1,
      transport: 'wails',
      app: { version: '0.2.0', os: 'windows', osBuild: 26100, arch: 'amd64' },
      server: { id: 'j92qv4mz', origin: 'https://watch.example.com', name: 'Friends' },
      serverIds: ['j92qv4mz'],
      dev: false,
    });
    expect(cfg?.serverIds).not.toBe(raw['serverIds']);
  });

  it('accepts no active server, the http transport and a server with a port', () => {
    expect(readDesktopConfig(withField('server', null))?.server).toBeNull();
    expect(readDesktopConfig(withField('serverIds', []))?.serverIds).toEqual([]);
    expect(readDesktopConfig(withField('transport', 'http'))?.transport).toBe('http');
    expect(readDesktopConfig(withField('server.origin', 'https://watch.example.com:8443'))?.server?.origin).toBe(
      'https://watch.example.com:8443',
    );
    expect(readDesktopConfig(withField('app.os', 'macos'))?.app.os).toBe('macos');
  });

  it('accepts an http loopback server only in a dev or e2e build', () => {
    const loopback = { id: 'tc55ev03', origin: 'http://127.0.0.1:8080', name: 'Dev' };
    expect(readDesktopConfig({ ...valid(), server: loopback, dev: true })?.server).toEqual(loopback);
    expect(
      readDesktopConfig({ ...valid(), server: { ...loopback, origin: 'http://localhost:8080' }, dev: true }),
    ).not.toBeNull();
    expect(readDesktopConfig({ ...valid(), server: loopback, dev: false })).toBeNull();
    expect(
      readDesktopConfig({ ...valid(), server: { ...loopback, origin: 'http://watch.example.com' }, dev: true }),
    ).toBeNull();
  });

  it.each([
    ['no config (a browser)', undefined],
    ['null', null],
    ['a string', '{"v":1}'],
    ['an array', [valid()]],
  ])('refuses %s', (_name, raw) => {
    expect(readDesktopConfig(raw)).toBeNull();
  });

  it.each([
    ['v 2', 'v', 2],
    ['v "1"', 'v', '1'],
    ['no v', 'v', undefined],
  ])('refuses a wrong version: %s', (_name, path, value) => {
    expect(readDesktopConfig(withField(path, value))).toBeNull();
  });

  it.each(
    ['transport', 'app', 'server', 'serverIds', 'dev', 'app.version', 'app.os', 'app.osBuild', 'app.arch'].map((p) => [
      p,
    ]),
  )('refuses a config without %s', (path) => {
    expect(readDesktopConfig(withField(path, undefined))).toBeNull();
  });

  it.each([
    ['transport', 'ipc'],
    ['transport', 1],
    ['dev', 'false'],
    ['app', 'windows'],
    ['app.version', ''],
    ['app.version', 2],
    ['app.os', 'win32'],
    ['app.osBuild', '26100'],
    ['app.osBuild', -1],
    ['app.osBuild', 1.5],
    ['app.arch', null],
    ['server', 'https://watch.example.com'],
    ['server', []],
    ['server.id', 'J92QV4MZ'],
    ['server.id', 'j92qv4m'],
    ['server.id', 'j92qv4mzu'],
    ['server.id', 'j92qv4mu'],
    ['server.origin', 'https://watch.example.com/'],
    ['server.origin', 'https://watch.example.com/r/lounge'],
    ['server.origin', 'wss://watch.example.com'],
    ['server.origin', 'watch.example.com'],
    ['server.origin', 42],
    ['server.name', null],
    ['serverIds', 'j92qv4mz'],
    ['serverIds', [1]],
    ['serverIds', ['../evil']],
    ['serverIds', null],
  ])('refuses %s = %j', (path, value) => {
    expect(readDesktopConfig(withField(path, value))).toBeNull();
  });
});

describe('detectPlatform', () => {
  let made: DesktopPlatform | undefined;
  afterEach(() => {
    made?.dispose();
    made = undefined;
    Reflect.deleteProperty(window, '__ISSHONI_DESKTOP__');
  });

  it('picks DesktopPlatform with a valid config', () => {
    window.__ISSHONI_DESKTOP__ = Object.freeze(valid());
    const p = detectPlatform();
    expect(p).toBeInstanceOf(DesktopPlatform);
    made = p as DesktopPlatform;
    expect(p.kind).toBe('desktop');
    expect(p.serverOrigin).toBe('https://watch.example.com');
    expect(p.desktop?.server?.id).toBe('j92qv4mz');
  });

  it.each([
    ['no config', undefined],
    ['a wrong version', { ...valid(), v: 2 }],
    ['a missing field', withField('app', undefined)],
    ['a field of another type', withField('dev', 1)],
  ])('picks BrowserPlatform with %s', (_name, raw) => {
    if (raw !== undefined) window.__ISSHONI_DESKTOP__ = raw;
    const p = detectPlatform();
    expect(p).toBeInstanceOf(BrowserPlatform);
    expect(p.desktop ?? null).toBeNull();
  });
});
