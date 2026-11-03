import { AppWindow, Check, Copy, Download, ExternalLink as ExternalLinkIcon } from 'lucide-react';
import { useEffect, useId, useRef, useState, type MouseEvent, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useSearchParams } from 'react-router';

import { useApp, usePlatform } from '../app/context';
import { useInfo } from '../app/info';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { Notice } from '../auth/Notice';
import { copyText } from '../lib/clipboard';
import { Button, buttonClass } from '../ui/Button';
import { cx } from '../ui/cx';
import { VisuallyHidden } from '../ui/VisuallyHidden';
import styles from './DownloadPage.module.css';
import { desktopLinks, desktopOrder, roomParam, SITE_LINKS, type DesktopLinks, type DesktopOS } from './links';

/** How long the Copy button says "Copied". */
const COPIED_MS = 2000;

interface ExternalLinkProps {
  href: string;
  children: ReactNode;
  /** Classes for a link that looks like a button; default: a text link with the external icon. */
  className?: string;
  /** A leading icon (lucide-react) for a button-like link. */
  icon?: ReactNode;
  /** Runs on every click, before the platform's own handling. */
  onClick?: () => void;
  /** Runs on a click of another button (auxclick: the middle button opens or saves the link without a click). */
  onAuxClick?: (e: MouseEvent<HTMLAnchorElement>) => void;
  /** Runs when the link's context menu opens (where "Save link as…" lives). */
  onContextMenu?: () => void;
}

/**
 * A link that leaves the SPA: a new tab, without opener or referrer (05 §20). Outside a browser tab (the desktop
 * app; RequireBrowser keeps it from rendering this page, 03 §7.5) the platform opens it in the system browser.
 */
function ExternalLink({ href, children, className, icon, onClick, onAuxClick, onContextMenu }: ExternalLinkProps) {
  const platform = usePlatform();
  const click = (e: MouseEvent<HTMLAnchorElement>): void => {
    onClick?.();
    if (platform.kind === 'browser') return;
    e.preventDefault();
    platform.openExternal(href);
  };
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className={className ?? styles.external}
      onClick={click}
      onAuxClick={onAuxClick}
      onContextMenu={onContextMenu}
    >
      {icon !== undefined && <span aria-hidden="true">{icon}</span>}
      <span>{children}</span>
      {icon === undefined && <ExternalLinkIcon aria-hidden="true" />}
    </a>
  );
}

/** One of the numbered Windows steps: a marker, its title as a heading for screen readers, and what to do. */
function Step({ n, title, children }: { n: number; title: string; children: ReactNode }) {
  const { t } = useTranslation();
  return (
    <li className={styles.step}>
      <span className={styles.marker} aria-hidden="true">
        {n}
      </span>
      <div className={styles.stepBody}>
        <h3 className={styles.stepTitle}>
          <VisuallyHidden>{t('a11y.stepOf', { current: n, total: 3 })} </VisuallyHidden>
          {title}
        </h3>
        {children}
      </div>
    </li>
  );
}

/** What the line under the command says after a click on Copy. */
type CopyFeedback = 'idle' | 'copied' | 'copyFailed';

/**
 * The PowerShell way (06 §7.1, §7.5): the server's pre-filled script installs the app without SmartScreen's
 * warning and starts it set up for this server. The clipboard is written only on a click (05 §20); when the browser
 * refuses, the command is selected for the friend's own copy.
 */
