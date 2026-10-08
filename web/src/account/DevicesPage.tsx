import { queryOptions, useMutation, useQuery, type UseQueryResult } from '@tanstack/react-query';
import { useId, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { useInfo } from '../app/info';
import { Notice } from '../auth/Notice';
import { useLogout } from '../auth/useLogout';
import { errorMessage } from '../lib/errorText';
import { formatDateTime, formatRelativeTime, parseWireTime } from '../lib/time';
import {
  CodeNotFound,
  type DeviceInfo,
  type DevicesResponse,
  type RevokeOthersResponse,
  type SessionInfo,
  type SessionsResponse,
} from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { api, isApiError, type ApiPath } from '../protocol/rest';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import { Spinner } from '../ui/Spinner';
import { AccountShell, Section } from './AccountShell';
import styles from './DevicesPage.module.css';
import { sessionEndedByServer } from './sessionEnded';

// Both lists are about who can act as this user, so a visit always asks the server (staleTime 0); 01's `devices`
// invalidation refreshes them while the page is open (protocol/invalidate.ts).

/** GET /api/v1/me/sessions (03 §12.3 #14). */
export function sessionsQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.meSessions,
    queryFn: ({ signal }) => api<SessionsResponse>('GET', '/api/v1/me/sessions', undefined, { signal }),
    staleTime: 0,
  });
}

/** GET /api/v1/me/devices (03 §12.3 #17): linked apps, empty until M2. */
export function devicesQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.meDevices,
    queryFn: ({ signal }) => api<DevicesResponse>('GET', '/api/v1/me/devices', undefined, { signal }),
    staleTime: 0,
  });
}

function time(wire: string | undefined): number {
  return parseWireTime(wire)?.getTime() ?? 0;
}

/** This browser first, then the most recently seen. */
export function sortSessions(sessions: readonly SessionInfo[]): SessionInfo[] {
  return [...sessions].sort((a, b) => Number(b.current) - Number(a.current) || time(b.lastSeenAt) - time(a.lastSeenAt));
}

/**
 * /account/devices (05 §5, §15.2): the browsers this account is signed in on, and its linked devices.
 * - Browsers: GET /api/v1/me/sessions. Each row has the name the server made from the User-Agent, when it was last
 *   seen and from which address. Another browser's row has Revoke (DELETE /api/v1/me/sessions/{id}); this browser's
 *   says "This browser" and has Sign out, which runs the logout flow (05 §15.1).
 * - "Sign out other browsers" (POST /api/v1/me/sessions/revoke-others) and "Log out everywhere"
 *   (POST /api/v1/auth/logout-everywhere, which ends this session too).
 * - Linked devices: GET /api/v1/me/devices, with Revoke (DELETE /api/v1/me/devices/{id}). The list is empty until
 *   the desktop app exists (M2).
 */
export function DevicesPage() {
  const { t } = useTranslation();
  return (
    <AccountShell title={t('account.devices.title')}>
      <Browsers />
      <LinkedDevices />
    </AccountShell>
  );
}

function Browsers() {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  const serverName = useInfo().data?.server.name ?? t('common.appName');
  const sessions = useQuery(sessionsQueryOptions());
  const heading = useRef<HTMLHeadingElement>(null);

  const list = sortSessions(sessions.data?.sessions ?? []);
  const hasOthers = list.some((s) => !s.current);

  /** A revoked row leaves at once; the server's list follows. Its button had focus, so the heading takes it. */
  const gone = (id: string): void => {
    queryClient.setQueryData<SessionsResponse>(queryKeys.meSessions, (old) =>
      old ? { ...old, sessions: old.sessions.filter((s) => s.id !== id) } : old,
    );
    void queryClient.invalidateQueries({ queryKey: queryKeys.meSessions });
    heading.current?.focus();
  };

  return (
    <Section
      heading={t('account.devices.browsers.heading')}
      lead={t('account.devices.browsers.lead', { server: serverName })}
      headingRef={heading}
    >
      <ListState query={sessions}>
        <ul className={styles.list}>
          {list.map((session) => (
            <SessionRow
              key={session.id}
              session={session}
              onGone={() => {
                gone(session.id);
              }}
            />
          ))}
        </ul>
      </ListState>
      <div className={styles.actions}>
        {hasOthers && (
          <RevokeOthers
            onDone={() => {
              heading.current?.focus();
            }}
          />
        )}
        <LogoutEverywhere />
      </div>
    </Section>
  );
}

