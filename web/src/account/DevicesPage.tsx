import { queryOptions, useMutation, useQuery, type UseQueryResult } from '@tanstack/react-query';
import { useEffect, useId, useRef, useState, type ReactNode, type SyntheticEvent } from 'react';
import { flushSync } from 'react-dom';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { useInfo } from '../app/info';
import { useMeQuery } from '../app/me';
import { Notice } from '../auth/Notice';
import { useLogout } from '../auth/useLogout';
import { errorMessage } from '../lib/errorText';
import { formatDateTime, formatRelativeTime, parseWireTime } from '../lib/time';
import { appWithVersion } from '../link/appName';
import { checkDeviceName, deviceNameError } from '../link/deviceName';
import {
  CodeNotFound,
  CodeValidationFailed,
  type DeviceInfo,
  type DeviceResponse,
  type DevicesResponse,
  type RenameDeviceRequest,
  type RevokeOthersResponse,
  type SessionInfo,
  type SessionsResponse,
} from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { api, isApiError, type ApiPath } from '../protocol/rest';
import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { Dialog } from '../ui/Dialog';
import { TextField } from '../ui/Field';
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

/** GET /api/v1/me/devices (03 §12.3 #17; M2 02 §7.2): the linked apps, with `current` for a bearer's own. */
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
 * - Linked apps (M2 02 §10.2): GET /api/v1/me/devices. Each row has the app's name, its kind and version
 *   (link.app.<kind>.<os>), when it was linked, when it was last seen and from where, Rename (an inline field,
 *   PATCH /api/v1/me/devices/{id}) and Remove (a confirmation, then DELETE /api/v1/me/devices/{id}).
 *
 * In M2 the page runs only in a browser with a cookie session (the desktop app opens it in the system browser), so
 * no app row is `current`. For a later client that shows the page with a bearer (02 §10.2: the mobile apps, M4's
 * agent), its own row says "This app" and Remove warns that it signs this app out, and the page leaves out what a
 * bearer can't do: "Sign out other browsers" is cookie-only (POST …/revoke-others is UserSession, 02 §6.4).
 */
export function DevicesPage() {
  const { t } = useTranslation();
  // /me answers {device} instead of {session} for a bearer (02 §7.2).
  const asDevice = useMeQuery().data?.device !== undefined;
  return (
    <AccountShell title={t('account.devices.title')}>
      <Browsers asDevice={asDevice} />
      <LinkedDevices />
    </AccountShell>
  );
}

