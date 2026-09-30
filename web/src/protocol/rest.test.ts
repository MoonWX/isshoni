import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';

import { apiError, apiPath, server } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import {
  ApiError,
  api,
  configureApi,
  isRetryableError,
  isSignedOutError,
  NetworkError,
  parseApiError,
  parseRetryAfter,
} from './rest';

const json = (v: unknown) => JSON.stringify(v);
const noHeaders = new Headers();

describe('ApiError from the REST envelope (03 §12.2, 04 §9.4)', () => {
  // 05 §19.1: "both error envelopes parse to the same ApiError". 03's /api/v1 handlers and 04's routes and global
  // middleware write one envelope; the admin socket wraps the same api.Error with English message/fix for the CLI.
  // Whichever writes it, the SPA must end up with the same ApiError.
  const cases: { name: string; status: number; restBody: unknown; opsBody: unknown; want: Partial<ApiError> }[] = [
    {
      name: 'validation_failed with fields',
      status: 422,
      restBody: { error: { code: 'validation_failed', fields: { username: 'invalid', password: 'too_common' } } },
      opsBody: {
        error: { code: 'validation_failed', fields: { username: 'invalid', password: 'too_common' } },
        message: 'invalid fields',
        fix: 'check the input',
      },
      want: { status: 422, code: 'validation_failed', fields: { username: 'invalid', password: 'too_common' } },
    },
    {
      name: 'rate_limited with retryAfter',
      status: 429,
      restBody: { error: { code: 'rate_limited', retryAfter: 42 } },
      opsBody: { error: { code: 'rate_limited', retryAfter: 42 }, message: 'slow down' },
      want: { status: 429, code: 'rate_limited', retryAfterSec: 42 },
    },
    {
      name: 'internal with requestId',
      status: 500,
      restBody: { error: { code: 'internal', requestId: '9f2c41d07a1be355' } },
      opsBody: { error: { code: 'internal', requestId: '9f2c41d07a1be355' }, message: 'internal error', fix: '' },
      want: { status: 500, code: 'internal', requestId: '9f2c41d07a1be355' },
    },
    {
      name: 'push_endpoint_rejected with params',
      status: 422,
      restBody: { error: { code: 'push_endpoint_rejected', params: { reason: 'private_address' } } },
      opsBody: { error: { code: 'push_endpoint_rejected', params: { reason: 'private_address' } }, message: 'x' },
      want: { status: 422, code: 'push_endpoint_rejected', params: { reason: 'private_address' } },
    },
    {
      name: 'limit_reached with params.limit',
      status: 409,
      restBody: { error: { code: 'limit_reached', params: { limit: 'rooms' } } },
      opsBody: { error: { code: 'limit_reached', params: { limit: 'rooms' } }, message: 'limit', fix: 'delete one' },
      want: { status: 409, code: 'limit_reached', params: { limit: 'rooms' } },
    },
  ];

  it.each(cases)('$name: both envelopes give the same ApiError', ({ status, restBody, opsBody, want }) => {
    const a = parseApiError(status, noHeaders, json(restBody));
    const b = parseApiError(status, noHeaders, json(opsBody));
    expect(a).toBeInstanceOf(ApiError);
    expect(a).toMatchObject(want);
    expect(b).toEqual(a);
    expect(b.message).toBe(a.message);
  });

  it('keeps only the envelope fields, never English text', () => {
    const e = parseApiError(403, noHeaders, json({ error: { code: 'forbidden' }, message: 'Forbidden', fix: 'ask' }));
    expect(Object.keys(e).sort()).toEqual(['code', 'name', 'status']);
    expect(e.message).toBe('api: forbidden (HTTP 403)');
  });

  it('takes Retry-After from the header when the body has no retryAfter', () => {
    const e = parseApiError(503, new Headers({ 'Retry-After': '5' }), json({ error: { code: 'server_shutdown' } }));
    expect(e).toMatchObject({ code: 'server_shutdown', retryAfterSec: 5 });
    const both = parseApiError(
      429,
      new Headers({ 'Retry-After': '9' }),
      json({ error: { code: 'rate_limited', retryAfter: 3 } }),
    );
    expect(both.retryAfterSec).toBe(3);
  });

  it.each([
    ['plain text (the Host check 421)', 421, 'Misdirected Request'],
    ['an HTML proxy page', 502, '<html><body>Bad gateway</body></html>'],
    ['JSON without the envelope', 500, json({ message: 'oops' })],
    ['an envelope without a code', 400, json({ error: { fields: {} } })],
    ['an empty code', 400, json({ error: { code: '' } })],
    ['an empty body', 404, ''],
  ])('%s → code "unknown" with the status', (_name, status, body) => {
    expect(parseApiError(status, noHeaders, body)).toMatchObject({ status, code: 'unknown' });
  });

  it('drops malformed optional fields instead of failing', () => {
    const e = parseApiError(
      422,
      noHeaders,
      json({
        error: { code: 'validation_failed', fields: { a: 'invalid', b: 3 }, params: [1], retryAfter: -1, requestId: 7 },
      }),
    );
    expect(e.fields).toEqual({ a: 'invalid' });
    expect(e.params).toBeUndefined();
    expect(e.retryAfterSec).toBeUndefined();
    expect(e.requestId).toBeUndefined();
  });
});

describe('parseRetryAfter', () => {
  it.each([
    [null, undefined],
    ['', undefined],
    ['12', 12],
    [' 3 ', 3],
    ['-1', undefined],
    ['soon', undefined],
  ])('%s → %s', (v, want) => {
    expect(parseRetryAfter(v)).toBe(want);
  });

  it('reads an HTTP date as seconds from now', () => {
    const now = Date.parse('2026-09-30T12:00:00Z');
    expect(parseRetryAfter('Wed, 30 Sep 2026 12:00:30 GMT', now)).toBe(30);
    expect(parseRetryAfter('Wed, 30 Sep 2026 11:00:00 GMT', now)).toBe(0);
  });
});

