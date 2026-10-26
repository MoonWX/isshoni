import { TriangleAlert } from 'lucide-react';
import { useCallback, useEffect, useId, useRef, useState, type ReactNode, type SyntheticEvent } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { useLocation, useNavigate } from 'react-router';

import { useApp } from '../app/context';
import { useInfo } from '../app/info';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { focusPageHeading } from '../app/layouts/RootLayout';
import { clearMe, useMeQuery } from '../app/me';
import { Actions, Prose, SmallPrint } from '../auth/AuthForm';
import { Notice } from '../auth/Notice';
import { useLogout } from '../auth/useLogout';
import { errorMessage } from '../lib/errorText';
import { formatRelativeTime, parseWireTime } from '../lib/time';
import { NetworkMatchDifferent, NetworkMatchSame, type DeviceLookup } from '../protocol/api.gen';
import { ClientKindDesktop } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { TextField } from '../ui/Field';
import { Spinner } from '../ui/Spinner';
import { VisuallyHidden } from '../ui/VisuallyHidden';
import { appWithVersion } from './appName';
import styles from './DeviceLinkPage.module.css';
import { checkDeviceName, deviceNameError } from './deviceName';
import { useDeviceLink, type DeviceLink, type LinkState } from './useDeviceLink';
import { formatCodeInput, formatUserCode, normalizeUserCode, spokenUserCode } from './userCode';

/**
 * /link (docs/m2/02-device-auth.md §10.1): the browser's approval of a desktop app's link request (RFC 8628), with
 * the friend's cookie session. RequireAuth sent a signed-out browser to /login?next=/link?code=… first, so the page
 * always has a user.
 *
 * - ?code= is read once and taken out of the address bar (history.replaceState through the router), so the code
 *   doesn't linger in the history; it stays in the page's state. A missing or malformed code shows the code field.
 * - The confirm card shows the code to compare with the app, the app and its version, a name for it, where and when
 *   the request was made, and what the app may do. Any `network` other than `same`, one this page doesn't know
 *   included, needs the "I started this myself" tick before Link (the check fails closed; §5.3: a phisher can force
 *   `unknown`). Only the Link and Don't link buttons decide; no GET ever approves.
 * - Each state change moves focus to the new heading, and the result states are announced (02 §10.1).
 * - When the session ends under the page (a 401, or "Not you? Sign out"), the code goes back into the address bar
 *   first, so the login page's next= brings the friend back to this card.
 */
