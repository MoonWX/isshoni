// registry.gen.ts (01 §14.4, §19): the generated message arrays agree with types.gen.ts and with the Go golden fixtures
// in internal/protocol/testdata/v1, imported with import.meta.glob so one fixture set serves both languages. The type
// maps are checked at compile time (npm run typecheck) with expectTypeOf.
import { describe, expect, expectTypeOf, it } from 'vitest';

import {
  clientNotificationTypes,
  clientRequestTypes,
  messageFeatures,
  messageTypes,
  requestReplies,
  serverMessageTypes,
  type ClientEnvelope,
  type ClientMessageType,
  type ClientNotifications,
  type ClientRequests,
  type ServerEnvelope,
  type ServerMessages,
} from './registry.gen';
import * as types from './types.gen';

const fixtureFiles = import.meta.glob<unknown>('../../../internal/protocol/testdata/v1/*.json', {
  eager: true,
  import: 'default',
});

interface Fixture {
  name: string; // the file name without .json: <type>[.<variant>], or ok.<request type>[.<variant>]
  type: string;
  id?: unknown;
  re?: unknown;
  data?: unknown;
}

const fixtures: Fixture[] = Object.entries(fixtureFiles).map(([path, json]) => {
  const name = (path.split('/').pop() ?? path).replace(/\.json$/, '');
  if (typeof json !== 'object' || json === null || !('type' in json) || typeof json.type !== 'string') {
    throw new Error(`${name}.json is not an envelope with a type`);
  }
  return { ...(json as Omit<Fixture, 'name'>), name };
});

/** The values of types.gen.ts's constants whose names start with prefix (tygo's <Type><Value> naming). */
function constants(prefix: string): string[] {
  return Object.entries(types)
    .filter(([name, value]) => name.startsWith(prefix) && typeof value === 'string')
    .map(([, value]) => value as string);
}

const has = (list: readonly string[], t: string) => list.includes(t);
const replyTypes = [...new Set<string>(Object.values(requestReplies)), types.MessageTypeError];

describe('registry.gen.ts', () => {
  it('lists every MessageType of types.gen.ts once, and nothing else', () => {
    expect(new Set(messageTypes).size).toBe(messageTypes.length);
    expect([...messageTypes].sort()).toEqual(constants('MessageType').sort());
  });

  it('splits the message types by direction and kind', () => {
    const client: string[] = [...clientRequestTypes, ...clientNotificationTypes];
    expect(new Set(client).size).toBe(client.length); // a client message is a request or a notification
    expect(new Set(serverMessageTypes).size).toBe(serverMessageTypes.length);
    expect(new Set([...client, ...serverMessageTypes])).toEqual(new Set(messageTypes));
    expect(Object.keys(requestReplies)).toEqual([...clientRequestTypes]);
    for (const reply of replyTypes) {
      expect(serverMessageTypes).toContain(reply);
    }
    expect(requestReplies.hello).toBe(types.MessageTypeWelcome);
  });

  it('names only known message types and features in messageFeatures', () => {
    const features = constants('Feature');
    for (const [type, feature] of Object.entries(messageFeatures)) {
      expect(messageTypes).toContain(type);
      expect(features).toContain(feature);
    }
    expect(messageFeatures['agent.send']).toBe(types.FeatureAgentRelay);
    expect(messageFeatures['room.join']).toBeUndefined();
  });
});

