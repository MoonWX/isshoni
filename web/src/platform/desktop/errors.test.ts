// Bridge errors (docs/m2/03 §6.4): the rejection's message is the JSON {code, params}.
import { describe, expect, it } from 'vitest';

import { BridgeError, bridgeErrorCode, bridgeErrorFromJSON, toBridgeError } from './errors';

describe('bridge errors', () => {
  it('parses a Go error whose Error() is the JSON', () => {
    const err = toBridgeError(new Error('{"code":"incompatible_server","params":{"verdict":"update_app"}}'));
    expect(err).toBeInstanceOf(BridgeError);
    expect(err.code).toBe('incompatible_server');
    expect(err.params).toEqual({ verdict: 'update_app' });
    expect(toBridgeError('{"code":"not_found"}')).toMatchObject({ code: 'not_found', params: {} });
  });

  it('keeps a BridgeError and turns anything else into internal, with the cause', () => {
    const be = new BridgeError('busy');
    expect(toBridgeError(be)).toBe(be);
    const cause = new TypeError('Failed to fetch');
    const err = toBridgeError(cause);
    expect(err.code).toBe('internal');
    expect(err.cause).toBe(cause);
    expect(toBridgeError(new Error('{"params":{}}')).code).toBe('internal');
    expect(toBridgeError(42).code).toBe('internal');
  });

  it('reads {code, params} only from an object with a code', () => {
    expect(bridgeErrorFromJSON({ code: 'busy', params: [] })).toMatchObject({ code: 'busy', params: {} });
    expect(bridgeErrorFromJSON({ code: '' })).toBeNull();
    expect(bridgeErrorFromJSON(['busy'])).toBeNull();
    expect(bridgeErrorFromJSON(null)).toBeNull();
  });

  it('bridgeErrorCode reads the code of a BridgeError only', () => {
    expect(bridgeErrorCode(new BridgeError('not_found'))).toBe('not_found');
    expect(bridgeErrorCode(new Error('not_found'))).toBeUndefined();
  });
});