export function DeviceLinkPage() {
  const { t } = useTranslation();
  const { queryClient, ui } = useApp();
  const location = useLocation();
  const navigate = useNavigate();
  const [rawCode] = useState(() => new URLSearchParams(location.search).get('code'));

  // The code leaves the address bar once, at the first render's effect; later changes of the URL are the page's own.
  const taken = useRef(false);
  useEffect(() => {
    if (taken.current) return;
    taken.current = true;
    const params = new URLSearchParams(location.search);
    if (!params.has('code')) return;
    params.delete('code');
    const rest = params.toString();
    void navigate(
      { pathname: location.pathname, search: rest === '' ? '' : `?${rest}`, hash: location.hash },
      { replace: true, state: location.state as unknown },
    );
  }, [location, navigate]);

  // The session's end under the page (a 401, or "Not you? Sign out") waits until the code is back in the address bar
  // and committed (the effect below): RequireAuth's redirect reads the location it renders with, and a router update
  // is a transition that the signed-out render could otherwise overtake, leaving next= without the code. The
  // navigation is a plain one, not flushSync: react-router would then run React's effects inside its own state
  // update, so this effect's sign-out and RequireAuth's redirect would start while that navigation still counts as
  // the current one, and the redirect would finish without its history.replace (the login form under an address bar
  // that still says /link?code=…).
  const { logout, pending: signingOut } = useLogout();
  const endSession = useRef<{ readonly code: string; readonly how: 'cleared' | 'logout' } | null>(null);
  const endWithCode = useCallback(
    (code: string, how: 'cleared' | 'logout'): void => {
      endSession.current = { code, how };
      void navigate({ pathname: location.pathname, search: `?code=${formatUserCode(code)}` }, { replace: true });
    },
    [location.pathname, navigate],
  );
  useEffect(() => {
    const end = endSession.current;
    if (end === null || new URLSearchParams(location.search).get('code') !== formatUserCode(end.code)) return;
    endSession.current = null;
    if (end.how === 'cleared') {
      // The server said the session is gone: this tab is signed out, as a 401 from a query would do.
      clearMe(queryClient);
      return;
    }
    void logout().then((ended) => {
      // Still signed in (the server wasn't reached; a toast says so): the code leaves the address bar again.
      if (!ended) void navigate({ pathname: location.pathname }, { replace: true });
    });
  }, [location.pathname, location.search, logout, navigate, queryClient]);

  const onSignedOut = useCallback(
    (code: string): void => {
      endWithCode(code, 'cleared');
    },
    [endWithCode],
  );

  const link = useDeviceLink(rawCode, onSignedOut);
  const { state } = link;

  // Focus and announcements follow the state: not on the first render (RootLayout's rule for the first page load).
  const shownKind = useRef<LinkState['kind'] | null>(null);
  useEffect(() => {
    const before = shownKind.current;
    shownKind.current = state.kind;
    if (before === null || before === state.kind) return;
    focusPageHeading();
    const spoken = resultText(state, t);
    if (spoken !== null) ui.getState().announce(spoken);
  }, [state, t, ui]);

  switch (state.kind) {
    case 'enter':
      return (
        <CenteredPage title={t('link.enter.title')}>
          <CodeForm draft={state.draft} onSubmit={link.lookup} />
        </CenteredPage>
      );
    case 'loading':
      return (
        <CenteredPage title={t('link.title')}>
          <div className={styles.loading}>
            <Spinner label={t('link.checking')} />
          </div>
        </CenteredPage>
      );
    case 'confirm':
      return (
        <Confirm
          code={state.code}
          lookup={state.lookup}
          link={link}
          signingOut={signingOut}
          onSignOut={() => {
            endWithCode(state.code, 'logout');
          }}
        />
      );
    case 'linked':
      return (
        <CenteredPage title={t('link.linked.title')}>
          <Prose>
            <p>{t('link.linked.body')}</p>
          </Prose>
        </CenteredPage>
      );
    case 'denied':
      return (
        <CenteredPage title={t('link.denied.title')}>
          <Prose>
            <p>{t('link.denied.body')}</p>
          </Prose>
        </CenteredPage>
      );
    case 'invalid':
      return (
        <CenteredPage title={t('link.invalid.title')}>
          <Prose>
            <p>{t('errors.device_code_invalid')}</p>
          </Prose>
          <CodeForm draft="" onSubmit={link.lookup} />
        </CenteredPage>
      );
    case 'rate_limited':
      return (
        <CenteredPage title={t('link.rateLimited.title')}>
          <Prose>
            <p>{t('link.rateLimited.body', { minutes: state.minutes })}</p>
          </Prose>
        </CenteredPage>
      );
    case 'error':
      return (
        <CenteredPage title={t('link.title')}>
          <Notice
            action={
              <Button size="sm" onClick={link.retry}>
                {t('auth.tryAgain')}
              </Button>
            }
          >
            {errorMessage(state.error, t)}
          </Notice>
        </CenteredPage>
      );
  }
}

/** What a result state says to screen readers besides its heading (02 §10.1: announced, aria-live polite). */
function resultText(state: LinkState, t: ReturnType<typeof useTranslation>['t']): string | null {
  switch (state.kind) {
    case 'linked':
      return t('link.linked.body');
    case 'denied':
      return t('link.denied.body');
    case 'invalid':
      return t('errors.device_code_invalid');
    case 'rate_limited':
      return t('link.rateLimited.body', { minutes: state.minutes });
    default:
      return null;
  }
}

/**
 * The code field (02 §10.1): one input, upper case, the "-" inserted after four letters; Continue is on at 8 letters
 * of the alphabet.
 */
function CodeForm({ draft, onSubmit }: { draft: string; onSubmit: (code: string) => void }) {
  const { t } = useTranslation();
  const [value, setValue] = useState(draft);
  const code = normalizeUserCode(value);
  return (
    <form
      className={styles.form}
      noValidate
      onSubmit={(e) => {
        e.preventDefault();
        if (code !== null) onSubmit(code);
      }}
    >
      <TextField
        className={styles.codeField}
        label={t('link.enter.label')}
        hint={t('link.enter.hint')}
        name="code"
        value={value}
        onChange={(e) => {
          setValue(formatCodeInput(e.target.value));
        }}
        autoCapitalize="characters"
        autoComplete="off"
        autoCorrect="off"
        spellCheck={false}
        enterKeyHint="go"
      />
      <Button type="submit" variant="primary" block disabled={code === null}>
        {t('link.enter.continue')}
      </Button>
    </form>
  );
}

/** "1 minute ago"; "now" for a time ahead of this device's clock. */
function requestedWhen(lookup: DeviceLookup, lang: string): string {
  const at = parseWireTime(lookup.requestedAt);
  if (at === undefined) return formatRelativeTime(new Date(), lang);
  const now = new Date();
  return formatRelativeTime(at > now ? now : at, lang, now);
}

