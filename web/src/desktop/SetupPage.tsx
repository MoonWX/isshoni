// /desktop/setup (docs/m2/03 §8.2): the first-run wizard's server, add, confirm and link steps. The sound and voice
// steps of the first install (§8.4, DesktopFeatures.wizardSteps()) come with README WA77; until then the wizard ends at
// the link step's "Linked", and goes to the room.
//
// - server: no server came with a link, --server or PendingServer: "Installed from your isshoni page? Go back to it
//   and click Open isshoni", and the address field as the fallback. Enter probes (Auth.Probe); an invite or setup link
//   gets the "open it in your browser first" hint instead. A link that arrives while it is open opens the add step.
// - add (?add=<origin>, or App.PendingLink()'s {add}): "Add server {host}?" before any network request (02 §12.3);
//   only Add probes. The same host as shown goes straight to Auth.AddServer; another host gets the confirm step.
// - confirm (an address the friend typed, or an add whose server calls itself another host): the probe's name and
//   host, "This server calls itself …" when it differs, the trust line, [Link] → Auth.AddServer.
// - Auth.AddServer makes the server active and Go reloads the window: the page puts /desktop/setup?link=1 into its
//   address first (Shell.Reload reloads the window's address), so the reloaded page, with the new server's bridge.js,
//   drops ?link=1 and calls Auth.Link() once (LinkStep). After addServer this page makes no other bridge call.
// - link: LinkStep, for the active server.
// Cancel on the first server leaves an empty "Link isshoni to your server" page with [Start].
import { useCallback, useEffect, useRef, useState, type SyntheticEvent } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { useLocation, useNavigate } from 'react-router';

import { useApp, usePlatform } from '../app/context';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { focusPageHeading } from '../app/layouts/RootLayout';
import { NotFound } from '../app/NotFound';
import { Actions, Prose } from '../auth/AuthForm';
import { Notice } from '../auth/Notice';
import { DOCS_URL } from '../conntest/links';
import type { DesktopFeatures } from '../platform/types';
import type { PendingLink, ServerProbe } from '../platform/desktop/bridge';
import { queryKeys } from '../protocol/queryKeys';
import { Button } from '../ui/Button';
import { TextField } from '../ui/Field';
import { Spinner } from '../ui/Spinner';
import { hostOf, inviteLink } from './address';
import { failureOf, failureText, type BridgeFailure } from './authText';
import { LinkStep } from './LinkStep';
import { hostLabel, TrustLine } from './parts';
import styles from './wizard.module.css';

/** The site's section on a server that opens in the browser but not in the app (03 §8.1, 02 DA12). */
export const DESKTOP_LINKING_HELP = `${DOCS_URL}troubleshooting#desktop-linking`;

/** The server of an add question: from the deeplink.open payload or App.PendingLink() (Go's deeplink fields). */
interface AddTarget {
  origin: string;
  host: string;
  hostUnicode: string;
  roomId: string;
}

type Step =
  | { kind: 'loading' }
  | { kind: 'start' }
  | { kind: 'server'; error?: BridgeFailure }
  | { kind: 'add'; target: AddTarget }
  | { kind: 'confirm'; probe: ServerProbe; shown: string; add: AddTarget | null }
  | { kind: 'switching' }
  | { kind: 'link'; autoLink: boolean };

/** Every shape Go's PendingLink can have (03 §6.3); bridge.ts names the two the router needs. */
type AnyPendingLink = PendingLink & { error?: string; switch?: AddTarget };

function addOf(p: AnyPendingLink | null | undefined): AddTarget | null {
  return p != null && 'add' in p ? p.add : null;
}

export function SetupPage() {
  const platform = usePlatform();
  const desktop = platform.desktop ?? null;
  // RequireDesktop renders this page only in the desktop app.
  if (desktop === null) return <NotFound />;
  return <Wizard desktop={desktop} origin={platform.serverOrigin} />;
}