function LinkedDevices() {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  const devices = useQuery(devicesQueryOptions());
  const heading = useRef<HTMLHeadingElement>(null);
  const list = devices.data?.devices ?? [];

  const gone = (id: string): void => {
    queryClient.setQueryData<DevicesResponse>(queryKeys.meDevices, (old) =>
      old ? { ...old, devices: old.devices.filter((d) => d.id !== id) } : old,
    );
    void queryClient.invalidateQueries({ queryKey: queryKeys.meDevices });
    heading.current?.focus();
  };

  return (
    <Section heading={t('account.devices.linked.heading')} headingRef={heading}>
      <ListState query={devices}>
        {list.length === 0 ? (
          <p className={styles.empty}>{t('account.devices.linked.empty')}</p>
        ) : (
          <ul className={styles.list}>
            {list.map((device) => (
              <DeviceRow
                key={device.id}
                device={device}
                onGone={() => {
                  gone(device.id);
                }}
              />
            ))}
          </ul>
        )}
      </ListState>
    </Section>
  );
}

/**
 * A list's loading and failure states around its content: a spinner until the first answer; when a request failed,
 * the reason with "Try again", above whatever an earlier answer still shows.
 */
function ListState({ query, children }: { query: UseQueryResult; children: ReactNode }) {
  const { t } = useTranslation();
  return (
    <>
      {query.isError && (
        <Notice
          action={
            <Button
              size="sm"
              loading={query.isFetching}
              onClick={() => {
                void query.refetch();
              }}
            >
              {t('auth.tryAgain')}
            </Button>
          }
        >
          {errorMessage(query.error, t)}
        </Notice>
      )}
      {query.isPending ? (
        <div className={styles.loading}>
          <Spinner />
        </div>
      ) : (
        query.data !== undefined && children
      )}
    </>
  );
}

/**
 * How long ago, in words ("3 minutes ago"). The server stamps the time with its clock: one that runs a little ahead
 * of this device's reads "now", not "in 20 seconds".
 */
export function lastSeenText(date: Date, lang: string, now: Date = new Date()): string {
  return formatRelativeTime(date > now ? now : date, lang, now);
}

/** "Last active 3 minutes ago", with the exact time as a tooltip. Nothing when the server sent no time. */
function LastSeen({ at }: { at: string | undefined }) {
  const { t, i18n } = useTranslation();
  const date = parseWireTime(at);
  if (!date) return null;
  return (
    <time dateTime={date.toISOString()} title={formatDateTime(date, i18n.language)}>
      {t('account.devices.lastSeen', { when: lastSeenText(date, i18n.language) })}
    </time>
  );
}

function SessionRow({ session, onGone }: { session: SessionInfo; onGone: () => void }) {
  const { t } = useTranslation();
  const nameId = useId();
  const logout = useLogout();
  return (
    <li className={styles.row}>
      <div className={styles.what}>
        <p className={styles.name}>
          <span id={nameId}>{session.name}</span>
          {session.current && <span className={styles.badge}>{t('account.devices.thisBrowser')}</span>}
        </p>
        <p className={styles.meta}>
          {session.current ? <span>{t('account.devices.activeNow')}</span> : <LastSeen at={session.lastSeenAt} />}
          {session.lastIp !== undefined && session.lastIp !== '' && <span className={styles.ip}>{session.lastIp}</span>}
        </p>
      </div>
      {session.current ? (
        <Button
          aria-describedby={nameId}
          loading={logout.pending}
          onClick={() => {
            void logout.logout();
          }}
        >
          {t('account.signOut')}
        </Button>
      ) : (
        <RevokeButton
          path={`/api/v1/me/sessions/${encodeURIComponent(session.id)}`}
          name={session.name}
          nameId={nameId}
          onGone={onGone}
        />
      )}
    </li>
  );
}

function DeviceRow({ device, onGone }: { device: DeviceInfo; onGone: () => void }) {
  const nameId = useId();
  return (
    <li className={styles.row}>
      <div className={styles.what}>
        <p className={styles.name}>
          <span id={nameId}>{device.name}</span>
        </p>
        <p className={styles.meta}>
          <LastSeen at={device.lastSeenAt} />
          {device.lastIp !== undefined && device.lastIp !== '' && <span className={styles.ip}>{device.lastIp}</span>}
        </p>
      </div>
      <RevokeButton
        path={`/api/v1/me/devices/${encodeURIComponent(device.id)}`}
        name={device.name}
        nameId={nameId}
        onGone={onGone}
      />
    </li>
  );
}

