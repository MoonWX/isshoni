// Compile-time checks between the Platform interface and the modules it describes structurally (npm run typecheck).
import { describe, expectTypeOf, it } from 'vitest';
import type { SignalClient } from '../protocol';
import type { SignalClientLike } from './types';

describe('Platform types', () => {
  it("S20's SignalClient fits ShareContext.signal (SignalClientLike)", () => {
    expectTypeOf<SignalClient>().toExtend<SignalClientLike>();
  });
});