describe('api()', () => {
  it('sends Content-Type: application/json and a JSON body on every unsafe method, {} when empty', async () => {
    const seen: { method: string; type: string | null; body: string }[] = [];
    server.use(
      http.all(apiPath('/api/v1/echo'), async ({ request }) => {
        seen.push({ method: request.method, type: request.headers.get('Content-Type'), body: await request.text() });
        return HttpResponse.json({ ok: true });
      }),
    );
    await api('POST', '/api/v1/echo', { a: 1 });
    await api('POST', '/api/v1/echo');
    await api('PUT', '/api/v1/echo', { b: 2 });
    await api('PATCH', '/api/v1/echo', {});
    await api('DELETE', '/api/v1/echo');
    await api('GET', '/api/v1/echo');
    expect(seen).toEqual([
      { method: 'POST', type: 'application/json', body: '{"a":1}' },
      { method: 'POST', type: 'application/json', body: '{}' },
      { method: 'PUT', type: 'application/json', body: '{"b":2}' },
      { method: 'PATCH', type: 'application/json', body: '{}' },
      { method: 'DELETE', type: 'application/json', body: '{}' },
      { method: 'GET', type: null, body: '' },
    ]);
  });

  it('refuses a body on GET', async () => {
    await expect(api('GET', '/api/v1/info', { x: 1 })).rejects.toThrow(TypeError);
  });

  it('goes through platform.apiFetch with the path, never a hand-built URL', async () => {
    const platform = createTestPlatform();
    const spy = vi.spyOn(platform, 'apiFetch');
    configureApi(platform);
    await api('GET', '/api/v1/info');
    expect(spy).toHaveBeenCalledWith('/api/v1/info', expect.objectContaining({ method: 'GET' }));
  });

  it('resolves JSON, and undefined for 204 and for a 2xx without JSON (202 from /push/test)', async () => {
    server.use(
      http.get(apiPath('/api/v1/json'), () => HttpResponse.json({ n: 1 })),
      http.post(apiPath('/api/v1/none'), () => new HttpResponse(null, { status: 204 })),
      http.post(apiPath('/api/v1/push/test'), () => new HttpResponse(null, { status: 202 })),
    );
    await expect(api('GET', '/api/v1/json')).resolves.toEqual({ n: 1 });
    await expect(api('POST', '/api/v1/none')).resolves.toBeUndefined();
    await expect(api('POST', '/api/v1/push/test')).resolves.toBeUndefined();
  });

  it('rejects with ApiError on a non-2xx response', async () => {
    server.use(http.post(apiPath('/api/v1/auth/login'), () => apiError(401, { code: 'invalid_credentials' })));
    const err = await api('POST', '/api/v1/auth/login', { username: 'a', password: 'b' }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 401, code: 'invalid_credentials' });
  });

  it('rejects with ApiError "unknown" for a non-JSON error body', async () => {
    server.use(http.get(apiPath('/api/v1/info'), () => new HttpResponse('Misdirected Request', { status: 421 })));
    await expect(api('GET', '/api/v1/info')).rejects.toMatchObject({ status: 421, code: 'unknown' });
  });

  it('rejects with NetworkError when there is no response', async () => {
    server.use(http.get(apiPath('/api/v1/info'), () => HttpResponse.error()));
    const err = await api('GET', '/api/v1/info').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(NetworkError);
    expect(isRetryableError(err)).toBe(true);
  });

  it('rejects with the abort reason when aborted', async () => {
    server.use(http.get(apiPath('/api/v1/slow'), () => new Promise<Response>(() => undefined)));
    const ctl = new AbortController();
    const p = api('GET', '/api/v1/slow', undefined, { signal: ctl.signal });
    ctl.abort(new DOMException('stop', 'AbortError'));
    await expect(p).rejects.toMatchObject({ name: 'AbortError' });
  });

  it('fails clearly without a platform', async () => {
    configureApi(null);
    await expect(api('GET', '/api/v1/info')).rejects.toThrow(/configureApi/);
  });
});

describe('error classification', () => {
  const e = (status: number, code: string) => new ApiError({ status, code });

  it('signed out = 401 unauthenticated or invalid_token, nothing else', () => {
    expect(isSignedOutError(e(401, 'unauthenticated'))).toBe(true);
    expect(isSignedOutError(e(401, 'invalid_token'))).toBe(true);
    expect(isSignedOutError(e(401, 'invalid_credentials'))).toBe(false);
    expect(isSignedOutError(e(403, 'unauthenticated'))).toBe(false);
    expect(isSignedOutError(new NetworkError())).toBe(false);
  });

  it('retryable = network, 5xx, server_busy, rate_limited (05 §6.2)', () => {
    expect(isRetryableError(new NetworkError())).toBe(true);
    expect(isRetryableError(e(500, 'internal'))).toBe(true);
    expect(isRetryableError(e(503, 'server_shutdown'))).toBe(true);
    expect(isRetryableError(e(503, 'server_busy'))).toBe(true);
    expect(isRetryableError(e(429, 'rate_limited'))).toBe(true);
    expect(isRetryableError(e(400, 'bad_request'))).toBe(false);
    expect(isRetryableError(e(401, 'unauthenticated'))).toBe(false);
    expect(isRetryableError(e(404, 'not_found'))).toBe(false);
    expect(isRetryableError(new Error('x'))).toBe(false);
  });
});
