// The signaling protocol layer of docs/m1/01-protocol.md §16, in one import: the generated wire types
// (types.gen.ts) and message maps (registry.gen.ts), codecs.ts, errors.ts and signal-client.ts. The REST DTOs of
// api.gen.ts come as the namespace api: some of their names (Error, Role, Empty, …) clash with the wire types.
// 05's rest.ts, queryKeys.ts and invalidate.ts are not re-exported here; they import what they need directly.
export * from './types.gen';
export * from './registry.gen';
export * as api from './api.gen';
export * from './codecs';
export * from './errors';
export * from './signal-client';
