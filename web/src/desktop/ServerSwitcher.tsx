// The server switcher (docs/m2/03 §8.1, §8.3, §14.4): the servers of servers.json (Auth.Servers()), with Switch,
// Sign out of the active one, Link again for the active one when it isn't linked (Auth.Link(), then the link step)
// and Remove, and "Add a server" (the wizard's server step, ?new=1). The relink page shows it for "Switch server";
// Settings → Servers (#servers) is this list too, which the account menu's Switch server and the Offline screen's
// link open.
//
// Every change of the active server reloads the window (Auth.SwitchServer, Auth.RemoveServer of the active server):
// the page puts "/" into its address first, so the reloaded page starts at the new server's room. Sign out keeps the
// server as "Not linked" (Auth.Unlink; a share on it is stopped by Go first): its page follows auth.state to the link
// step. When the server wasn't reached, the device is still on the user's Devices page: "Remove {device} on your
// Devices page to finish".
import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router';

import { Notice } from '../auth/Notice';
import type { NativeBridge, ServerInfo } from '../platform/desktop/bridge';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import { Spinner } from '../ui/Spinner';
import { hostOf } from './address';
import { failureOf, failureText, type BridgeFailure } from './authText';
import styles from './wizard.module.css';

export interface ServerSwitcherProps {
  bridge: NativeBridge;
  appVersion: string;
}

/** A question the switcher asks before it acts. */
type Question = { kind: 'signOut' | 'remove'; server: ServerInfo };

/** Where the reloaded window starts after a change of the active server: the new server's room. */
function reloadAtStart(): void {
  globalThis.history.replaceState(globalThis.history.state, '', '/');
}

export function ServerSwitcher({ bridge, appVersion }: ServerSwitcherProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [servers, setServers] = useState<ServerInfo[] | null>(null);
  const [error, setError] = useState<BridgeFailure | null>(null);
  const [busy, setBusy] = useState(false);
  const [question, setQuestion] = useState<Question | null>(null);
  const [unconfirmed, setUnconfirmed] = useState<string | null>(null);

  const load = useCallback(() => {
    bridge.auth.servers().then(
      (s) => {
        setServers(Array.isArray(s) ? s : []);
      },
      (err: unknown) => {
        setError(failureOf(err));
        setServers([]);
      },
    );
  }, [bridge]);
  useEffect(() => {
    load();
  }, [load]);

  /** Runs a change; a refusal shows its text, and the list is read again. */
  const run = (fn: () => Promise<unknown>, before?: () => void): void => {
    if (busy) return;
    setBusy(true);
    setError(null);
    setUnconfirmed(null);
    const where = globalThis.location.pathname + globalThis.location.search + globalThis.location.hash;
    before?.();
    fn()
      .catch((err: unknown) => {
        if (before) globalThis.history.replaceState(globalThis.history.state, '', where);
        setError(failureOf(err));
      })
      .finally(() => {
        setBusy(false);
        load();
      });
  };

  const name = (s: ServerInfo): string => (s.name !== '' ? s.name : hostOf(s.origin));

  if (servers === null) {
    return (
      <div className={styles.center}>
        <Spinner />
      </div>
    );
  }

  const answer = (q: Question): void => {
    setQuestion(null);
    if (q.kind === 'remove') {
      run(() => bridge.auth.removeServer(q.server.origin), q.server.active ? reloadAtStart : undefined);
      return;
    }
    const device = q.server.device?.name ?? '';
    run(() =>
      bridge.auth.unlink().then((r) => {
        if (!r.confirmed) setUnconfirmed(device);
      }),
    );
  };

  return (
    <section className={styles.stack} aria-labelledby="desktop-servers-heading">
      <h2 id="desktop-servers-heading">{t('desktop.servers.title')}</h2>
      {error !== null && <Notice>{failureText(t, error, appVersion)}</Notice>}
      {unconfirmed !== null && (
        <Notice tone="info">{t('desktop.servers.signOutUnconfirmed', { device: unconfirmed })}</Notice>
      )}
      <ul className={styles.servers}>
        {servers.map((s) => (
          <li key={s.origin} className={styles.server}>
            <div className={styles.serverName}>
              <strong>{name(s)}</strong>
              <span className={styles.muted}>
                {hostOf(s.origin)}
                {' · '}
                {s.linked && s.user
                  ? t('desktop.servers.linkedAs', { username: s.user.username })
                  : t('desktop.servers.notLinked')}
                {s.active && ` · ${t('desktop.servers.active')}`}
              </span>
            </div>
            <div className={styles.serverActions}>
              {!s.active && (
                <Button
                  size="sm"
                  disabled={busy}
                  onClick={() => {
                    run(() => bridge.auth.switchServer(s.origin), reloadAtStart);
                  }}
                >
                  {t('desktop.servers.switch')}
                </Button>
              )}
              {s.active && !s.linked && (
                <Button
                  size="sm"
                  variant="primary"
                  disabled={busy}
                  onClick={() => {
                    // The link step of the active server shows the code that this call starts.
                    run(() =>
                      bridge.auth.link().then(() => {
                        void navigate('/desktop/setup');
                      }),
                    );
                  }}
                >
                  {t('desktop.servers.linkAgain')}
                </Button>
              )}
              {s.active && s.linked && (
                <Button
                  size="sm"
                  disabled={busy}
                  onClick={() => {
                    setQuestion({ kind: 'signOut', server: s });
                  }}
                >
                  {t('desktop.servers.signOut', { server: name(s) })}
                </Button>
              )}
              <Button
                size="sm"
                variant="danger"
                disabled={busy}
                onClick={() => {
                  setQuestion({ kind: 'remove', server: s });
                }}
              >
                {t('desktop.servers.remove')}
              </Button>
            </div>
          </li>
        ))}
      </ul>
      <div>
        <Button
          disabled={busy}
          onClick={() => {
            void navigate('/desktop/setup?new=1');
          }}
        >
          {t('desktop.servers.add')}
        </Button>
      </div>
      <Dialog
        open={question !== null}
        size="sm"
        onClose={() => {
          setQuestion(null);
        }}
        title={
          question === null
            ? ''
            : question.kind === 'signOut'
              ? t('desktop.servers.signOutConfirm', { server: name(question.server) })
              : t('desktop.servers.removeConfirm', { server: name(question.server) })
        }
        footer={
          question !== null && (
            <>
              <Button
                onClick={() => {
                  setQuestion(null);
                }}
              >
                {t('desktop.setup.cancel')}
              </Button>
              <Button
                variant="danger"
                onClick={() => {
                  answer(question);
                }}
              >
                {question.kind === 'signOut' ? t('desktop.servers.signOutAction') : t('desktop.servers.remove')}
              </Button>
            </>
          )
        }
      >
        {question?.kind === 'remove' && <p>{t('desktop.servers.removeBody')}</p>}
        {question?.kind === 'signOut' && <p>{t('desktop.servers.signOutBody')}</p>}
      </Dialog>
    </section>
  );
}