function PowerShellCommand({ command }: { command: string }) {
  const { t } = useTranslation();
  const { ui } = useApp();
  const headingId = useId();
  const code = useRef<HTMLPreElement>(null);
  const [feedback, setFeedback] = useState<CopyFeedback>('idle');

  useEffect(() => {
    if (feedback !== 'copied') return undefined;
    const id = setTimeout(() => {
      setFeedback('idle');
    }, COPIED_MS);
    return () => {
      clearTimeout(id);
    };
  }, [feedback]);

  const copy = async (): Promise<void> => {
    if (await copyText(command)) {
      setFeedback('copied');
      ui.getState().announce(t('account.download.windows.powerShell.copiedSpoken'));
      return;
    }
    setFeedback('copyFailed');
    if (code.current !== null) globalThis.getSelection()?.selectAllChildren(code.current);
  };

  return (
    <section className={styles.subsection} aria-labelledby={headingId}>
      <h3 id={headingId} className={styles.subheading}>
        {t('account.download.windows.powerShell.heading')}
      </h3>
      <p className={styles.muted}>{t('account.download.windows.powerShell.lead')}</p>
      <div className={styles.command}>
        <pre ref={code} className={styles.code} translate="no">
          <code>{command}</code>
        </pre>
        <Button
          icon={feedback === 'copied' ? <Check /> : <Copy />}
          onClick={() => {
            void copy();
          }}
        >
          {feedback === 'copied' ? t('common.copied') : t('common.copy')}
        </Button>
      </div>
      {/* Always there, so screen readers hear the text when it arrives. */}
      <p className={styles.status} role="status">
        {feedback === 'copyFailed' ? t('account.download.windows.powerShell.copyManually') : null}
      </p>
      <p>
        <ExternalLink href={SITE_LINKS.powerShell}>{t('account.download.windows.powerShell.about')}</ExternalLink>
      </p>
    </section>
  );
}

/** Step 3: "Open isshoni", which waits for the download (06 §7.5), or the Start menu without a link. */
function OpenApp({ url, downloaded }: { url: string; downloaded: boolean }) {
  const { t } = useTranslation();
  const noteId = useId();
  // Links are https only (02 §12.1): a dev server on http://localhost has none.
  if (url === '') return <p className={styles.muted}>{t('account.download.windows.open.fromStartMenu')}</p>;
  if (!downloaded) {
    return (
      <div className={styles.row}>
        <Button variant="primary" icon={<AppWindow />} disabled aria-describedby={noteId}>
          {t('account.download.windows.open.button')}
        </Button>
        <span id={noteId} className={styles.muted}>
          {t('account.download.windows.open.afterInstall')}
        </span>
      </div>
    );
  }
  // The app's protocol handler (06 §2.3): 03's wizard shows the add step for this server, and the room if one came.
  return (
    <div className={styles.row}>
      <a href={url} className={buttonClass({ variant: 'primary' })}>
        <AppWindow aria-hidden="true" className={styles.buttonIcon} />
        <span>{t('account.download.windows.open.button')}</span>
      </a>
    </div>
  );
}