interface RevokeButtonProps {
  /** The session or device: DELETE it. */
  path: ApiPath;
  /** The row's name, for the messages. */
  name: string;
  /** The element that shows the name: it describes the button, since every row's says just "Revoke". */
  nameId: string;
  /** It is signed out: take the row off the list. */
  onGone: () => void;
}

/**
 * Revoke one browser or linked device: DELETE → 204. A 404 not_found means it was signed out already (it expired,
 * or another tab revoked it), which is what was asked for, so the row goes the same way.
 */
function RevokeButton({ path, name, nameId, onGone }: RevokeButtonProps) {
  const { t } = useTranslation();
  const { ui } = useApp();
  const revoke = useMutation({
    mutationFn: async () => {
      try {
        await api('DELETE', path);
      } catch (err) {
        if (!isApiError(err, CodeNotFound)) throw err;
      }
    },
    onSuccess: () => {
      ui.getState().announce(t('account.devices.revoked', { name }));
      onGone();
    },
    onError: (err) => {
      ui.getState().toast({
        kind: 'error',
        message: t('account.devices.revokeFailed', { name, reason: errorMessage(err, t) }),
      });
    },
  });
  return (
    <Button
      aria-describedby={nameId}
      loading={revoke.isPending}
      onClick={() => {
        revoke.mutate();
      }}
    >
      {t('account.devices.revoke')}
    </Button>
  );
}

/**
 * "Sign out other browsers": POST /api/v1/me/sessions/revoke-others → 200 {revoked}. This browser stays signed in.
 * The button is only there while the list has another browser, so it leaves when it worked; onDone moves focus.
 */
function RevokeOthers({ onDone }: { onDone: () => void }) {
  const { t } = useTranslation();
  const { queryClient, ui } = useApp();
  const others = useMutation({
    mutationFn: () => api<RevokeOthersResponse | undefined>('POST', '/api/v1/me/sessions/revoke-others'),
    onSuccess: async (res) => {
      const revoked = res?.revoked ?? 0;
      ui.getState().toast({
        kind: 'success',
        message: revoked > 0 ? t('account.devices.others.done', { count: revoked }) : t('account.devices.others.none'),
      });
      await queryClient.invalidateQueries({ queryKey: queryKeys.meSessions });
      onDone();
    },
    onError: (err) => {
      ui.getState().toast({
        kind: 'error',
        message: t('account.devices.others.failed', { reason: errorMessage(err, t) }),
      });
    },
  });
  return (
    <Button
      loading={others.isPending}
      onClick={() => {
        others.mutate();
      }}
    >
      {t('account.devices.others.action')}
    </Button>
  );
}

/**
 * "Log out everywhere": POST /api/v1/auth/logout-everywhere → 204 with the cookie cleared. Every session and every
 * linked device of the account ends, this browser's included (03 §7.7), so it asks first. Afterwards this tab's side
 * of the sign-out runs (sessionEnded.ts), and the guard sends the page to /login.
 */
function LogoutEverywhere() {
  const { t } = useTranslation();
  const app = useApp();
  const [open, setOpen] = useState(false);
  const everywhere = useMutation({
    mutationFn: () => api<undefined>('POST', '/api/v1/auth/logout-everywhere'),
    onSuccess: () => sessionEndedByServer(app),
  });
  // After a success the page is about to leave: the button stays busy rather than inviting a second click.
  const busy = everywhere.isPending || everywhere.isSuccess;
  const close = (): void => {
    setOpen(false);
  };
  return (
    <>
      <Button
        onClick={() => {
          everywhere.reset();
          setOpen(true);
        }}
      >
        {t('account.devices.everywhere.action')}
      </Button>
      <Dialog
        open={open}
        onClose={close}
        size="sm"
        title={t('account.devices.everywhere.title')}
        dismissible={!busy}
        footer={
          <>
            <Button onClick={close} disabled={busy}>
              {t('common.cancel')}
            </Button>
            <Button
              variant="danger"
              loading={busy}
              onClick={() => {
                everywhere.mutate();
              }}
            >
              {t('account.devices.everywhere.confirm')}
            </Button>
          </>
        }
      >
        <div className={styles.confirm}>
          <p>{t('account.devices.everywhere.body')}</p>
          {everywhere.isError && <Notice>{errorMessage(everywhere.error, t)}</Notice>}
        </div>
      </Dialog>
    </>
  );
}
