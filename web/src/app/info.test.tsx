// The ['info'] query (05 §4, §6.2): seeded at boot, read with useInfo(), refetched by invalidation and staleness.
import { QueryObserver, useQuery } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { describe, expect, it } from 'vitest';

import { applyInvalidate } from '../protocol/invalidate';
import { queryKeys } from '../protocol/queryKeys';
import { apiPath, infoFixture, server } from '../test/msw';
import { createTestServices } from '../test/render';
import { AppProviders } from './App';
import type { AppServices } from './context';
import { infoQueryOptions, seedInfo, useInfo } from './info';

/** GET /api/v1/info answers with a server in approval registration; calls() counts the requests. */
function approvalRegistration(): { calls: () => number } {
  let calls = 0;
  server.use(
    http.get(apiPath('/api/v1/info'), () => {
      calls++;
      return HttpResponse.json(infoFixture({ registration: 'approval' }));
    }),
  );
  return { calls: () => calls };
}

function wrapperFor(services: AppServices) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <AppProviders services={services}>{children}</AppProviders>;
  };
}

describe('info', () => {
  it('useInfo reads the info boot seeded, without a request', () => {
    const counter = approvalRegistration();
    const services = createTestServices();
    const { result } = renderHook(() => useInfo(), { wrapper: wrapperFor(services) });
    expect(result.current.data).toEqual(infoFixture());
    expect(counter.calls()).toBe(0);
  });

  it('the admin.settings topic refetches it for an active reader', async () => {
    const counter = approvalRegistration();
    const services = createTestServices();
    const { result } = renderHook(() => useInfo(), { wrapper: wrapperFor(services) });
    await applyInvalidate(services.queryClient, { topics: ['admin.settings'] });
    expect(counter.calls()).toBe(1);
    await waitFor(() => {
      expect(result.current.data?.registration).toBe('approval');
    });
  });

  it('a reader without a queryFn of its own refetches stale info instead of failing', async () => {
    const counter = approvalRegistration();
    const services = createTestServices();
    const { result } = renderHook(() => useQuery({ queryKey: queryKeys.info, staleTime: 0 }), {
      wrapper: wrapperFor(services),
    });
    await waitFor(() => {
      expect(result.current.isFetching).toBe(false);
    });
    expect(result.current.error).toBeNull();
    expect(counter.calls()).toBe(1);
    expect(services.queryClient.getQueryData(infoQueryOptions().queryKey)?.registration).toBe('approval');
  });

  it('keeps the seeded info while nothing reads it (no garbage collection)', () => {
    const counter = approvalRegistration();
    const services = createTestServices();
    const { queryClient } = services;
    expect(queryClient.getQueryCache().find({ queryKey: queryKeys.info })?.gcTime).toBe(Infinity);
    // An observer that comes and goes leaves it in the cache.
    const observer = new QueryObserver(queryClient, infoQueryOptions());
    observer.subscribe(() => undefined)();
    expect(queryClient.getQueryData(queryKeys.info)).toEqual(infoFixture());
    expect(counter.calls()).toBe(0);
  });

  it('seedInfo on a fresh client sets the data', () => {
    const services = createTestServices({ info: null });
    expect(services.queryClient.getQueryData(queryKeys.info)).toBeUndefined();
    seedInfo(services.queryClient, infoFixture({ setupRequired: true }));
    expect(services.queryClient.getQueryData(infoQueryOptions().queryKey)?.setupRequired).toBe(true);
  });
});
