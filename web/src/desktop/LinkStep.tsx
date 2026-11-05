// The wizard's link step (docs/m2/03 §8.2): it renders from the AuthState of the active server, which is this page's
// server, with the server's name and host and the trust line above the code. Every URI it opens, draws or copies is
// 02's canonical LinkState value, built in Go, never a string from the server.
//
// - linking: the code large, a QR code of verificationUriComplete, "or open {url} and enter {code}", the countdown,
//   [Open browser again] (Go opened it once for the new code), [Copy link] and the rename note;
// - linked (or offline: linked, the server not reached right now): "Linked as …", then onDone; with save_failed the
//   banner and [Continue] instead, so it is read;
// - the notices denied and code_expired, a failed Link() and incompatible, each with its way to try again;
// - "Sign in with username and password instead" (02 §14): PasswordForm.
// Leaving the step while a code is shown calls Auth.CancelLink().
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { Actions, Prose } from '../auth/AuthForm';
import { Notice } from '../auth/Notice';
import type { AuthState, NativeBridge } from '../platform/desktop/bridge';
import { QrCode } from '../setup/QrCode';
import { Button } from '../ui/Button';
import { Spinner } from '../ui/Spinner';
import { hostOf } from './address';
import { compatText, failureOf, failureText, type BridgeFailure } from './authText';
import { formatLeft, TrustLine, useNow } from './parts';
import { PasswordForm } from './PasswordForm';
import { useAuthState } from './useAuthState';
import styles from './wizard.module.css';

/** The Link() errors with a text of their own (03 §8.2); every other one is the generic link error. */
const LINK_ERROR_CODES: ReadonlySet<string> = new Set(['server_unreachable', 'rate_limited', 'incompatible_server']);

export interface LinkStepProps {
  bridge: NativeBridge;
  /** This page's server (bridge.js), the active one. */
  origin: string;
  appVersion: string;
  /** The page was loaded at /desktop/setup?link=1 after Auth.AddServer: Link() once, when unlinked without a notice. */
  autoLink: boolean;
  /** Linked: the wizard goes on. */
  onDone: () => void;
  /** Opens a URL in the system browser (platform.openExternal). */
  openExternal: (url: string) => void;
  /** "Use another server": the server step. */
  onOtherServer: () => void;
  /** Tells the page's heading what this step is called. */
  onTitle: (title: string) => void;
}

