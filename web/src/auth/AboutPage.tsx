import { ExternalLink as ExternalLinkIcon } from 'lucide-react';
import { useId, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { usePlatform } from '../app/context';
import { useInfo } from '../app/info';
import { CenteredPage } from '../app/layouts/CenteredPage';
import styles from './AboutPage.module.css';

/** The project site (06 D11) and the pages of it that the SPA links to (06 §10.2). */
export const PROJECT_SITE_URL = 'https://moonwx.github.io/isshoni/';
export const PRIVACY_URL = `${PROJECT_SITE_URL}privacy`;
export const CODE_SIGNING_URL = `${PROJECT_SITE_URL}code-signing`;
/** Third-party notices of this build, served next to the SPA (05 §17.1, 06 §8.3). A file, so not a route. */
export const LICENSES_PATH = '/licenses.txt';

/**
 * A link that leaves the SPA: a new tab, without opener or referrer (05 §20). Outside a browser tab (the desktop
 * app, M2) the platform opens it in the system browser.
 */
function ExternalLink({ href, children }: { href: string; children: ReactNode }) {
  const platform = usePlatform();
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className={styles.external}
      onClick={(e) => {
        if (platform.kind === 'browser') return;
        e.preventDefault();
        platform.openExternal(href);
      }}
    >
      <span>{children}</span>
      <ExternalLinkIcon aria-hidden="true" />
    </a>
  );
}

function Section({ heading, children }: { heading: string; children: ReactNode }) {
  const id = useId();
  return (
    <section className={styles.section} aria-labelledby={id}>
      <h2 id={id} className={styles.heading}>
        {heading}
      </h2>
      {children}
    </section>
  );
}

/**
 * /about (05 §5): the versions of this page and the server, the trust model (plan "Privacy and trust model"; the
 * login page shows its three-line form and links here), and links to the project site's privacy and code-signing
 * pages and to /licenses.txt. Public: it needs nothing but /info.
 */
export function AboutPage() {
  const { t } = useTranslation();
  const platform = usePlatform();
  const info = useInfo().data;
  return (
    <CenteredPage
      width="lg"
      title={t('auth.about.title')}
      lead={t('auth.about.lead')}
      footer={<Link to="/">{t('auth.about.back')}</Link>}
    >
      <Section heading={t('auth.about.trust.heading')}>
        <ul className={styles.list}>
          <li>{t('auth.about.trust.server')}</li>
          <li>{t('auth.about.trust.noRecording')}</li>
          <li>{t('auth.about.trust.stored')}</li>
          <li>{t('auth.about.trust.local')}</li>
          <li>{t('auth.about.trust.noTelemetry')}</li>
        </ul>
      </Section>

      <Section heading={t('auth.about.versions.heading')}>
        <dl className={styles.facts}>
          {info !== undefined && (
            <>
              <div>
                <dt>{t('auth.about.versions.serverName')}</dt>
                <dd>{info.server.name}</dd>
              </div>
              <div>
                <dt>{t('auth.about.versions.server')}</dt>
                <dd className={styles.version}>{info.server.version}</dd>
              </div>
            </>
          )}
          <div>
            <dt>{t('auth.about.versions.page')}</dt>
            <dd className={styles.version}>{__ISSHONI_VERSION__}</dd>
          </div>
        </dl>
      </Section>

      <Section heading={t('auth.about.links.heading')}>
        <ul className={styles.links}>
          <li>
            <ExternalLink href={PRIVACY_URL}>{t('auth.about.links.privacy')}</ExternalLink>
          </li>
          <li>
            <ExternalLink href={CODE_SIGNING_URL}>{t('auth.about.links.codeSigning')}</ExternalLink>
          </li>
          <li>
            <ExternalLink href={new URL(LICENSES_PATH, platform.serverOrigin).href}>
              {t('auth.about.links.licenses')}
            </ExternalLink>
          </li>
          <li>
            <ExternalLink href={PROJECT_SITE_URL}>{t('auth.about.links.site')}</ExternalLink>
          </li>
        </ul>
      </Section>
    </CenteredPage>
  );
}