function Browsers({ asDevice }: { asDevice: boolean }) {
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
        {hasOthers && !asDevice && (
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

  /**
   * A removed row leaves at once; the server's list follows. Focus was in the row (on its Remove, which the closed
   * dialog handed it back to, or in its rename field), so the heading takes it.
   */
  const gone = (id: string): void => {
    queryClient.setQueryData<DevicesResponse>(queryKeys.meDevices, (old) =>
      old ? { ...old, devices: old.devices.filter((d) => d.id !== id) } : old,
    );
    void queryClient.invalidateQueries({ queryKey: queryKeys.meDevices });
    heading.current?.focus();
  };

  /** A renamed row shows its new name at once; the server's list follows. */
  const renamed = (device: DeviceInfo): void => {
    queryClient.setQueryData<DevicesResponse>(queryKeys.meDevices, (old) =>
      old ? { ...old, devices: old.devices.map((d) => (d.id === device.id ? { ...d, ...device } : d)) } : old,
    );
    void queryClient.invalidateQueries({ queryKey: queryKeys.meDevices });
  };

  return (
    <Section heading={t('account.devices.linked.heading')} headingRef={heading}>
      <ListState query={devices}>
        {list.length === 0 ? (
          <p className={styles.empty}>{t('link.devices.empty')}</p>
        ) : (
          <ul className={styles.list}>
            {list.map((device) => (
              <DeviceRow
                key={device.id}
                device={device}
                onGone={() => {
                  gone(device.id);
                }}
                onRenamed={renamed}
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

/** "Sep 30, 2026" in the given language. */
function formatDate(date: Date, lang: string): string {
  return new Intl.DateTimeFormat(lang, { dateStyle: 'medium' }).format(date);
}

/** "Linked Sep 30, 2026", with the exact time as a tooltip. Nothing when the server sent no time. */
function LinkedOn({ at }: { at: string | undefined }) {
  const { t, i18n } = useTranslation();
  const date = parseWireTime(at);
  if (!date) return null;
  return (
    <time dateTime={date.toISOString()} title={formatDateTime(date, i18n.language)}>
      {t('link.devices.linkedOn', { date: formatDate(date, i18n.language) })}
    </time>
  );
}

/** "Last seen 3 minutes ago from 203.0.113.7" (02 §10.2), with the exact time as a tooltip. */
function DeviceLastSeen({ device }: { device: DeviceInfo }) {
  const { t, i18n } = useTranslation();
  const date = parseWireTime(device.lastSeenAt);
  if (!date) return null;
  const when = lastSeenText(date, i18n.language);
  const ip = device.lastIp ?? '';
  return (
    <time dateTime={date.toISOString()} title={formatDateTime(date, i18n.language)}>
      {ip === '' ? t('link.devices.lastSeenNoIp', { when }) : t('link.devices.lastSeen', { when, ip })}
    </time>
  );
}

interface DeviceRowProps {
  device: DeviceInfo;
  /** It was removed (here, or already elsewhere): take the row off the list. */
  onGone: () => void;
  /** The server renamed it: its answer. */
  onRenamed: (device: DeviceInfo) => void;
}

/**
 * One linked app (02 §10.2): its name ("This app" for the caller's own, a bearer only), the app and its version,
 * when it was linked, when and where it was last seen, Rename and Remove.
 */
function DeviceRow({ device, onGone, onRenamed }: DeviceRowProps) {
  const { t } = useTranslation();
  const nameId = useId();
  const [editing, setEditing] = useState(false);
  const renameButton = useRef<HTMLButtonElement>(null);
  /** The rename field just closed: focus goes back to Rename, which is there again. */
  const [refocus, setRefocus] = useState(false);
  useEffect(() => {
    if (!refocus) return;
    renameButton.current?.focus();
  }, [refocus]);

  const close = (): void => {
    setEditing(false);
    setRefocus(true);
  };

  return (
    <li className={cx(styles.row, styles.appRow)}>
      <div className={styles.what}>
        {editing ? (
          <RenameForm device={device} onClose={close} onRenamed={onRenamed} onGone={onGone} />
        ) : (
          <p className={styles.name}>
            <span id={nameId}>{device.name}</span>
            {device.current && <span className={styles.badge}>{t('link.devices.thisApp')}</span>}
          </p>
        )}
        <p className={styles.meta}>
          <span>{appWithVersion(t, device.clientKind, device.os, device.appVersion)}</span>
          <LinkedOn at={device.createdAt} />
        </p>
        <p className={styles.meta}>
          <DeviceLastSeen device={device} />
        </p>
      </div>
      {!editing && (
        <div className={styles.rowActions}>
          <Button
            ref={renameButton}
            aria-describedby={nameId}
            onClick={() => {
              setRefocus(false);
              setEditing(true);
            }}
          >
            {t('link.devices.rename')}
          </Button>
          <RemoveDevice device={device} nameId={nameId} onGone={onGone} />
        </div>
      )}
    </li>
  );
}

interface RenameFormProps {
  device: DeviceInfo;
  /** Saved, unchanged or cancelled: the row shows the name again. */
  onClose: () => void;
  onRenamed: (device: DeviceInfo) => void;
  onGone: () => void;
}

/**
 * Rename, inline (02 §7.2, §10.2): PATCH /api/v1/me/devices/{id} {name} → 200 {device}. Enter saves; Esc and Cancel
 * close without a request, and so does an unchanged name. 422 {name} shows under the field; 404 not_found means the
 * app was removed meanwhile, so its row goes.
 */
function RenameForm({ device, onClose, onRenamed, onGone }: RenameFormProps) {
  const { t } = useTranslation();
  const { ui } = useApp();
  const [value, setValue] = useState(device.name);
  const [error, setError] = useState<string | null>(null);
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => {
    input.current?.focus();
    input.current?.select();
  }, []);

  const rename = useMutation({
    mutationFn: (name: string) => {
      const body: RenameDeviceRequest = { name };
      return api<DeviceResponse | undefined>('PATCH', `/api/v1/me/devices/${encodeURIComponent(device.id)}`, body);
    },
    onSuccess: (res, name) => {
      const next = res?.device ?? { ...device, name };
      onRenamed(next);
      ui.getState().announce(t('link.devices.renamed', { name: next.name }));
      onClose();
    },
    onError: (err) => {
      if (isApiError(err, CodeNotFound)) {
        ui.getState().announce(t('link.devices.gone', { name: device.name }));
        onGone();
        return;
      }
      const field = isApiError(err, CodeValidationFailed) ? err.fields?.['name'] : undefined;
      setError(field !== undefined ? deviceNameError(field, t) : errorMessage(err, t));
      input.current?.focus();
    },
  });

  const submit = (e: SyntheticEvent): void => {
    e.preventDefault();
    if (rename.isPending) return;
    const name = value.trim();
    const problem = checkDeviceName(name);
    if (problem !== null) {
      setError(deviceNameError(problem, t));
      input.current?.focus();
      return;
    }
    if (name === device.name) {
      onClose();
      return;
    }
    setError(null);
    rename.mutate(name);
  };

  return (
    <form className={styles.rename} noValidate onSubmit={submit}>
      <TextField
        ref={input}
        label={t('link.devices.renameLabel', { name: device.name })}
        name="name"
        autoComplete="off"
        spellCheck={false}
        value={value}
        onChange={(e) => {
          setValue(e.target.value);
        }}
        onKeyDown={(e) => {
          if (e.key === 'Escape' && !rename.isPending) {
            e.preventDefault();
            onClose();
          }
        }}
        error={error ?? undefined}
      />
      <div className={styles.renameActions}>
        <Button type="submit" variant="primary" loading={rename.isPending}>
          {t('link.devices.save')}
        </Button>
        <Button disabled={rename.isPending} onClick={onClose}>
          {t('common.cancel')}
        </Button>
      </div>
    </form>
  );
}

/**
 * Remove one linked app (02 §10.2): a confirmation, then DELETE /api/v1/me/devices/{id} → 204. The app is signed
 * out (its connections close, 02 §6.5) and has to be linked again. A 404 not_found means it was removed already,
 * which is what was asked for, so the row goes the same way.
 */
function RemoveDevice({ device, nameId, onGone }: { device: DeviceInfo; nameId: string; onGone: () => void }) {
  const { t } = useTranslation();
  const { ui } = useApp();
  const [open, setOpen] = useState(false);
  const remove = useMutation({
    mutationFn: async () => {
      try {
        await api('DELETE', `/api/v1/me/devices/${encodeURIComponent(device.id)}`);
      } catch (err) {
        if (!isApiError(err, CodeNotFound)) throw err;
      }
    },
    onSuccess: () => {
      ui.getState().announce(t('link.devices.removed', { name: device.name }));
      // The dialog closes first: while it is modal the rest of the page is inert, so the heading could not take
      // focus, and closing it afterwards would hand focus back to this row's Remove, which is about to go.
      flushSync(() => {
        setOpen(false);
      });
      onGone();
    },
  });
  // After a success the row is about to leave: the button stays busy rather than inviting a second click.
  const busy = remove.isPending || remove.isSuccess;
  const close = (): void => {
    setOpen(false);
  };
  return (
    <>
      <Button
        aria-describedby={nameId}
        onClick={() => {
          remove.reset();
          setOpen(true);
        }}
      >
        {t('link.devices.remove')}
      </Button>
      <Dialog
        open={open}
        onClose={close}
        size="sm"
        title={t('link.devices.removeTitle', { name: device.name })}
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
                remove.mutate();
              }}
            >
              {t('link.devices.remove')}
            </Button>
          </>
        }
      >
        <div className={styles.confirm}>
          <p>
            {device.current ? t('link.devices.removeBodyCurrent') : t('link.devices.removeBody', { name: device.name })}
          </p>
          {remove.isError && <Notice>{errorMessage(remove.error, t)}</Notice>}
        </div>
      </Dialog>
    </>
  );
}

interface RevokeButtonProps {
  /** The session: DELETE it. */
  path: ApiPath;
  /** The row's name, for the messages. */
  name: string;
  /** The element that shows the name: it describes the button, since every row's says just "Revoke". */
  nameId: string;
  /** It is signed out: take the row off the list. */
  onGone: () => void;
}

/**
 * Revoke one browser: DELETE → 204. A 404 not_found means it was signed out already (it expired, or another tab
 * revoked it), which is what was asked for, so the row goes the same way.
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