export function LinkStep({
  bridge,
  origin,
  appVersion,
  autoLink,
  onDone,
  openExternal,
  onOtherServer,
  onTitle,
}: LinkStepProps) {
  const { t } = useTranslation();
  const { state, failed, set } = useAuthState(bridge, origin);
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState<BridgeFailure | null>(null);
  const [password, setPassword] = useState(false);
  const [rechecking, setRechecking] = useState(false);

  const startLink = useCallback(() => {
    setError(null);
    setStarting(true);
    bridge.auth.link().then(
      () => {
        setStarting(false);
      },
      (err: unknown) => {
        setStarting(false);
        setError(failureOf(err));
      },
    );
  }, [bridge]);

  // ?link=1: the click on Link (or Add) a moment ago, before the reload, is this call's user gesture (02 §11.3).
  const autoDone = useRef(false);
  useEffect(() => {
    if (!autoLink || autoDone.current || state === null) return;
    autoDone.current = true;
    if (state.status === 'unlinked' && state.notice === '') queueMicrotask(startLink);
  }, [autoLink, state, startLink]);

  // Leaving the page while a code is shown ends that link (03 §8.2).
  const linking = useRef(false);
  useEffect(() => {
    linking.current = state?.status === 'linking';
  }, [state]);
  useEffect(
    () => () => {
      if (linking.current) void bridge.auth.cancelLink().catch(() => undefined);
    },
    [bridge],
  );

  const linked = state !== null && (state.status === 'linked' || state.status === 'offline');
  const saveFailed = linked && state.notice === 'save_failed';
  useEffect(() => {
    if (linked && !saveFailed) onDone();
  }, [linked, saveFailed, onDone]);

  const name = state?.serverName !== undefined && state.serverName !== '' ? state.serverName : hostOf(origin);
  const host = hostOf(origin);
  useEffect(() => {
    onTitle(t('desktop.setup.link.title', { name, host }));
  }, [onTitle, t, name, host]);

  if (failed !== null) {
    return <Notice>{failureText(t, { code: failed, params: {} }, appVersion)}</Notice>;
  }
  if (state === null) {
    return (
      <div className={styles.center}>
        <Spinner />
      </div>
    );
  }

  if (password && !linked) {
    return (
      <div className={styles.stack}>
        <TrustLine />
        <PasswordForm
          bridge={bridge}
          appVersion={appVersion}
          onLinked={(s) => {
            setPassword(false);
            set(s);
          }}
          onBack={() => {
            setPassword(false);
            startLink();
          }}
        />
      </div>
    );
  }

  const passwordLink = (
    <button
      type="button"
      className={styles.linkButton}
      onClick={() => {
        setPassword(true);
      }}
    >
      {t('desktop.setup.link.password')}
    </button>
  );

  if (linked) {
    return (
      <div className={styles.stack}>
        <p role="status">{t('desktop.setup.link.linked', { username: state.user?.username ?? '' })}</p>
        {saveFailed && (
          <>
            <Notice>{t('desktop.setup.saveFailed')}</Notice>
            <Actions>
              <Button variant="primary" onClick={onDone}>
                {t('desktop.setup.continue')}
              </Button>
            </Actions>
          </>
        )}
      </div>
    );
  }

  if (state.status === 'incompatible') {
    return (
      <div className={styles.stack}>
        <Notice>{compatText(t, state.compat, appVersion)}</Notice>
        <Actions>
          <Button
            variant="primary"
            loading={rechecking}
            onClick={() => {
              setRechecking(true);
              bridge.auth
                .recheck()
                .then(set, (err: unknown) => {
                  setError(failureOf(err));
                })
                .finally(() => {
                  setRechecking(false);
                });
            }}
          >
            {t('desktop.setup.tryAgain')}
          </Button>
          <Button onClick={onOtherServer}>{t('desktop.setup.link.otherServer')}</Button>
        </Actions>
        {error !== null && <Notice>{failureText(t, error, appVersion)}</Notice>}
      </div>
    );
  }

  if (state.status === 'linking' && state.link !== null) {
    return (
      <CodeView
        link={state.link}
        onOpen={openExternal}
        onCancel={() => {
          void bridge.auth.cancelLink().catch(() => undefined);
        }}
        passwordLink={passwordLink}
      />
    );
  }

  // unlinked (or linking without a code yet): what ended the last link, if anything, and how to start one.
  let message: string | null = null;
  let action = t('desktop.setup.link.start');
  if (error !== null) {
    // A Link() error has its own text only for these codes; any other (02's ErrBadResponse is internal
    // {reason: bad_response}) is the generic "Couldn't start linking with this server" (03 §8.2).
    message = LINK_ERROR_CODES.has(error.code) ? failureText(t, error, appVersion) : t('desktop.setup.link.failed');
    action = t('desktop.setup.tryAgain');
  } else if (state.notice === 'denied') {
    message = t('desktop.setup.link.denied');
    action = t('desktop.setup.tryAgain');
  } else if (state.notice === 'code_expired') {
    message = t('desktop.setup.link.expired');
    action = t('desktop.setup.link.newCode');
  }
  if (starting || state.status === 'linking') {
    return (
      <div className={styles.center}>
        <Spinner label={t('desktop.setup.link.starting')} />
      </div>
    );
  }
  return (
    <div className={styles.stack}>
      <TrustLine />
      {message !== null ? (
        <Notice>{message}</Notice>
      ) : (
        <Prose>
          <p>{t('desktop.setup.link.intro')}</p>
        </Prose>
      )}
      <Actions>
        <Button variant="primary" onClick={startLink}>
          {action}
        </Button>
        <Button onClick={onOtherServer}>{t('desktop.setup.link.otherServer')}</Button>
      </Actions>
      {passwordLink}
    </div>
  );
}

interface CodeViewProps {
  link: NonNullable<AuthState['link']>;
  onOpen: (url: string) => void;
  onCancel: () => void;
  passwordLink: ReactNode;
}

function CodeView({ link, onOpen, onCancel, passwordLink }: CodeViewProps) {
  const { t } = useTranslation();
  const now = useNow();
  const [copied, setCopied] = useState<'yes' | 'no' | null>(null);
  const short = link.verificationUri.replace(/^https?:\/\//, '');
  return (
    <div className={styles.stack}>
      <TrustLine />
      <div className={styles.codeBox}>
        <QrCode value={link.verificationUriComplete} label={t('desktop.setup.link.qr')} size={176} />
        <div className={styles.codeText}>
          <span className={styles.muted}>{t('desktop.setup.link.codeLabel')}</span>
          <span className={styles.code} data-testid="link-code">
            {link.userCode}
          </span>
          <span className={styles.muted}>
            {t('desktop.setup.link.expires', { time: formatLeft(link.expiresAt, now) })}
          </span>
        </div>
      </div>
      <p>{t('desktop.setup.link.short', { url: short, code: link.userCode })}</p>
      <Actions>
        <Button
          variant="primary"
          onClick={() => {
            onOpen(link.verificationUriComplete);
          }}
        >
          {t('desktop.setup.link.openAgain')}
        </Button>
        <Button
          onClick={() => {
            const clip = globalThis.navigator.clipboard as Clipboard | undefined;
            (clip ? clip.writeText(link.verificationUriComplete) : Promise.reject(new Error('no clipboard'))).then(
              () => {
                setCopied('yes');
              },
              () => {
                setCopied('no');
              },
            );
          }}
        >
          {t('desktop.setup.link.copy')}
        </Button>
        <Button onClick={onCancel}>{t('desktop.setup.cancel')}</Button>
      </Actions>
      {copied !== null && (
        <p role="status" className={styles.muted}>
          {copied === 'yes' ? t('desktop.setup.link.copied') : t('desktop.setup.link.copyFailed')}
        </p>
      )}
      <p className={styles.muted}>{t('desktop.setup.link.rename')}</p>
      {passwordLink}
    </div>
  );
}
