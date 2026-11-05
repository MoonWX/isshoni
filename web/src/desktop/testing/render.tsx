// The desktop pages' test harness: a page of this folder in a memory router, inside the app's providers, on the
// desktop test platform (app/testing/desktop.ts: the fake NativeBridge records every call). The routes around the
// page are stand-ins that only name themselves, so a test sees where the page went.
import { render, type RenderResult } from '@testing-library/react';
import type { ReactNode } from 'react';
import { createMemoryRouter, RouterProvider, useParams, type DataRouter } from 'react-router';

import '../../i18n/lazy/desktop';
import { AppProviders } from '../../app/App';
import { createDesktopTestPlatform, type DesktopTestPlatform } from '../../app/testing/desktop';
import { createTestServices } from '../../test/render';
import { RelinkPage } from '../RelinkPage';
import { SetupPage } from '../SetupPage';

function Room() {
  const { roomId = '' } = useParams();
  return <h1>{`room ${roomId}`}</h1>;
}

export interface RenderedDesktopPage extends RenderResult {
  d: DesktopTestPlatform;
  router: DataRouter;
}

/** Renders the desktop routes at path, on d (default: a platform whose server is the test origin). */
export function renderDesktopPage(path: string, d = createDesktopTestPlatform(), extra?: ReactNode) {
  const router = createMemoryRouter(
    [
      { path: '/desktop/setup', element: <SetupPage /> },
      { path: '/desktop/relink', element: <RelinkPage /> },
      { path: '/', element: <h1>home</h1> },
      { path: '/about', element: <h1>about</h1> },
      { path: '/r/:roomId', element: <Room /> },
    ],
    { initialEntries: [path] },
  );
  // Go answers null when no link waits.
  if (!('app.pendingLink' in d.fake.answers)) d.fake.answers['app.pendingLink'] = () => Promise.resolve(null);
  const services = createTestServices({ platform: d.platform });
  const result = render(
    <AppProviders services={services}>
      <RouterProvider router={router} />
      {extra}
    </AppProviders>,
  );
  return { ...result, d, router } satisfies RenderedDesktopPage;
}
