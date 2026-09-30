// Rendering helpers for component tests: the app's providers around a UI, or a memory router with the app's routes
// (or a test's own) at a given path.
import { render, type RenderResult } from '@testing-library/react';
import type { ReactNode } from 'react';
import { createMemoryRouter, RouterProvider, type DataRouter, type RouteObject } from 'react-router';

import { AppProviders } from '../app/App';
import { createAppServices, type AppServices } from '../app/context';
import { seedInfo } from '../app/info';
import { createQueryClient } from '../app/queryClient';
import { createAppRoutes } from '../app/router';
import type { Platform } from '../platform/types';
import type { Info } from '../protocol/api.gen';
import { configureApi } from '../protocol/rest';
import { infoFixture } from './msw';
import { createTestPlatform } from './platform';

export interface TestServicesOptions {
  platform?: Platform;
  /** Seeded as ['info'] like boot does; default infoFixture(). null leaves it out. */
  info?: Info | null;
}

/** Services like boot makes them, with a test platform bound to api(). Queries don't retry (tests stay fast). */
export function createTestServices({
  platform = createTestPlatform(),
  info = infoFixture(),
}: TestServicesOptions = {}): AppServices {
  configureApi(platform);
  const queryClient = createQueryClient();
  queryClient.setDefaultOptions({
    ...queryClient.getDefaultOptions(),
    queries: { ...queryClient.getDefaultOptions().queries, retry: false },
  });
  if (info) seedInfo(queryClient, info);
  return createAppServices(platform, queryClient);
}

/** Renders ui inside the app's providers. */
export function renderWithApp(
  ui: ReactNode,
  { services = createTestServices() }: { services?: AppServices } = {},
): RenderResult & { services: AppServices } {
  return { ...render(<AppProviders services={services}>{ui}</AppProviders>), services };
}

export interface RenderRouteOptions {
  /** The start path (with query). Default '/'. */
  path?: string;
  /** Default: the app's routes (createAppRoutes()). */
  routes?: RouteObject[];
  services?: AppServices;
}

/** Renders a memory router at path inside the app's providers. */
export function renderRoute({
  path = '/',
  routes = createAppRoutes(),
  services = createTestServices(),
}: RenderRouteOptions = {}): RenderResult & { services: AppServices; router: DataRouter } {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  const result = render(
    <AppProviders services={services}>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return { ...result, services, router };
}
