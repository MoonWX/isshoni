// The connection test's UI (05 §14.2): the wizard's step 2, the doctor page and "Test my connection" all render
// this panel. It runs runConnTest and shows:
// - a row per transport (✓, ✗ or "not tested"; a TCP transport the server has turned off gets no row) and the round
//   trip;
// - the status (green, amber, red), or "Couldn't finish the test" when a probe could not run;
// - for admins the fix text (fixText.ts); for everyone else "Send this to your admin" with the result to copy;
// - links to the project site's troubleshooting sections, and "Copy result" (the ConnTestResult as JSON, which holds
//   no IP address).
// Who is an admin comes from ['me']: the server sends its provider and NAT kind to every signed-in user, but only
// admins can act on them.
import { useMutation } from '@tanstack/react-query';
import type { TFunction } from 'i18next';
import { Check, CircleCheck, CircleX, Copy, ExternalLink, Minus, TriangleAlert, X } from 'lucide-react';
import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import { Trans, useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { isAdmin } from '../app/guards';
import { useMeQuery } from '../app/me';
import { CommandBlock } from '../app/screens/ScreenFrame';
import { copyText } from '../lib/clipboard';
import { errorMessage } from '../lib/errorText';
import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { Spinner } from '../ui/Spinner';
import styles from './ConnTestPanel.module.css';
import { fixText, type FixLine, type FixText } from './fixText';
import { troubleshootingUrl, vpsGuideUrl, type CtCode } from './links';
import { runConnTest, type ConnTestResult } from './runConnTest';
import { verdictOf, type ConnTestStatus, type ConnTestVerdict, type RowState, type TransportRow } from './verdict';

export interface ConnTestPanelProps {
  /** Run the test as soon as the panel shows (the wizard's step 2). Default: wait for the button. */
  autoStart?: boolean;
  /**
   * Called with every finished run. The wizard uses it to make "Continue" secondary to "Test again" after a
   * failure (05 §14.1).
   */
  onResult?: (result: ConnTestResult, verdict: ConnTestVerdict) => void;
  /** The test itself; default runConnTest. Tests pass canned results. */
  runner?: typeof runConnTest;
  className?: string;
}

function rowLabel(t: TFunction, row: TransportRow): string {
  if (row.transport !== 'udp') return t('conntest.rows.tcp', { port: row.port });
  return row.port === undefined ? t('conntest.rows.udpNoPort') : t('conntest.rows.udp', { port: row.port });
}

function stateText(t: TFunction, state: RowState): string {
  switch (state) {
    case 'ok':
      return t('conntest.state.ok');
    case 'failed':
      return t('conntest.state.failed');
    case 'off':
      return t('conntest.state.off');
    case 'not_tested':
      return t('conntest.state.not_tested');
  }
}

function statusText(t: TFunction, status: ConnTestStatus): string {
  switch (status) {
    case 'green':
      return t('conntest.status.green');
    case 'amber':
      return t('conntest.status.amber');
    case 'red':
      return t('conntest.status.red');
  }
}

/** "Couldn't finish the test", with the server's wait when it gave one. */
function incompleteText(t: TFunction, retryAfterSec: number | undefined): string {
  return retryAfterSec !== undefined && retryAfterSec > 0
    ? t('conntest.incompleteWait', { count: Math.ceil(retryAfterSec) })
    : t('conntest.incomplete');
}

function rttText(t: TFunction, verdict: ConnTestVerdict, lang: string): string {
  if (verdict.rttMs === undefined) return t('conntest.rtt.none');
  const ms = new Intl.NumberFormat(lang, { maximumFractionDigits: verdict.rttMs < 10 ? 1 : 0 }).format(verdict.rttMs);
  switch (verdict.rttLabel) {
    case 'good':
      return t('conntest.rtt.good', { ms });
    case 'ok':
      return t('conntest.rtt.ok', { ms });
    default:
      return t('conntest.rtt.high', { ms });
  }
}

/** The link text of a troubleshooting section. */
function helpLabel(t: TFunction, code: CtCode): string {
  switch (code) {
    case 'udp_blocked':
      return t('conntest.help.udp_blocked');
    case 'no_media':
      return t('conntest.help.no_media');
    case 'no_public_ip':
      return t('conntest.help.no_public_ip');
    case 'tcp443_blocked':
      return t('conntest.help.tcp443_blocked');
    case 'high_rtt':
      return t('conntest.help.high_rtt');
    case 'nat_port_forward':
      return t('conntest.help.nat_port_forward');
    case 'nat_cgnat':
      return t('conntest.help.nat_cgnat');
    case 'mac_local_network':
      return t('conntest.help.mac_local_network');
    case 'docker_ports':
      return t('conntest.help.docker_ports');
  }
}

/** A link to the project site (05 §20: external links never pass the opener or the referrer). */
function SiteLink({ href, children }: { href: string; children: ReactNode }) {
  return (
    <a className={styles.link} href={href} target="_blank" rel="noopener noreferrer">
      <span>{children}</span>
      <ExternalLink aria-hidden="true" />
    </a>
  );
}

const MARKS: Readonly<Record<RowState, ReactNode>> = {
  ok: <Check />,
  failed: <X />,
  off: <X />,
  not_tested: <Minus />,
};

function Row({ row }: { row: TransportRow }) {
  const { t } = useTranslation();
  return (
    <li className={styles.row}>
      <span className={cx(styles.mark, styles[row.state])} aria-hidden="true">
        {MARKS[row.state]}
      </span>
      <span className={styles.rowLabel}>{rowLabel(t, row)}</span>
      <span className={styles.rowState}>{stateText(t, row.state)}</span>
      {row.transport === 'udp' && row.state === 'off' && (
        <p className={styles.rowNote}>
          <Trans i18nKey="conntest.udpDisabled" components={{ code: <code /> }} />
        </p>
      )}
    </li>
  );
}

function FixItem({ line }: { line: FixLine }) {
  const { t } = useTranslation();
  return (
    <li className={styles.fixItem}>
      <p>
        {/* The key comes from fixText.ts: check:i18n's rule 3 and fixText.test.ts cover what t('…') literals can't. */}
        <Trans i18nKey={line.key} values={line.values} components={{ code: <code /> }} />
      </p>
      {line.command !== undefined && <CommandBlock>{line.command}</CommandBlock>}
      {line.provider !== undefined && <SiteLink href={vpsGuideUrl(line.provider)}>{t('fix.providerGuide')}</SiteLink>}
      {line.code !== undefined && <SiteLink href={troubleshootingUrl(line.code)}>{helpLabel(t, line.code)}</SiteLink>}
    </li>
  );
}

/** "Copy result": the result as JSON. When the browser refuses the clipboard, the text is shown to copy by hand. */
function CopyResult({ result }: { result: ConnTestResult }) {
  const { t } = useTranslation();
  const [state, setState] = useState<'idle' | 'copied' | 'manual'>('idle');
  const fieldId = useId();
  const json = useMemo(() => JSON.stringify(result, null, 2), [result]);
  useEffect(() => {
    if (state !== 'copied') return undefined;
    const id = setTimeout(() => {
      setState('idle');
    }, 2000);
    return () => {
      clearTimeout(id);
    };
  }, [state]);
  return (
    <>
      <Button
        icon={state === 'copied' ? <Check /> : <Copy />}
        onClick={() => {
          void copyText(json).then((ok) => {
            setState(ok ? 'copied' : 'manual');
          });
        }}
      >
        {state === 'copied' ? t('common.copied') : t('conntest.copyResult')}
      </Button>
      {state === 'manual' && (
        <div className={styles.manual}>
          <label htmlFor={fieldId}>{t('conntest.copyManually')}</label>
          <textarea
            id={fieldId}
            readOnly
            rows={8}
            value={json}
            spellCheck={false}
            onFocus={(e) => {
              e.currentTarget.select();
            }}
          />
        </div>
      )}
    </>
  );
}

const STATUS_ICONS: Readonly<Record<ConnTestStatus, ReactNode>> = {
  green: <CircleCheck />,
  amber: <TriangleAlert />,
  red: <CircleX />,
};

function Result({ result, verdict, fix }: { result: ConnTestResult; verdict: ConnTestVerdict; fix: FixText }) {
  const { t, i18n } = useTranslation();
  const fixTitleId = useId();
  const helpTitleId = useId();
  const hint = fix.lines.find((l) => l.id === 'ownNetwork');
  const lines = fix.lines.filter((l) => l.id !== 'ownNetwork');
  const connected = verdict.rows.some((r) => r.state === 'ok');
  return (
    <>
      <ul className={styles.rows}>
        {verdict.rows.map((row) => (
          <Row key={row.transport} row={row} />
        ))}
        {connected && (
          <li className={styles.row}>
            <span className={styles.mark} aria-hidden="true" />
            <span className={styles.rowLabel}>{t('conntest.rows.rtt')}</span>
            <span className={styles.rowState}>{rttText(t, verdict, i18n.language)}</span>
          </li>
        )}
      </ul>

      {verdict.status !== null && (
        <p className={cx(styles.status, styles[verdict.status])}>
          <span className={styles.statusIcon} aria-hidden="true">
            {STATUS_ICONS[verdict.status]}
          </span>
          <span>{statusText(t, verdict.status)}</span>
        </p>
      )}
      {verdict.incomplete && <p className={styles.incomplete}>{incompleteText(t, result.retryAfterSec)}</p>}

      {lines.length > 0 && (
        <section className={styles.fix} aria-labelledby={fixTitleId}>
          <h3 id={fixTitleId} className={styles.sectionTitle}>
            {t('conntest.fixTitle')}
          </h3>
          <ul className={styles.fixList}>
            {lines.map((line) => (
              <FixItem key={line.id} line={line} />
            ))}
          </ul>
        </section>
      )}
      {hint && <p className={styles.hint}>{t('conntest.ownNetwork')}</p>}

      {verdict.codes.length > 0 && (
        <section className={styles.help} aria-labelledby={helpTitleId}>
          <h3 id={helpTitleId} className={styles.sectionTitle}>
            {t('conntest.help.title')}
          </h3>
          <ul className={styles.helpList}>
            {verdict.codes.map((code) => (
              <li key={code}>
                <SiteLink href={troubleshootingUrl(code)}>{helpLabel(t, code)}</SiteLink>
              </li>
            ))}
          </ul>
        </section>
      )}

      {fix.sendToAdmin && (
        <div className={styles.send}>
          <p>{t('conntest.sendToAdmin')}</p>
          <CopyResult key={result.at} result={result} />
        </div>
      )}
    </>
  );
}

export function ConnTestPanel({ autoStart = false, onResult, runner = runConnTest, className }: ConnTestPanelProps) {
  const { t } = useTranslation();
  const { platform, ui } = useApp();
  const me = useMeQuery();
  const admin = me.data ? isAdmin(me.data) : false;

  /** The running test's controller: a new run or leaving the page stops the old one (its PCs close). */
  const controller = useRef<AbortController | null>(null);
  /** Date.now() until which the server asked not to be probed again (Retry-After), or null. */
  const [retryAt, setRetryAt] = useState<number | null>(null);
  /** A run has finished (with a result or not): from then on the button says "Test again". */
  const [ranBefore, setRanBefore] = useState(false);

  const run = useMutation({
    mutationFn: () => {
      controller.current?.abort();
      const ctl = new AbortController();
      controller.current = ctl;
      return runner(platform, { signal: ctl.signal });
    },
    onSuccess: (result) => {
      const verdict = verdictOf(result);
      const wait = result.retryAfterSec;
      setRetryAt(wait !== undefined && wait > 0 ? Date.now() + wait * 1000 : null);
      // Screen readers hear the outcome from the app's live region (05 §16.6).
      const said = [
        verdict.status !== null ? statusText(t, verdict.status) : '',
        verdict.incomplete ? incompleteText(t, wait) : '',
      ].filter((s) => s !== '');
      if (said.length > 0) ui.getState().announce(said.join(' '));
      onResult?.(result, verdict);
    },
    onSettled: () => {
      setRanBefore(true);
    },
  });
  const start = run.mutate;

  useEffect(() => {
    if (!autoStart) return undefined;
    // On a timer, so React's StrictMode double mount in development starts one run, not two.
    const id = setTimeout(() => {
      start();
    }, 0);
    return () => {
      clearTimeout(id);
    };
  }, [autoStart, start]);

  useEffect(
    () => () => {
      controller.current?.abort();
    },
    [],
  );

  useEffect(() => {
    if (retryAt === null) return undefined;
    const id = setTimeout(
      () => {
        setRetryAt(null);
      },
      Math.max(0, retryAt - Date.now()),
    );
    return () => {
      clearTimeout(id);
    };
  }, [retryAt]);

  const result = run.isPending ? undefined : run.data;
  const shown = useMemo(() => {
    if (!result) return null;
    const verdict = verdictOf(result);
    return { result, verdict, fix: fixText(result, verdict, { admin }) };
  }, [result, admin]);
  const failed = !run.isPending && run.isError;

  return (
    <section className={cx(styles.panel, className)} aria-label={t('conntest.title')} aria-busy={run.isPending}>
      {run.isIdle && <p className={styles.intro}>{t('conntest.intro')}</p>}
      {run.isPending && (
        <p className={styles.running}>
          <Spinner size="sm" label={null} />
          <span>{t('conntest.running')}</span>
        </p>
      )}
      {failed && (
        <p className={styles.error} role="alert">
          {t('conntest.error', { message: errorMessage(run.error, t) })}
        </p>
      )}
      {shown && <Result result={shown.result} verdict={shown.verdict} fix={shown.fix} />}

      <div className={styles.actions}>
        <Button
          // After a green result the host's own next step is the main action (the wizard's "Continue").
          variant={shown?.verdict.status === 'green' ? 'secondary' : 'primary'}
          loading={run.isPending}
          disabled={retryAt !== null}
          onClick={() => {
            start();
          }}
        >
          {ranBefore ? t('conntest.again') : t('conntest.start')}
        </Button>
        {/* A non-admin with a problem gets the button next to "Send this to your admin" instead. */}
        {shown && !shown.fix.sendToAdmin && <CopyResult key={shown.result.at} result={shown.result} />}
      </div>
    </section>
  );
}
