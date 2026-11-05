// /desktop/relink (docs/m2/03 §8.3): the active server signed this PC out. RequireAuth sends the app here when the
// AuthState is unlinked with the notice of a refused refresh (signed_out, refresh_reused, account_disabled; the
// server answers invalid_grant for every cause and 02 turns it into the notice). It says why, and offers Link again
// (the link step for the same server, Auth.Link()), Switch server and Remove server. A live share was already
// stopped by the server. The page follows auth.state only: once the state is linked again it goes to the room, and a
// state without a relink notice (a link that started) goes to the link step.
//
// save_failed (the credential couldn't be written; linked for this run only) is not a reason to be here: it is a
// banner on the link step, shown here too if the state has it.
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Navigate, useNavigate } from 'react-router';

import { usePlatform } from '../app/context';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { NotFound } from '../app/NotFound';
import { Actions } from '../auth/AuthForm';
import { Notice } from '../auth/Notice';
import type { DesktopFeatures } from '../platform/types';
import { Button } from '../ui/Button';
import { Spinner } from '../ui/Spinner';
import { hostOf } from './address';
import { failureOf, failureText, relinkText, type BridgeFailure, type RelinkNotice } from './authText';
import { ServerSwitcher } from './ServerSwitcher';
import { useAuthState } from './useAuthState';
import styles from './wizard.module.css';

const RELINK_NOTICES: readonly string[] = ['signed_out', 'refresh_reused', 'account_disabled'];

export function RelinkPage() {
  const platform = usePlatform();
  const desktop = platform.desktop ?? null;
  if (desktop === null) return <NotFound />;
  if (platform.serverOrigin === '') return <Navigate to="/desktop/setup" replace />;
  return <Relink desktop={desktop} origin={platform.serverOrigin} />;
}

function Relink({ desktop, origin }: { desktop: DesktopFeatures; origin: string }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { bridge } = desktop;
  const appVersion = desktop.app.version;
  const { state, failed } = useAuthState(bridge, origin);
  const [switching, setSwitching] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<BridgeFailure | null>(null);
  /** The notice this page was opened for: kept while Link again starts a new link (which clears it). */
  const [notice, setNotice] = useState<RelinkNotice | null>(null);

  const current = state !== null && RELINK_NOTICES.includes(state.notice) ? (state.notice as RelinkNotice) : null;
  if (current !== null && current !== notice) setNotice(current);

  // Linked again, or a link in progress: the link step shows it (and goes on to the room).
  const elsewhere =
    state !== null && (state.status !== 'unlinked' || (state.notice !== '' && !RELINK_NOTICES.includes(state.notice)));
  useEffect(() => {
    if (elsewhere) void navigate('/desktop/setup', { replace: true });
  }, [elsewhere, navigate]);

  const server = state?.serverName !== undefined && state.serverName !== '' ? state.serverName : hostOf(origin);

  let body;
  if (failed !== null) {
    body = <Notice>{failureText(t, { code: failed, params: {} }, appVersion)}</Notice>;
  } else if (state === null || elsewhere) {
    body = (
      <div className={styles.center}>
        <Spinner />
      </div>
    );
  } else {
    body = (
      <div className={styles.stack}>
        {notice !== null && <Notice tone="info">{relinkText(t, notice, server)}</Notice>}
        {state.notice === 'save_failed' && <Notice>{t('desktop.setup.saveFailed')}</Notice>}
        {error !== null && <Notice>{failureText(t, error, appVersion)}</Notice>}
        <Actions>
          <Button
            variant="primary"
            loading={busy}
            onClick={() => {
              setBusy(true);
              setError(null);
              // The link step of the same server: it shows the code that this call starts.
              bridge.auth.link().then(
                () => {
                  void navigate('/desktop/setup', { replace: true });
                },
                (err: unknown) => {
                  setBusy(false);
                  setError(failureOf(err));
                },
              );
            }}
          >
            {t('desktop.relink.linkAgain')}
          </Button>
          <Button
            aria-expanded={switching}
            onClick={() => {
              setSwitching((v) => !v);
            }}
          >
            {t('desktop.relink.switchServer')}
          </Button>
          <Button
            variant="danger"
            disabled={busy}
            onClick={() => {
              setBusy(true);
              setError(null);
              const where = globalThis.location.pathname + globalThis.location.search;
              globalThis.history.replaceState(globalThis.history.state, '', '/');
              bridge.auth.removeServer(origin).then(
                () => {
                  // The window reloads for the next server, or for none.
                },
                (err: unknown) => {
                  globalThis.history.replaceState(globalThis.history.state, '', where);
                  setBusy(false);
                  setError(failureOf(err));
                },
              );
            }}
          >
            {t('desktop.relink.removeServer')}
          </Button>
        </Actions>
        {switching && <ServerSwitcher bridge={bridge} appVersion={appVersion} />}
      </div>
    );
  }

  return (
    <CenteredPage title={t('desktop.relink.title', { server })} width="lg">
      {body}
    </CenteredPage>
  );
}
