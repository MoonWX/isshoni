import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Download, Send } from 'lucide-react';
import { useEffect, useId, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { isAdmin } from '../app/guards';
import { useInfo } from '../app/info';
import { InAppBrowserBanner } from '../auth/InAppBrowserBanner';
import { useInAppBanner } from '../auth/inAppBanner';
import { Notice } from '../auth/Notice';
import { useMe } from '../auth/useMe';
import { errorMessage } from '../lib/errorText';
import { ShareStartedPrefAll, ShareStartedPrefOff, type PushPreferences } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { api } from '../protocol/rest';
import { Button } from '../ui/Button';
import { Spinner } from '../ui/Spinner';
import { AccountShell, Section } from './AccountShell';
import styles from './NotificationsPage.module.css';
import { PushHelp } from './PushHelp';
import { pushErrorMessage, usePush } from './pushState';
import { useNotificationOpen } from './useNotificationOpen';

/** GET /api/v1/push/preferences (03 §12.4.6); the choices are the user's, for every device. */
export function pushPreferencesQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.pushPreferences,
    queryFn: ({ signal }) => api<PushPreferences>('GET', '/api/v1/push/preferences', undefined, { signal }),
  });
}

/**
 * /account/notifications (05 §5, §16.3): Web Push on this device, and what the user wants to hear about.
 * - This device: a switch where the browser and the server can push (platform.notifications enable() and
 *   disable(); the browser asks for permission only on that click, never by itself), with "Send a test
 *   notification" while it is on. Elsewhere the reason and what to do: the Home Screen steps on iOS in a Safari tab,
 *   how to allow notifications again after a block, or why this browser or server can't. "Install app" while the
 *   browser offers an install prompt (05 §16.3 "Install prompts").
 * - What to tell me about: GET and PUT /api/v1/push/preferences: "When someone starts sharing" (`shareStarted`
 *   all or off) and, for admins, "Admin alerts". Shown while the server has push (/info's `push`).
 * In a chat app's built-in browser the in-app banner comes first, and the Home Screen steps wait until it is
 * dismissed (05 §16.3: they can't be followed there).
 */
export function NotificationsPage() {
  const { t } = useTranslation();
  const me = useMe().data;
  const info = useInfo().data;
  useNotificationOpen();
  if (!me) return null;
  return (
    <AccountShell title={t('account.notifications.title')}>
      <InAppBrowserBanner />
      <ThisDevice />
      {info?.push !== undefined && <Preferences admin={isAdmin(me)} />}
    </AccountShell>
  );
}

function ThisDevice() {
  const { t } = useTranslation();
  const { platform, ui } = useApp();
  const push = usePush();
  const banner = useInAppBanner();
  const toggleId = useId();
  const { refresh } = push;
  // An action of this page failed (push.error has the reason; a failure elsewhere earlier, say in the room's card,
  // isn't this page's to show).
  const [failed, setFailed] = useState(false);
  // promptInstall() changes what installState() says; this re-reads it.
  const [, setInstallTries] = useState(0);

  // The page reads the browser again on every visit: a permission changed in the browser's settings shows here.
  useEffect(() => {
    void refresh();
  }, [refresh]);

  const supported = push.support === 'supported';
  const on = push.status === 'on';
  const install = platform.pwa?.installState() === 'prompt';
  const done = (ok: boolean): void => {
    setFailed(!ok);
  };

  return (
    <Section heading={t('account.notifications.device.heading')} lead={t('account.notifications.device.lead')}>
      {push.support === null ? (
        <p className={styles.checking}>
          <Spinner size="sm" label={null} />
          <span>{t('account.notifications.device.checking')}</span>
        </p>
      ) : supported ? (
        <>
          <div className={styles.toggle}>
            <input
              id={toggleId}
              type="checkbox"
              role="switch"
              checked={on}
              disabled={push.busy}
              aria-describedby={`${toggleId}-state`}
              onChange={(e) => {
                setFailed(false);
                if (e.target.checked) {
                  void push.enable().then((outcome) => {
                    done(outcome !== 'failed');
                    if (outcome === 'on') ui.getState().toast({ kind: 'success', message: t('account.push.enabled') });
                  });
                } else {
                  void push.disable().then((ok) => {
                    done(ok);
                    if (ok) ui.getState().announce(t('account.notifications.device.disabled'));
                  });
                }
              }}
            />
            <div className={styles.toggleText}>
              <label htmlFor={toggleId}>{t('account.notifications.device.toggle')}</label>
              <p id={`${toggleId}-state`} className={styles.hint}>
                {on ? t('account.notifications.device.on') : t('account.notifications.device.off')}
              </p>
            </div>
          </div>
          {failed && push.error !== null && <Notice>{pushErrorMessage(push.error, t)}</Notice>}
          {on && (
            <TestButton
              onStart={() => {
                setFailed(false);
              }}
              onDone={done}
            />
          )}
        </>
      ) : push.support === 'needs-install' && banner.showing ? null : (
        <PushHelp support={push.support} />
      )}
      {install && (
        <div className={styles.install}>
          <p className={styles.hint}>{t('account.notifications.install.lead')}</p>
          <Button
            icon={<Download />}
            onClick={() => {
              void platform.pwa?.promptInstall().then(() => {
                setInstallTries((n) => n + 1);
              });
            }}
          >
            {t('account.notifications.install.action')}
          </Button>
        </div>
      )}
    </Section>
  );
}

