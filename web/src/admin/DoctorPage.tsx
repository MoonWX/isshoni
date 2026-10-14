import { useMutation, useQuery } from '@tanstack/react-query';
import type { TFunction } from 'i18next';
import { CircleAlert, CircleCheck, CircleMinus, ExternalLink, Info, TriangleAlert } from 'lucide-react';
import { useEffect, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { CommandBlock } from '../app/screens/ScreenFrame';
import { Notice } from '../auth/Notice';
import { ConnTestPanel } from '../conntest';
import { errorMessage } from '../lib/errorText';
import {
  CodeDoctorBusy,
  DoctorStatusFail,
  DoctorStatusInfo,
  DoctorStatusOK,
  DoctorStatusSkip,
  DoctorStatusWarn,
  type DoctorCheck,
  type DoctorReport,
  type DoctorStatus,
} from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { isApiError } from '../protocol/rest';
import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { Spinner } from '../ui/Spinner';
import { adminApi, doctorQueryOptions } from './adminApi';
import { AdminPage, Loaded, Section, Tag, type TagTone } from './AdminPage';
import { BandwidthCalculator } from './BandwidthCalculator';
import { checkName } from './dashboardText';
import styles from './DoctorPage.module.css';
import { checkText, type CheckLink } from './doctorText';
import { When } from './When';

/**
 * /admin/doctor (05 §15.3, 04 §13): doctor's last report with "Run doctor", the connection test (05 §14.2) and the
 * bandwidth calculator (04 §13.4).
 *
 * The report is GET /api/v1/admin/doctor, which answers 404 until the server has its first one (the first 20 s of a
 * new installation): the page then says that there is none yet. "Run doctor" is POST /api/v1/admin/doctor, which
 * runs every check in the server (up to two minutes) and keeps the report. While a run is under way, and for 10 s
 * after one, the server answers 429 doctor_busy with the seconds to wait: the page keeps the report it has and
 * offers the button again after that time.
 *
 * Each check reads from the catalog, doctor.<code> and doctor.fix.<fixCode> with its params (doctorText.ts); a
 * check that only looks at this machine (ports) is labeled so, and a warning or failure links to its section of the
 * project site's troubleshooting page.
 */
export function DoctorPage() {
  const { t } = useTranslation();
  const report = useQuery(doctorQueryOptions());
  return (
    <AdminPage title={t('admin.doctor.title')} lead={t('admin.doctor.lead')}>
      <Loaded query={report}>{(data) => <Report report={data} />}</Loaded>
      <Section title={t('admin.doctor.conntest.title')} lead={t('admin.doctor.conntest.lead')} region={false}>
        <ConnTestPanel />
      </Section>
      <BandwidthCalculator />
    </AdminPage>
  );
}

/** The time until which the server said doctor is busy, in ms since the epoch. */
function useWait(): [number | null, (seconds: number) => void] {
  const [until, setUntil] = useState<number | null>(null);
  useEffect(() => {
    if (until === null) return undefined;
    const id = setTimeout(
      () => {
        setUntil(null);
      },
      Math.max(0, until - Date.now()),
    );
    return () => {
      clearTimeout(id);
    };
  }, [until]);
  return [
    until,
    (seconds) => {
      setUntil(Date.now() + seconds * 1000);
    },
  ];
}

function Report({ report }: { report: DoctorReport | null }) {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  const [waitUntil, wait] = useWait();
  const [waitSeconds, setWaitSeconds] = useState(0);

  const run = useMutation({
    mutationFn: () => adminApi.runDoctor(),
    // Sent at once (a browser that says it is offline gets the failure), and never retried: a run is not free.
    networkMode: 'always',
    onSuccess: (fresh) => {
      queryClient.setQueryData(queryKeys.adminDoctor, fresh);
    },
    onError: (err) => {
      if (isApiError(err, CodeDoctorBusy)) {
        const seconds = Math.max(1, err.retryAfterSec ?? 10);
        setWaitSeconds(seconds);
        wait(seconds);
      }
    },
  });
  const busy = run.isError && isApiError(run.error, CodeDoctorBusy);

  return (
    <Section title={t('admin.doctor.report.title')}>
      <div className={styles.runRow}>
        <Button
          variant="primary"
          loading={run.isPending}
          disabled={waitUntil !== null}
          onClick={() => {
            run.mutate();
          }}
        >
          {report ? t('admin.doctor.runAgain') : t('admin.doctor.run')}
        </Button>
        {run.isPending && (
          <p className={styles.running} role="status">
            <Spinner size="sm" label={null} />
            <span>{t('admin.doctor.running')}</span>
          </p>
        )}
      </div>
      {busy && waitUntil !== null && <Notice tone="info">{t('admin.doctor.busy', { count: waitSeconds })}</Notice>}
      {run.isError && !busy && <Notice>{t('admin.doctor.runFailed', { reason: errorMessage(run.error, t) })}</Notice>}
      {report === null ? <p className={styles.none}>{t('admin.doctor.noReport')}</p> : <Checks report={report} />}
    </Section>
  );
}

const STATUS_ORDER: readonly DoctorStatus[] = [
  DoctorStatusFail,
  DoctorStatusWarn,
  DoctorStatusOK,
  DoctorStatusInfo,
  DoctorStatusSkip,
];

function statusLabel(status: DoctorStatus, t: TFunction): string {
  switch (status) {
    case DoctorStatusOK:
      return t('admin.doctor.status.ok');
    case DoctorStatusWarn:
      return t('admin.doctor.status.warn');
    case DoctorStatusFail:
      return t('admin.doctor.status.fail');
    case DoctorStatusSkip:
      return t('admin.doctor.status.skip');
    case DoctorStatusInfo:
      return t('admin.doctor.status.info');
  }
}

function statusTone(status: DoctorStatus): TagTone {
  switch (status) {
    case DoctorStatusOK:
      return 'success';
    case DoctorStatusWarn:
      return 'warning';
    case DoctorStatusFail:
      return 'danger';
    case DoctorStatusInfo:
      return 'accent';
    case DoctorStatusSkip:
      return 'neutral';
  }
}

function StatusIcon({ status }: { status: DoctorStatus }) {
  switch (status) {
    case DoctorStatusOK:
      return <CircleCheck aria-hidden="true" />;
    case DoctorStatusWarn:
      return <TriangleAlert aria-hidden="true" />;
    case DoctorStatusFail:
      return <CircleAlert aria-hidden="true" />;
    case DoctorStatusInfo:
      return <Info aria-hidden="true" />;
    case DoctorStatusSkip:
      return <CircleMinus aria-hidden="true" />;
  }
}

function Checks({ report }: { report: DoctorReport }) {
  const { t } = useTranslation();
  const counts = report.summary;
  return (
    <>
      <p className={styles.ran}>
        {t('admin.doctor.ran')} <When at={report.ranAt} />{' '}
        <span className={styles.version}>{t('admin.doctor.version', { version: report.version })}</span>
      </p>
      <p className={styles.summary}>
        {STATUS_ORDER.map((status) => (
          <Tag key={status} tone={counts[status] > 0 ? statusTone(status) : 'neutral'}>
            {t(`admin.doctor.count.${status}`, { count: counts[status] })}
          </Tag>
        ))}
      </p>
      <ul className={styles.checks} aria-label={t('admin.doctor.checks')}>
        {report.checks.map((check) => (
          <CheckItem key={check.id} check={check} report={report} />
        ))}
      </ul>
    </>
  );
}

function linkLabel(link: CheckLink, t: TFunction): string {
  switch (link.kind) {
    case 'more':
      return t('admin.doctor.more');
    case 'vps':
      return t('admin.doctor.vpsGuide');
    case 'release':
      return t('admin.doctor.releaseNotes');
  }
}

function CheckItem({ check, report }: { check: DoctorCheck; report: DoctorReport }) {
  const { t, i18n } = useTranslation();
  const { message, fix, links } = checkText(check, report.env, t, i18n.language);
  const name = checkName(check.id, t);
  return (
    <li className={cx(styles.check, styles[check.status])} aria-label={name}>
      <span className={styles.icon}>
        <StatusIcon status={check.status} />
      </span>
      <div className={styles.body}>
        <p className={styles.head}>
          <span className={styles.name}>{name}</span>
          <Tag tone={statusTone(check.status)}>{statusLabel(check.status, t)}</Tag>
          {check.localOnly && <Tag>{t('admin.doctor.localOnly')}</Tag>}
        </p>
        <Message text={message} />
        {check.localOnly && <p className={styles.note}>{t('admin.doctor.localOnlyNote')}</p>}
        {fix.length > 0 && (
          <div className={styles.fix}>
            <p className={styles.fixLabel}>{t('admin.doctor.fix')}</p>
            {fix.map((s, i) => (
              // Steps are positional: the same check gives the same steps in the same order.
              <div key={i} className={styles.step}>
                <Message text={s.text} />
                {s.command !== undefined && <CommandBlock>{s.command}</CommandBlock>}
              </div>
            ))}
          </div>
        )}
        {links.length > 0 && (
          <p className={styles.links}>
            {links.map((link) => (
              <a key={link.url} href={link.url} target="_blank" rel="noreferrer">
                {linkLabel(link, t)}
                <ExternalLink aria-hidden="true" />
              </a>
            ))}
          </p>
        )}
      </div>
    </li>
  );
}

/** A text that may have lines of its own (the server's English for a code this build doesn't know). */
function Message({ text }: { text: string }): ReactNode {
  const lines = text.split('\n');
  return (
    <p className={styles.message}>
      {lines.map((line, i) => (
        <span key={i}>{line}</span>
      ))}
    </p>
  );
}