describe('Go golden fixtures (internal/protocol/testdata/v1)', () => {
  it('are found', () => {
    // 01 §14.3's minimum set has 44 files.
    expect(fixtures.length).toBeGreaterThanOrEqual(44);
  });

  it.each(fixtures)("$name: its type is in the generated arrays and matches the file's name", (f) => {
    expect(messageTypes).toContain(f.type);
    if (f.type === types.MessageTypeOK) {
      // ok.<request type>[.<variant>]: the longest request type that prefixes the rest of the name.
      const rest = f.name.slice('ok.'.length);
      const request = clientRequestTypes
        .filter((t) => rest === t || rest.startsWith(`${t}.`))
        .sort((a, b) => b.length - a.length)[0];
      expect(request, `${f.name}: want ok.<request type>[.<variant>]`).toBeDefined();
      expect(request && requestReplies[request]).toBe(types.MessageTypeOK);
    } else {
      expect(f.name === f.type || f.name.startsWith(`${f.type}.`), `${f.name}: the name starts with its type`).toBe(
        true,
      );
    }
  });

  it.each(fixtures)('$name: id only on requests, re only on replies', (f) => {
    if (f.id !== undefined) {
      expect(typeof f.id).toBe('string');
      expect(clientRequestTypes).toContain(f.type);
      expect(f.re).toBeUndefined();
    } else if (f.re !== undefined) {
      expect(typeof f.re).toBe('string');
      expect(replyTypes).toContain(f.type);
    } else {
      // A notification: error without re is one too (01 §8.13).
      expect(has(clientNotificationTypes, f.type) || has(serverMessageTypes, f.type), f.type).toBe(true);
      expect(f.type === types.MessageTypeOK || f.type === types.MessageTypeWelcome, 'a reply needs a re').toBe(false);
    }
  });

  it('cover every generated message type', () => {
    const covered = new Set(fixtures.map((f) => f.type));
    expect(messageTypes.filter((t) => !covered.has(t))).toEqual([]);
  });

  it('use only error codes and scopes that types.gen.ts knows', () => {
    const codes = constants('ErrorCode');
    const scopes = constants('ErrorScope');
    const errors = fixtures.filter((f) => f.type === types.MessageTypeError);
    expect(errors.length).toBeGreaterThan(0);
    for (const f of errors) {
      const data = f.data as { code?: unknown; scope?: unknown } | undefined;
      expect(codes, f.name).toContain(data?.code);
      expect(scopes, f.name).toContain(data?.scope);
    }
  });
});

// Compile-time checks: tsc (npm run typecheck) fails when the generated maps lose their shape; at run time these are
// no-ops.
describe('generated types', () => {
  it('map requests to payload, result and reply', () => {
    expectTypeOf<ClientRequests['hello']>().toEqualTypeOf<{
      data: types.Hello;
      result: types.Welcome;
      reply: 'welcome';
    }>();
    expectTypeOf<ClientRequests['room.join']>().toEqualTypeOf<{
      data: types.RoomJoin;
      result: types.RoomJoinResult;
      reply: 'ok';
    }>();
    expectTypeOf<ClientRequests['subscribe.update']['result']>().toEqualTypeOf<types.SubscribeResult>();
    expectTypeOf<ClientRequests['share.start']['result']>().toEqualTypeOf<types.ShareParams>();
  });

  it('map notifications and server messages to their payloads, per direction', () => {
    expectTypeOf<ClientNotifications['stats']>().toEqualTypeOf<types.ClientStats>();
    expectTypeOf<ServerMessages['stats']>().toEqualTypeOf<types.ServerStats>();
    expectTypeOf<ClientNotifications['pc.offer']>().toEqualTypeOf<types.PCOffer>();
    expectTypeOf<ServerMessages['pc.offer']>().toEqualTypeOf<types.PCOffer>();
    expectTypeOf<ServerMessages['room.state']>().toEqualTypeOf<types.RoomState>();
    expectTypeOf<ServerMessages['error']>().toEqualTypeOf<types.Error>();
    expectTypeOf<ServerMessages['welcome']>().toEqualTypeOf<types.Welcome>();
    expectTypeOf<types.RoomJoinResult>().toExtend<ServerMessages['ok']>();
    expectTypeOf<types.AgentSendResult>().toExtend<ServerMessages['ok']>();
  });

  it('discriminate envelopes by type', () => {
    expectTypeOf<ServerEnvelope<'room.state'>>().toEqualTypeOf<{
      type: 'room.state';
      re?: string;
      data: types.RoomState;
    }>();
    expectTypeOf<Extract<ServerEnvelope, { type: 'invalidate' }>['data']>().toEqualTypeOf<types.Invalidate>();
    expectTypeOf<ClientEnvelope<'room.join'>>().toEqualTypeOf<{
      type: 'room.join';
      id: string;
      data: types.RoomJoin;
    }>();
    expectTypeOf<ClientEnvelope<'ping'>>().toEqualTypeOf<{ type: 'ping'; data: types.Ping }>();
    expectTypeOf<ClientMessageType>().toExtend<types.MessageType>();
    expectTypeOf<(typeof serverMessageTypes)[number]>().toEqualTypeOf<keyof ServerMessages>();
  });
});