function Wizard({ desktop, origin }: { desktop: DesktopFeatures; origin: string }) {
  const { t } = useTranslation();
  const { platform, queryClient } = useApp();
  const location = useLocation();
  const navigate = useNavigate();
  const { bridge } = desktop;
  const appVersion = desktop.app.version;
  // ?link=1: the page Go reloaded after Auth.AddServer; its link step calls Auth.Link() once (03 §8.2).
  const [step, setStep] = useState<Step>(() =>
    new URLSearchParams(location.search).get('link') === '1' && origin !== ''
      ? { kind: 'link', autoLink: true }
      : { kind: 'loading' },
  );
  const [title, setTitle] = useState('');
  /** What the last Auth.AddServer refused, shown on the step it came from. */
  const [failure, setFailure] = useState<BridgeFailure | null>(null);
  /** Counts AddServer's refusals: the add step starts again with the new one (its own state is per mount). */
  const [refusals, setRefusals] = useState(0);

  // The first step, once: from the address (?link=1, ?add=, ?new=1) and App.PendingLink() (the local link that
  // started the app, --server or PendingServer). No other bridge call before the friend answers (02 §12.3).
  const started = useRef(false);
  useEffect(() => {
    if (started.current) return;
    started.current = true;
    const params = new URLSearchParams(location.search);
    if (params.get('link') === '1' && origin !== '') {
      // The parameter leaves the address: a reload later is no new Add.
      params.delete('link');
      const rest = params.toString();
      void navigate({ pathname: location.pathname, search: rest === '' ? '' : `?${rest}` }, { replace: true });
      return;
    }
    const wanted = params.get('add');
    const fresh = params.get('new') === '1';
    void bridge.app.pendingLink().then(
      (p: AnyPendingLink | null | undefined) => {
        const add = addOf(p);
        if (wanted !== null && wanted !== '') {
          const host = hostOf(wanted);
          setStep({
            kind: 'add',
            target:
              add !== null && add.origin === wanted
                ? add
                : { origin: wanted, host, hostUnicode: host, roomId: add?.roomId ?? '' },
          });
        } else if (add !== null) {
          setStep({ kind: 'add', target: add });
        } else if (p?.error === 'too_many_servers') {
          setStep({ kind: 'server', error: { code: 'too_many_servers', params: {} } });
        } else if (origin === '' || fresh) {
          setStep({ kind: 'server' });
        } else {
          setStep({ kind: 'link', autoLink: false });
        }
      },
      () => {
        setStep(origin === '' || fresh ? { kind: 'server' } : { kind: 'link', autoLink: false });
      },
    );
  }, [bridge, location.pathname, location.search, navigate, origin]);

  // A link that arrives while the server step is open opens the add step (03 §8.2); during any question or code,
  // Go ignores links itself (§13.2).
  useEffect(() => {
    if (step.kind !== 'server' && step.kind !== 'start') return undefined;
    return bridge.on('deeplink.open', (p: AnyPendingLink) => {
      const add = addOf(p);
      if (add !== null) setStep({ kind: 'add', target: add });
      else if (p.error === 'too_many_servers') setStep({ kind: 'server', error: { code: p.error, params: {} } });
    });
  }, [bridge, step.kind]);

  // Each step's heading takes focus (05 §16.6), so a screen reader starts at it.
  useEffect(() => {
    // The server step's field takes the focus itself (03 §8.2).
    if (step.kind !== 'loading' && step.kind !== 'server') focusPageHeading();
  }, [step.kind]);

  /** Auth.AddServer: the server becomes active and the window reloads at /desktop/setup?link=1 (03 §8.2). */
  const activate = (target: string, back: Step): void => {
    const before = globalThis.location.pathname + globalThis.location.search + globalThis.location.hash;
    globalThis.history.replaceState(globalThis.history.state, '', '/desktop/setup?link=1');
    setStep({ kind: 'switching' });
    bridge.auth.addServer(target).then(
      () => {
        // The reload follows; this page asks nothing more.
      },
      (err: unknown) => {
        globalThis.history.replaceState(globalThis.history.state, '', before);
        setFailure(failureOf(err));
        setRefusals((n) => n + 1);
        setStep(back);
      },
    );
  };

  /** The add question was answered with Cancel: Go may take the next link (App.LinkHandled). */
  const cancelAdd = (): void => {
    void bridge.app.linkHandled().catch(() => undefined);
    if (origin !== '') void navigate('/', { replace: true });
    else setStep({ kind: 'start' });
  };

  const done = useCallback((): void => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.me, exact: true });
    void bridge.app.pendingLink().then(
      (p: AnyPendingLink | null | undefined) => {
        const room = p != null && 'roomId' in p ? p.roomId : '';
        if (room !== '') {
          void bridge.app.linkHandled().catch(() => undefined);
          void navigate(`/r/${encodeURIComponent(room)}`, { replace: true });
        } else {
          void navigate('/', { replace: true });
        }
      },
      () => {
        void navigate('/', { replace: true });
      },
    );
  }, [bridge, navigate, queryClient]);

  let heading = t('desktop.setup.title');
  let body;
  switch (step.kind) {
    case 'loading':
    case 'switching':
      body = (
        <div className={styles.center}>
          <Spinner />
        </div>
      );
      break;
    case 'start':
      body = (
        <Actions>
          <Button
            variant="primary"
            onClick={() => {
              setStep({ kind: 'server' });
            }}
          >
            {t('desktop.setup.start')}
          </Button>
        </Actions>
      );
      break;
    case 'server':
      body = (
        <ServerStep
          initialError={step.error ?? null}
          appVersion={appVersion}
          probe={(address) => bridge.auth.probe(address)}
          openExternal={(url) => {
            platform.openExternal(url);
          }}
          onProbed={(probe, shown) => {
            setFailure(null);
            setStep({ kind: 'confirm', probe, shown, add: null });
          }}
          onCancel={
            origin !== ''
              ? () => {
                  void navigate('/', { replace: true });
                }
              : () => {
                  setStep({ kind: 'start' });
                }
          }
        />
      );
      break;
    case 'add':
      heading = t('desktop.setup.add.title', { host: hostLabel(step.target.host, step.target.hostUnicode) });
      body = (
        <AddStep
          key={refusals}
          target={step.target}
          initialError={failure}
          appVersion={appVersion}
          probe={(address) => bridge.auth.probe(address)}
          onSameHost={(probe) => {
            activate(probe.origin, step);
          }}
          onOtherHost={(probe) => {
            setFailure(null);
            setStep({ kind: 'confirm', probe, shown: step.target.host, add: step.target });
          }}
          onCancel={() => {
            setFailure(null);
            cancelAdd();
          }}
        />
      );
      break;
    case 'confirm': {
      const host = hostOf(step.probe.origin);
      heading = t('desktop.setup.confirm.title', { name: step.probe.name || host, host });
      body = (
        <div className={styles.stack}>
          {host !== step.shown && <p className={styles.host}>{t('desktop.setup.confirm.callsItself', { host })}</p>}
          <TrustLine />
          {failure !== null && <Notice>{failureText(t, failure, appVersion)}</Notice>}
          <Actions>
            <Button
              variant="primary"
              onClick={() => {
                // An add question is confirmed under its own origin, so Go answers it even when the server calls
                // itself another host (Servers.Add adopts that host's publicUrl all the same).
                activate(step.add?.origin ?? step.probe.origin, step);
              }}
            >
              {t('desktop.setup.confirm.link')}
            </Button>
            <Button
              onClick={() => {
                setFailure(null);
                if (step.add !== null) cancelAdd();
                else setStep({ kind: 'server' });
              }}
            >
              {t('desktop.setup.cancel')}
            </Button>
          </Actions>
        </div>
      );
      break;
    }
    case 'link':
      heading = title !== '' ? title : t('desktop.setup.title');
      body = (
        <LinkStep
          bridge={bridge}
          origin={origin}
          appVersion={appVersion}
          autoLink={step.autoLink}
          onDone={done}
          openExternal={(url) => {
            platform.openExternal(url);
          }}
          onOtherServer={() => {
            setStep({ kind: 'server' });
          }}
          onTitle={setTitle}
        />
      );
      break;
  }

  return (
    <CenteredPage title={heading} width="lg">
      {body}
    </CenteredPage>
  );
}

