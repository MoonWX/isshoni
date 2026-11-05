// The app shell (05 §4 step 5): providers, the router, and what stays mounted around it: toasts, the announcer's
// live regions and the update pill. An app-level screen in uiStore (Fatal, VersionMismatch) replaces the router.
// In the desktop app (M2) the banner of a required app update sits above both, on every page (docs/m2/03 §16.2); it
// comes from desktop/shell.ts, loaded on demand, so the browser never fetches it.
import { QueryClientProvider } from '@tanstack/react-query';
import { lazy, Suspense, type ReactNode } from 'react';
import { I18nextProvider } from 'react-i18next';
import type { DataRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';

import { i18n } from '../i18n';
import { Announcer } from './Announcer';
import { AppContext, useUi, type AppServices } from './context';
import { AppErrorBoundary } from './ErrorBoundary';
import { AppScreenView } from './screens';
import { Toasts } from './Toasts';
import { UpdatePill } from './UpdatePill';

/** The React context every part of the app reads: services, REST cache, i18n. */
export function AppProviders({ services, children }: { services: AppServices; children: ReactNode }) {
  return (
    <AppContext.Provider value={services}>
      <QueryClientProvider client={services.queryClient}>
        <I18nextProvider i18n={i18n}>{children}</I18nextProvider>
      </QueryClientProvider>
    </AppContext.Provider>
  );
}

const RequiredUpdateBanner = lazy(() => import('../desktop/shell').then((m) => ({ default: m.RequiredUpdateBanner })));

function Shell({ services, router }: { services: AppServices; router: DataRouter }) {
  const screen = useUi((s) => s.screen);
  return (
    <>
      {(services.platform.desktop ?? null) !== null && (
        <Suspense fallback={null}>
          <RequiredUpdateBanner />
        </Suspense>
      )}
      {screen ? <AppScreenView screen={screen} platform={services.platform} /> : <RouterProvider router={router} />}
      <UpdatePill />
      <Toasts />
      <Announcer />
    </>
  );
}

export function App({ services, router }: { services: AppServices; router: DataRouter }) {
  return (
    <AppProviders services={services}>
      <AppErrorBoundary onReload={services.platform.versionActions().reload}>
        <Shell services={services} router={router} />
      </AppErrorBoundary>
    </AppProviders>
  );
}
