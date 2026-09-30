// Error boundaries (05 §4): a render error never leaves a blank page.
// - RouteErrorBoundary is every route's errorElement: "Something went wrong" with Reload, the error logged to the
//   in-memory log (lib/log.ts). A 404 thrown by the router shows NotFound.
// - AppErrorBoundary wraps the whole app (outside the router): the Fatal screen.
import { TriangleAlert } from 'lucide-react';
import { Component, useEffect, type ErrorInfo, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { isRouteErrorResponse, useRouteError } from 'react-router';

import { createLogger } from '../lib/log';
import { Button } from '../ui/Button';
import { NotFound } from './NotFound';
import { Fatal } from './screens/Fatal';
import { ScreenFrame } from './screens/ScreenFrame';

const log = createLogger('ui');

export function RouteErrorBoundary() {
  const error = useRouteError();
  const { t } = useTranslation();
  const notFound = isRouteErrorResponse(error) && error.status === 404;
  useEffect(() => {
    if (!notFound) log.error('route render failed', { error });
  }, [error, notFound]);
  if (notFound) return <NotFound />;
  return (
    <ScreenFrame
      icon={TriangleAlert}
      tone="danger"
      title={t('common.routeError.title')}
      actions={
        <Button
          variant="primary"
          onClick={() => {
            globalThis.location.reload();
          }}
        >
          {t('common.reload')}
        </Button>
      }
    >
      <p>{t('common.routeError.body')}</p>
    </ScreenFrame>
  );
}

interface AppErrorBoundaryState {
  failed: boolean;
}

export class AppErrorBoundary extends Component<{ children: ReactNode; onReload?: () => void }, AppErrorBoundaryState> {
  override state: AppErrorBoundaryState = { failed: false };

  static getDerivedStateFromError(): AppErrorBoundaryState {
    return { failed: true };
  }

  override componentDidCatch(error: unknown, info: ErrorInfo): void {
    log.error('app render failed', { error, componentStack: info.componentStack ?? '' });
  }

  override render(): ReactNode {
    if (this.state.failed) return <Fatal onReload={this.props.onReload} />;
    return this.props.children;
  }
}
