// Small parts of the wizard's steps and the relink page (docs/m2/03 §8.2, §19).
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import styles from './wizard.module.css';

/**
 * The trust line of the confirm and link steps (03 §19): the login page's trust model in one line, with [More]
 * (/about).
 */
export function TrustLine() {
  const { t } = useTranslation();
  return (
    <p className={styles.trust}>
      {t('desktop.setup.trust')} <Link to="/about">{t('desktop.setup.trustMore')}</Link>
    </p>
  );
}

/** "hostUnicode (host)" when the two differ (against look-alike names, 03 §8.2), else the host. */
export function hostLabel(host: string, hostUnicode: string): string {
  return hostUnicode !== '' && hostUnicode !== host ? `${hostUnicode} (${host})` : host;
}

/** The current time, updated every `everyMs` while mounted: the link code's countdown. */
export function useNow(everyMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => {
      setNow(Date.now());
    }, everyMs);
    return () => {
      clearInterval(id);
    };
  }, [everyMs]);
  return now;
}

/** m:ss of the time left until `until` (ms since the epoch), 0:00 once it has passed. */
export function formatLeft(until: number, now: number): string {
  const s = Math.max(0, Math.ceil((until - now) / 1000));
  return `${String(Math.floor(s / 60))}:${String(s % 60).padStart(2, '0')}`;
}