/** The Windows section (06 §7.5): the three numbered steps, the requirements and the PowerShell way. */
function WindowsSection({ links }: { links: DesktopLinks }) {
  const { t } = useTranslation();
  const headingId = useId();
  const [downloaded, setDownloaded] = useState(false);
  return (
    <section className={styles.section} aria-labelledby={headingId}>
      <h2 id={headingId} className={styles.heading}>
        {t('account.download.windows.heading')}
      </h2>
      {!links.release && (
        <Notice
          tone="info"
          action={
            <ExternalLink href={links.releasePageUrl}>{t('account.download.windows.latestRelease')}</ExternalLink>
          }
        >
          {t('account.download.windows.devBuild', { version: links.version })}
        </Notice>
      )}
      {/* The steps stay as they are the whole time: a friend back from the installer sees what comes next. */}
      <ol className={styles.steps}>
        <Step n={1} title={t('account.download.windows.download.title')}>
          <div className={styles.row}>
            <ExternalLink
              href={links.windowsInstallerUrl}
              className={buttonClass({ variant: 'primary' })}
              icon={<Download className={styles.buttonIcon} />}
              onClick={() => {
                setDownloaded(true);
              }}
              // A middle click and "Save link as…" download it too, without a click: step 3 follows them as well.
              onAuxClick={(e) => {
                if (e.button === 1) setDownloaded(true);
              }}
              onContextMenu={() => {
                setDownloaded(true);
              }}
            >
              {t('account.download.windows.download.button')}
            </ExternalLink>
          </div>
          {links.release && (
            <p className={styles.muted}>
              {t('account.download.windows.download.version', { version: links.version })}{' '}
              <ExternalLink href={links.releasePageUrl}>
                {t('account.download.windows.download.releaseNotes')}
              </ExternalLink>
            </p>
          )}
        </Step>
        <Step n={2} title={t('account.download.windows.run.title')}>
          {/* In the order the friend meets them (06 §7.5, §12.1). */}
          <ul className={styles.list}>
            <li>
              {t('account.download.windows.run.browserWarning')}{' '}
              <ExternalLink href={SITE_LINKS.smartScreen}>
                {t('account.download.windows.run.aboutWarning')}
              </ExternalLink>
            </li>
            <li>
              {t('account.download.windows.run.smartScreen')}{' '}
              <ExternalLink href={SITE_LINKS.smartScreen}>
                {t('account.download.windows.run.aboutWarning')}
              </ExternalLink>
            </li>
            <li>
              {t('account.download.windows.run.smartAppControl')}{' '}
              <ExternalLink href={SITE_LINKS.smartAppControl}>
                {t('account.download.windows.run.aboutSmartAppControl')}
              </ExternalLink>
            </li>
          </ul>
        </Step>
        <Step n={3} title={t('account.download.windows.open.title')}>
          <OpenApp url={links.openAppUrl} downloaded={downloaded} />
        </Step>
      </ol>

      <div className={styles.subsection}>
        <h3 className={styles.subheading}>{t('account.download.windows.requirements.heading')}</h3>
        <ul className={styles.list}>
          <li>{t('account.download.windows.requirements.os')}</li>
          <li>
            <ExternalLink href={SITE_LINKS.gpuDriver}>{t('account.download.windows.requirements.gpu')}</ExternalLink>
          </li>
          <li>
            <ExternalLink href={SITE_LINKS.mediaFeaturePack}>
              {t('account.download.windows.requirements.mediaFeaturePack')}
            </ExternalLink>
          </li>
        </ul>
      </div>

      <PowerShellCommand command={links.ps1Command} />
    </section>
  );
}

/** A platform whose app comes in a later milestone (06 §7.5). */
function ComingSection({ os }: { os: Exclude<DesktopOS, 'windows'> }) {
  const { t } = useTranslation();
  const headingId = useId();
  return (
    <section className={styles.section} aria-labelledby={headingId}>
      <h2 id={headingId} className={styles.heading}>
        {os === 'macos' ? t('account.download.macos.heading') : t('account.download.linux.heading')}
      </h2>
      <p className={styles.muted}>
        {os === 'macos' ? t('account.download.macos.coming') : t('account.download.linux.coming')}
      </p>
    </section>
  );
}

/**
 * /download (06 §7.5, §7.6): the desktop app for this server. The installer is the release of the server's own
 * version (a dev, CI or e2e build links to the latest release), the PowerShell one-liner fetches this server's
 * pre-filled install-desktop.ps1, and "Open isshoni" opens the app set up for this server, with the room of
 * ?room=<id> (03 §12.3). Windows comes first when the browser reports Windows; macOS and Linux are coming. Public: it
 * needs only /api/v1/info. In the desktop app RequireBrowser opens it in the system browser instead (03 §7.5, D7).
 */
export function DownloadPage() {
  const { t } = useTranslation();
  const platform = usePlatform();
  const info = useInfo().data;
  const [params] = useSearchParams();
  // server.publicUrl is the server's origin; boot has /info before any page, so the fallback is only a guard.
  const origin = info !== undefined && info.server.publicUrl !== '' ? info.server.publicUrl : platform.serverOrigin;
  const links = desktopLinks(info?.server.version ?? '', origin, roomParam(params));
  return (
    <CenteredPage
      width="lg"
      title={t('account.download.title')}
      lead={t('account.download.lead')}
      footer={<Link to="/">{t('account.back')}</Link>}
    >
      {desktopOrder(platform.client.os).map((os) =>
        os === 'windows' ? <WindowsSection key={os} links={links} /> : <ComingSection key={os} os={os} />,
      )}
      <p className={cx(styles.section, styles.site)}>
        <ExternalLink href={SITE_LINKS.download}>{t('account.download.site')}</ExternalLink>
      </p>
    </CenteredPage>
  );
}