/**
 * POST /api/v1/push/test: a notification to this browser (1 per 10 s; 429 says how long to wait). The page shows a
 * failure's reason (onDone(false)).
 */
function TestButton({ onStart, onDone }: { onStart: () => void; onDone: (ok: boolean) => void }) {
  const { t } = useTranslation();
  const { ui } = useApp();
  const push = usePush();
  const [sending, setSending] = useState(false);
  return (
    <div className={styles.actions}>
      <Button
        icon={<Send />}
        loading={sending}
        disabled={push.busy && !sending}
        onClick={() => {
          setSending(true);
          onStart();
          void push.test().then((ok) => {
            setSending(false);
            onDone(ok);
            if (ok) ui.getState().toast({ kind: 'success', message: t('account.notifications.device.testSent') });
          });
        }}
      >
        {t('account.notifications.device.test')}
      </Button>
    </div>
  );
}

function samePreferences(a: PushPreferences | undefined, b: PushPreferences): boolean {
  return a !== undefined && a.shareStarted === b.shareStarted && a.adminAlerts === b.adminAlerts;
}

/**
 * The user's choices (03 §12.4.6). PUT replaces both, so every change sends both; the page shows the change at
 * once and puts the server's answer in its place (or the choice before, after a failure).
 */
function Preferences({ admin }: { admin: boolean }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const prefs = useQuery(pushPreferencesQueryOptions());
  const shareId = useId();
  const alertsId = useId();

  // The choice being saved, shown until the query has the server's answer (or, after a failure, at once the
  // choice before). React state, set in the click's own turn: TanStack tells its observers a moment later, and a
  // controlled checkbox would flick back to the old state meanwhile.
  const [saving, setSaving] = useState<PushPreferences | null>(null);
  const save = useMutation({
    mutationFn: (next: PushPreferences) => api<PushPreferences>('PUT', '/api/v1/push/preferences', next),
    onSuccess: (saved) => {
      queryClient.setQueryData(queryKeys.pushPreferences, saved);
    },
  });

  const current = saving ?? prefs.data;
  // The query has caught up with the saved choice: it is shown from there again.
  if (saving !== null && !save.isPending && samePreferences(prefs.data, saving)) setSaving(null);

  const change = (patch: Partial<PushPreferences>): void => {
    if (!current) return;
    const next = { ...current, ...patch };
    setSaving(next);
    save.mutate(next, {
      onSuccess: (saved) => {
        setSaving(saved);
      },
      onError: () => {
        setSaving(null);
      },
    });
  };

  return (
    <Section heading={t('account.notifications.prefs.heading')} lead={t('account.notifications.prefs.lead')}>
      {prefs.isError && (
        <Notice
          action={
            <Button
              size="sm"
              loading={prefs.isFetching}
              onClick={() => {
                void prefs.refetch();
              }}
            >
              {t('account.notifications.prefs.retry')}
            </Button>
          }
        >
          {t('account.notifications.prefs.loadFailed', { reason: errorMessage(prefs.error, t) })}
        </Notice>
      )}
      {save.isError && (
        <Notice>{t('account.notifications.prefs.saveFailed', { reason: errorMessage(save.error, t) })}</Notice>
      )}
      {prefs.isPending ? (
        <p className={styles.checking}>
          <Spinner size="sm" label={null} />
          <span>{t('account.notifications.prefs.loading')}</span>
        </p>
      ) : (
        current !== undefined && (
          <div className={styles.prefs}>
            <div className={styles.check}>
              <input
                id={shareId}
                type="checkbox"
                checked={current.shareStarted === ShareStartedPrefAll}
                disabled={save.isPending}
                aria-describedby={`${shareId}-hint`}
                onChange={(e) => {
                  change({ shareStarted: e.target.checked ? ShareStartedPrefAll : ShareStartedPrefOff });
                }}
              />
              <div className={styles.toggleText}>
                <label htmlFor={shareId}>{t('account.notifications.prefs.shareStarted')}</label>
                <p id={`${shareId}-hint`} className={styles.hint}>
                  {t('account.notifications.prefs.shareStartedHint')}
                </p>
              </div>
            </div>
            {admin && (
              <div className={styles.check}>
                <input
                  id={alertsId}
                  type="checkbox"
                  checked={current.adminAlerts}
                  disabled={save.isPending}
                  aria-describedby={`${alertsId}-hint`}
                  onChange={(e) => {
                    change({ adminAlerts: e.target.checked });
                  }}
                />
                <div className={styles.toggleText}>
                  <label htmlFor={alertsId}>{t('account.notifications.prefs.adminAlerts')}</label>
                  <p id={`${alertsId}-hint`} className={styles.hint}>
                    {t('account.notifications.prefs.adminAlertsHint')}
                  </p>
                </div>
              </div>
            )}
          </div>
        )
      )}
    </Section>
  );
}