interface ConfirmProps {
  code: string;
  lookup: DeviceLookup;
  link: DeviceLink;
  /** "Not you? Sign out". */
  onSignOut: () => void;
  signingOut: boolean;
}

/** The confirm card (02 §10.1): same, different and unknown (any other value) networks. */
function Confirm({ code, lookup, link, onSignOut, signingOut }: ConfirmProps) {
  const { t, i18n } = useTranslation();
  const serverName = useInfo().data?.server.name ?? t('common.appName');
  const username = useMeQuery().data?.user.username ?? '';
  const [name, setName] = useState(lookup.deviceName);
  const [ticked, setTicked] = useState(false);
  const [ownNameError, setOwnNameError] = useState<string | null>(null);
  const nameInput = useRef<HTMLInputElement>(null);
  const noteId = useId();
  const tickId = useId();

  const same = lookup.network === NetworkMatchSame;
  const different = lookup.network === NetworkMatchDifferent;
  const nameError = ownNameError ?? link.nameError;
  const when = requestedWhen(lookup, i18n.language);

  // A name the server refused: focus goes to the field, whose error describes it.
  useEffect(() => {
    if (link.nameError !== null) nameInput.current?.focus();
  }, [link.nameError]);

  const approve = (e: SyntheticEvent): void => {
    e.preventDefault();
    if (!same && !ticked) return;
    const trimmed = name.trim();
    const problem = checkDeviceName(trimmed);
    setOwnNameError(problem);
    if (problem !== null) {
      nameInput.current?.focus();
      return;
    }
    link.decide(code, 'approve', trimmed === lookup.deviceName ? undefined : trimmed);
  };

  let note: ReactNode = null;
  if (!same) {
    note = (
      <>
        <div id={noteId} className={cx(styles.note, different && styles.warning)}>
          <TriangleAlert className={styles.noteIcon} aria-hidden="true" />
          <p>{t(different ? 'link.confirm.different' : 'link.confirm.unknown')}</p>
        </div>
        <div className={styles.tick}>
          <input
            id={tickId}
            type="checkbox"
            checked={ticked}
            onChange={(e) => {
              setTicked(e.target.checked);
            }}
            aria-describedby={noteId}
          />
          <label htmlFor={tickId}>{t('link.confirm.startedMyself')}</label>
        </div>
      </>
    );
  }

  return (
    <CenteredPage title={t('link.confirm.title')}>
      <div className={styles.codeBox}>
        <p className={styles.code}>
          <span aria-hidden="true">{formatUserCode(code)}</span>
          <VisuallyHidden>{spokenUserCode(code)}</VisuallyHidden>
        </p>
        <p className={styles.compare}>{t('link.confirm.compare')}</p>
      </div>
      <form className={styles.form} noValidate onSubmit={approve}>
        <p className={styles.app}>{appWithVersion(t, lookup.clientKind, lookup.os, lookup.appVersion)}</p>
        <TextField
          ref={nameInput}
          label={t(lookup.clientKind === ClientKindDesktop ? 'link.confirm.nameDesktop' : 'link.confirm.nameOther')}
          hint={t('link.confirm.nameHint')}
          name="deviceName"
          autoComplete="off"
          spellCheck={false}
          value={name}
          onChange={(e) => {
            setName(e.target.value);
          }}
          error={nameError === null ? undefined : deviceNameError(nameError, t)}
        />
        <p className={styles.requested}>
          {same
            ? t('link.confirm.requestedSame', { when, ip: lookup.requestIp })
            : t('link.confirm.requested', { when, ip: lookup.requestIp })}
        </p>
        {note}
        <p className={styles.actsAs}>
          <Trans
            i18nKey="link.confirm.actsAs"
            values={{ username, server: serverName }}
            components={{ b: <strong /> }}
          />
        </p>
        {link.decideError !== null && <Notice>{errorMessage(link.decideError, t)}</Notice>}
        <Actions>
          <Button
            type="submit"
            variant="primary"
            disabled={(!same && !ticked) || link.deciding === 'deny'}
            loading={link.deciding === 'approve'}
          >
            {t(different ? 'link.confirm.linkAnyway' : 'link.confirm.link')}
          </Button>
          <Button
            disabled={link.deciding === 'approve'}
            loading={link.deciding === 'deny'}
            onClick={() => {
              link.decide(code, 'deny');
            }}
          >
            {t('link.confirm.dontLink')}
          </Button>
        </Actions>
      </form>
      <SmallPrint>
        <p>
          <span>{t('link.confirm.signedInAs', { username })}</span>
          <button
            type="button"
            className={styles.textButton}
            aria-busy={signingOut || undefined}
            disabled={signingOut}
            onClick={onSignOut}
          >
            {t('link.confirm.signOut')}
          </button>
        </p>
      </SmallPrint>
    </CenteredPage>
  );
}