interface ServerStepProps {
  initialError: BridgeFailure | null;
  appVersion: string;
  probe: (address: string) => Promise<ServerProbe>;
  openExternal: (url: string) => void;
  onProbed: (probe: ServerProbe, shownHost: string) => void;
  onCancel: () => void;
}

/** The address field (03 §8.1): a host or any URL of the server; Enter probes. Errors show under the field. */
function ServerStep({ initialError, appVersion, probe, openExternal, onProbed, onCancel }: ServerStepProps) {
  const { t } = useTranslation();
  const [address, setAddress] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<BridgeFailure | null>(initialError);
  const [invite, setInvite] = useState<string | null>(null);

  const submit = (e: SyntheticEvent): void => {
    e.preventDefault();
    if (busy) return;
    setError(null);
    const inv = inviteLink(address);
    setInvite(inv);
    if (inv !== null) return;
    if (address.trim() === '') {
      setError({ code: 'invalid_address', params: {} });
      return;
    }
    setBusy(true);
    probe(address.trim()).then(
      (p) => {
        setBusy(false);
        onProbed(p, typedHost(address));
      },
      (err: unknown) => {
        setBusy(false);
        setError(failureOf(err));
      },
    );
  };

  const fieldError =
    error !== null ? (
      <>
        {failureText(t, error, appVersion)}
        {error.code === 'server_unreachable' && (
          <>
            {' '}
            <button
              type="button"
              className={styles.linkButton}
              onClick={() => {
                openExternal(DESKTOP_LINKING_HELP);
              }}
            >
              {t('desktop.setup.server.seeWhy')}
            </button>
          </>
        )}
      </>
    ) : undefined;

  return (
    <form className={styles.form} noValidate onSubmit={submit}>
      <Prose>
        <p>
          <Trans i18nKey="desktop.setup.server.fromPage" components={{ strong: <strong /> }} />
        </p>
      </Prose>
      <TextField
        label={t('desktop.setup.server.label')}
        placeholder={t('desktop.setup.server.placeholder')}
        // The wizard's one field: it takes focus (03 §8.2).
        // eslint-disable-next-line jsx-a11y/no-autofocus -- the step's only control
        autoFocus
        inputMode="url"
        autoComplete="off"
        autoCapitalize="none"
        autoCorrect="off"
        spellCheck={false}
        value={address}
        error={fieldError}
        onChange={(e) => {
          setAddress(e.target.value);
          setInvite(null);
        }}
      />
      {invite !== null && (
        <Notice
          tone="info"
          action={
            <Button
              onClick={() => {
                openExternal(invite);
              }}
            >
              {t('desktop.setup.server.openInvite')}
            </Button>
          }
        >
          {t('desktop.setup.server.inviteHint')}
        </Notice>
      )}
      <Actions>
        <Button type="submit" variant="primary" loading={busy}>
          {t('desktop.setup.continue')}
        </Button>
        <Button onClick={onCancel} disabled={busy}>
          {t('desktop.setup.cancel')}
        </Button>
      </Actions>
    </form>
  );
}

