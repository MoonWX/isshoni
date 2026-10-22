// tokens.ts (docs/m2/03 §5.8, §7.3, §22.3): the origin in every call, the cache and its 60 s renewal, one call for
// concurrent callers, rejected(), and auth.state for its own origin only.
import { describe, expect, it } from 'vitest';

import { authState, createFakeBridge } from './testing/fakeBridge';
import { createTokens, TokenRenewBeforeMs } from './tokens';

const A = 'https://a.example.com';
const B = 'https://b.example.com';

function setup(active = A) {
  let clock = 1_800_000_000_000;
  const fake = createFakeBridge({ active });
  fake.now = () => clock;
  const tokens = createTokens({ origin: A, bridge: fake.bridge, now: () => clock });
  return {
    fake,
    tokens,
    advance: (ms: number) => {
      clock += ms;
    },
  };
}

describe('tokens', () => {
  it('passes its origin in every call, with force as asked', async () => {
    const { fake, tokens } = setup();
    expect(await tokens.get()).toBe('isa_fake0001');
    expect(await tokens.get(true)).toBe('isa_fake0002');
    expect(fake.callsOf('auth.accessToken')).toEqual([
      [A, false],
      [A, true],
    ]);
  });

  it('caches the token until 60 s before expiresAt, then asks again', async () => {
    const { fake, tokens, advance } = setup();
    await tokens.get();
    advance(fake.tokenTtlMs - TokenRenewBeforeMs - 1);
    await tokens.get();
    expect(fake.callsOf('auth.accessToken')).toHaveLength(1);
    advance(1);
    await tokens.get();
    expect(fake.callsOf('auth.accessToken')).toHaveLength(2);
  });

  it('shares one call among concurrent callers', async () => {
    const { fake, tokens } = setup();
    const got = await Promise.all([tokens.get(), tokens.get(), tokens.get()]);
    expect(got).toEqual(['isa_fake0001', 'isa_fake0001', 'isa_fake0001']);
    expect(fake.callsOf('auth.accessToken')).toHaveLength(1);
    await Promise.all([tokens.get(true), tokens.get(true), tokens.get()]);
    // The joined forced call's answer is cached: the next plain call asks nothing.
    expect(await tokens.get()).toBe('isa_fake0002');
    expect(fake.callsOf('auth.accessToken')).toEqual([
      [A, false],
      [A, true],
    ]);
  });

  it('two requests refused with the same token share one forced call, whose answer is cached', async () => {
    const { fake, tokens } = setup();
    const t = (await tokens.get()) ?? '';
    tokens.rejected(t);
    const first = tokens.get(true);
    tokens.rejected(t); // the cache no longer holds it: nothing to do
    const second = tokens.get(true);
    expect(await Promise.all([first, second])).toEqual(['isa_fake0002', 'isa_fake0002']);
    expect(await tokens.get()).toBe('isa_fake0002');
    expect(fake.callsOf('auth.accessToken')).toEqual([
      [A, false],
      [A, true],
    ]);
  });

  it('answers null for a server that is not active, and caches no null', async () => {
    const { fake, tokens } = setup(B);
    expect(await tokens.get()).toBeNull();
    expect(await tokens.get()).toBeNull();
    expect(fake.callsOf('auth.accessToken')).toEqual([
      [A, false],
      [A, false],
    ]);
  });

  it('rejected(token) clears the cache when it holds that token, and the next call forces', async () => {
    const { fake, tokens } = setup();
    const t = await tokens.get();
    tokens.rejected('isa_other');
    expect(await tokens.get()).toBe(t);
    expect(fake.callsOf('auth.accessToken')).toHaveLength(1);
    tokens.rejected(t ?? '');
    expect(await tokens.get()).toBe('isa_fake0002');
    expect(fake.callsOf('auth.accessToken')).toEqual([
      [A, false],
      [A, true],
    ]);
  });

  it('ignores an auth.state for another origin', async () => {
    const { fake, tokens } = setup();
    await tokens.get();
    fake.emit('auth.state', authState(B, 'unlinked'));
    await tokens.get();
    expect(fake.callsOf('auth.accessToken')).toHaveLength(1);
  });

  it.each(['unlinked', 'offline', 'incompatible', 'linking'] as const)(
    'clears its cache when its own origin leaves linked (%s)',
    async (status) => {
      const { fake, tokens } = setup();
      await tokens.get();
      fake.emit('auth.state', authState(A, 'linked'));
      await tokens.get();
      expect(fake.callsOf('auth.accessToken')).toHaveLength(1);
      fake.emit('auth.state', authState(A, status));
      await tokens.get();
      expect(fake.callsOf('auth.accessToken')).toHaveLength(2);
    },
  );

  it('does not let an older answer refill a cache that a forced call cleared', async () => {
    const { fake, tokens } = setup();
    let release: (() => void) | undefined;
    const slow = new Promise<void>((r) => {
      release = r;
    });
    fake.answers['auth.accessToken'] = async () => {
      await slow;
      return { token: 'isa_old', expiresAt: Date.now() + 900_000 };
    };
    const plain = tokens.get();
    delete fake.answers['auth.accessToken'];
    const forced = await tokens.get(true);
    release?.();
    expect(await plain).toBe('isa_old');
    expect(await tokens.get()).toBe(forced);
  });

  it('dispose stops listening', () => {
    const { fake, tokens } = setup();
    expect(fake.listeners('auth.state')).toBe(1);
    tokens.dispose();
    expect(fake.listeners('auth.state')).toBe(0);
  });
});
