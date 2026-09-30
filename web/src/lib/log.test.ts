import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { clearLog, createLogger, logLines, maskAddresses, MAX_LOG_LINES, sanitizeAttrs, setConsoleLevel } from './log';

describe('in-memory log (05 §10.7)', () => {
  beforeEach(() => {
    clearLog();
  });
  afterEach(() => {
    setConsoleLevel('off');
    vi.restoreAllMocks();
  });

  it('keeps lines with level, component and attributes, oldest first', () => {
    const log = createLogger('room');
    log.info('joined', { roomId: 'lounge' });
    log.child('share').warn('slow');
    expect(logLines()).toMatchObject([
      { level: 'info', component: 'room', msg: 'joined', attrs: { roomId: 'lounge' } },
      { level: 'warn', component: 'room/share', msg: 'slow' },
    ]);
    expect(logLines()[1]).not.toHaveProperty('attrs');
  });

  it(`keeps the last ${String(MAX_LOG_LINES)} lines`, () => {
    const log = createLogger('t');
    for (let i = 0; i < MAX_LOG_LINES + 20; i++) log.debug(`line ${String(i)}`);
    const lines = logLines();
    expect(lines).toHaveLength(MAX_LOG_LINES);
    expect(lines[0]?.msg).toBe('line 20');
    expect(lines.at(-1)?.msg).toBe(`line ${String(MAX_LOG_LINES + 19)}`);
  });

  it('never keeps SDP, candidates, tokens or passwords (05 §20)', () => {
    const attrs = sanitizeAttrs({
      sdp: 'v=0\r\n…',
      candidate: 'candidate:1 1 udp 2122260223 192.0.2.10 54321 typ host',
      resumeToken: 'abc',
      inviteToken: 'def',
      password: 'hunter22',
      nested: { authorization: 'Bearer x', ok: 1 },
      pushEndpoint: 'https://push.example/abc',
      shareId: 's_1',
    });
    expect(attrs).toEqual({
      sdp: '[redacted]',
      candidate: '[redacted]',
      resumeToken: '[redacted]',
      inviteToken: '[redacted]',
      password: '[redacted]',
      nested: { authorization: '[redacted]', ok: 1 },
      pushEndpoint: '[redacted]',
      shareId: 's_1',
    });
  });

  it('masks IP addresses in messages and values', () => {
    const log = createLogger('pc');
    log.warn('ICE failed via 192.0.2.10:7882 and [2001:db8::1]:443', { remote: 'fe80::1%en0', at: '12:30:45' });
    const [line] = logLines();
    expect(line?.msg).toBe('ICE failed via [ip]:7882 and [[ip]]:443');
    expect(line?.attrs).toEqual({ remote: '[ip]%en0', at: '12:30:45' });
  });

  it.each([
    ['203.0.113.7', '[ip]'],
    ['::1', '[ip]'],
    ['2001:db8:0:0:0:0:0:1', '[ip]'],
    ['at 12:30:45', 'at 12:30:45'],
    ['std::vector', 'std::vector'],
    ['::ffff:192.0.2.1', '[ip][ip]'],
    ['a:b', 'a:b'],
    ['version 1.2.3', 'version 1.2.3'],
  ])('maskAddresses(%s) → %s', (text, want) => {
    expect(maskAddresses(text)).toBe(want);
  });

  it('stores errors as name and message, and cuts deep nesting', () => {
    const attrs = sanitizeAttrs({ error: new TypeError('bad 10.0.0.1'), deep: { a: { b: { c: { d: 1 } } } } });
    expect(attrs).toEqual({ error: { name: 'TypeError', message: 'bad [ip]' }, deep: { a: { b: '[…]' } } });
  });

  it('writes to the console only at or above the console level', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const logFn = vi.spyOn(console, 'log').mockImplementation(() => undefined);
    setConsoleLevel('warn');
    const log = createLogger('c');
    log.info('quiet');
    log.warn('loud', { n: 1 });
    expect(logFn).not.toHaveBeenCalled();
    expect(warn).toHaveBeenCalledWith('[c] loud', { n: 1 });
  });
});