/** The host the friend typed, to compare with the probe's (03 §8.1: "This server calls itself …"). */
function typedHost(address: string): string {
  const s = address.trim();
  return hostOf(/^[a-z][a-z\d+.-]*:\/\//i.test(s) ? s : `https://${s}`);
}

interface AddStepProps {
  target: AddTarget;
  initialError: BridgeFailure | null;
  appVersion: string;
  probe: (address: string) => Promise<ServerProbe>;
  onSameHost: (probe: ServerProbe) => void;
  onOtherHost: (probe: ServerProbe) => void;
  onCancel: () => void;
}

/** "Add server {host}?" (03 §8.2): nothing goes over the network until Add. */
function AddStep({ target, initialError, appVersion, probe, onSameHost, onOtherHost, onCancel }: AddStepProps) {
  const { t } = useTranslation();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<BridgeFailure | null>(initialError);
  const add = (): void => {
    if (busy) return;
    setBusy(true);
    setError(null);
    probe(target.origin).then(
      (p) => {
        setBusy(false);
        if (hostOf(p.origin) === target.host) onSameHost(p);
        else onOtherHost(p);
      },
      (err: unknown) => {
        setBusy(false);
        setError(failureOf(err));
      },
    );
  };
  return (
    <div className={styles.stack}>
      <p className={styles.host}>{hostLabel(target.host, target.hostUnicode)}</p>
      <p className={styles.trust}>{t('desktop.setup.add.trust')}</p>
      {error !== null && <Notice>{failureText(t, error, appVersion)}</Notice>}
      <Actions>
        <Button variant="primary" loading={busy} onClick={add}>
          {error !== null ? t('desktop.setup.tryAgain') : t('desktop.setup.add.add')}
        </Button>
        <Button onClick={onCancel} disabled={busy}>
          {t('desktop.setup.cancel')}
        </Button>
      </Actions>
    </div>
  );
}
