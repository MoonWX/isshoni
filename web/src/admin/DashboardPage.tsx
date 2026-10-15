import { useQuery } from '@tanstack/react-query';
import type { TFunction } from 'i18next';
import { CircleAlert, CircleCheck, ExternalLink, Info, TriangleAlert } from 'lucide-react';
import { Fragment, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { CommandBlock } from '../app/screens/ScreenFrame';
import { errorMessage } from '../lib/errorText';
import { parseWireTime } from '../lib/time';
import {
  AlertCodeDoctorFail,
  AlertCodeReleaseSecurityUpdate,
  AlertCodeReleaseUpdate,
  AlertSeverityError,
  AlertSeverityWarn,
  RegistrationModeApproval,
  RegistrationModeClosed,
  RegistrationModeInvite,
  TLSModeOff,
  TransportTCP443,
  TransportTCP7882,
  TransportUDP,
  type Alert,
  type AuditEntry,
  type ConnectionLive,
  type DashboardAccounts,
  type OpsDashboard,
  type RoomLive,
  type ShareLive,
} from '../protocol/api.gen';
import { buttonClass } from '../ui/Button';
import { cx } from '../ui/cx';
import { VisuallyHidden } from '../ui/VisuallyHidden';
import { dashboardQueryOptions, settingsQueryOptions } from './adminApi';
import { AdminPage, Empty, Loaded, Section, Tag } from './AdminPage';
import { actionLabel, actorLabel, refName } from './auditText';
import styles from './DashboardPage.module.css';
import {
  alertText,
  certificateFixOf,
  clientKindName,
  failedSources,
  type FailedSources,
  natName,
  osName,
  tlsModeName,
} from './dashboardText';
import { DataTable, Lines, Note, Row } from './DataTable';
import type { FixStep } from './doctorText';
import { Format } from './format';
import { usePageVisible } from './usePageVisible';
import { When } from './When';

/**
 * /admin, the dashboard (05 §15.3, 04 §11.4): what the server is doing now. One snapshot,
 * GET /api/v1/admin/dashboard, asked for every 2 s while the page is visible and never while it is hidden
 * (usePageVisible): alerts, the transfer now and this month, the live rooms with every share's layers and viewers,
 * the server's version, certificate and addresses, the client versions, doctor's last summary, and the accounts.
 * Until the setup wizard is done (settings.setupWizardDone) a checklist card leads to its steps (05 §14.1).
 *
 * A section whose source failed is empty in the snapshot, and an alert dashboard.source_failed says which: the
 * page shows that part as not available, not its zero values. A poll that fails keeps the last snapshot on screen
 * and says it is not current.
 */
export function DashboardPage() {
  const { t } = useTranslation();
  const visible = usePageVisible();
  const dashboard = useQuery(dashboardQueryOptions(visible));
  const settings = useQuery(settingsQueryOptions());
  const setupDone = settings.data?.settings.setupWizardDone ?? true;

  return (
    <AdminPage title={t('admin.dashboard.title')} lead={t('admin.dashboard.lead')}>
      {!setupDone && <SetupChecklist />}
      <Loaded query={dashboard}>
        {(data) => (
          <>
            <Freshness data={data} failed={dashboard.isError ? dashboard.error : null} />
            <Dashboard data={data} />
          </>
        )}
      </Loaded>
    </AdminPage>
  );
}

/**
 * When the snapshot was made, to the second, so the admin sees it tick; after a failed poll, that it is the last one
 * there is. Only that is announced: a time that changes every 2 s would talk over everything else.
 */
function Freshness({ data, failed }: { data: OpsDashboard; failed: unknown }) {
  const { t, i18n } = useTranslation();
  if (failed !== null) {
    return (
      <p className={styles.freshness} role="status">
        {t('admin.dashboard.stale', { reason: errorMessage(failed, t) })}
      </p>
    );
  }
  const at = parseWireTime(data.generatedAt);
  return (
    <p className={styles.freshness}>
      <span className={styles.live} aria-hidden="true" />
      {t('admin.dashboard.updated')}{' '}
      {at && (
        <time dateTime={at.toISOString()}>
          {new Intl.DateTimeFormat(i18n.language, { timeStyle: 'medium' }).format(at)}
        </time>
      )}
    </p>
  );
}

function SetupChecklist() {
  const { t } = useTranslation();
  return (
    <Section title={t('admin.dashboard.checklist.title')} lead={t('admin.dashboard.checklist.lead')}>
      <ol className={styles.checklist}>
        <li className={styles.done}>
          <CircleCheck aria-hidden="true" />
          <span>
            {t('admin.dashboard.checklist.admin')}{' '}
            <VisuallyHidden>{t('admin.dashboard.checklist.doneHint')}</VisuallyHidden>
          </span>
        </li>
        <li>
          <span className={styles.stepNumber} aria-hidden="true">
            {t('admin.dashboard.checklist.two')}
          </span>
          <Link to="/admin/welcome?step=2">{t('admin.dashboard.checklist.conntest')}</Link>
        </li>
        <li>
          <span className={styles.stepNumber} aria-hidden="true">
            {t('admin.dashboard.checklist.three')}
          </span>
          <Link to="/admin/welcome?step=3">{t('admin.dashboard.checklist.invite')}</Link>
        </li>
      </ol>
      <div>
        <Link to="/admin/welcome?step=2" className={buttonClass({ variant: 'primary' })}>
          {t('admin.dashboard.checklist.continue')}
        </Link>
      </div>
    </Section>
  );
}

function Dashboard({ data }: { data: OpsDashboard }) {
  const failed = failedSources(data);
  return (
    <>
      {data.alerts.length > 0 && <Alerts data={data} />}
      <Overview data={data} failed={failed} />
      <LiveRooms rooms={data.rooms} failed={failed} />
      <div className={styles.columns}>
        <ServerCard data={data} failed={failed} />
        <div className={styles.stack}>
          <DoctorCard data={data} failed={failed} />
          <ClientsCard data={data} failed={failed} />
        </div>
      </div>
      {data.accounts && <AccountsCard accounts={data.accounts} />}
    </>
  );
}

/** What a part whose source failed shows instead of its zero values. */
function Unavailable() {
  const { t } = useTranslation();
  return <span className={styles.unavailable}>{t('admin.dashboard.unavailable')}</span>;
}

/** A fix from doctor's catalog: each step's sentence, and its command to copy when it has one. */
function FixSteps({ steps, className }: { steps: readonly FixStep[]; className?: string }) {
  return steps.map((s, i) => (
    // Steps are positional: the same code gives the same steps in the same order.
    <Fragment key={i}>
      <p className={className}>{s.text}</p>
      {s.command !== undefined && <CommandBlock>{s.command}</CommandBlock>}
    </Fragment>
  ));
}

// ---- alerts ----

function severityIcon(alert: Alert): ReactNode {
  if (alert.severity === AlertSeverityError) return <CircleAlert aria-hidden="true" />;
  if (alert.severity === AlertSeverityWarn) return <TriangleAlert aria-hidden="true" />;
  return <Info aria-hidden="true" />;
}

function severityName(alert: Alert, t: TFunction): string {
  if (alert.severity === AlertSeverityError) return t('admin.dashboard.severity.error');
  if (alert.severity === AlertSeverityWarn) return t('admin.dashboard.severity.warn');
  return t('admin.dashboard.severity.info');
}

function Alerts({ data }: { data: OpsDashboard }) {
  const { t, i18n } = useTranslation();
  const update = data.server.update;
  return (
    <section className={styles.alerts} aria-label={t('admin.dashboard.alerts')}>
      <ul className={styles.alertList}>
        {data.alerts.map((alert, i) => {
          const { text, fix } = alertText(alert, data, t, i18n.language);
          const release = alert.code === AlertCodeReleaseUpdate || alert.code === AlertCodeReleaseSecurityUpdate;
          return (
            // Alerts have no identity of their own: the list is the snapshot's, in its order.
            <li key={i} className={cx(styles.alert, styles[alert.severity])}>
              {severityIcon(alert)}
              <div className={styles.alertBody}>
                <p>
                  <VisuallyHidden>{severityName(alert, t)}: </VisuallyHidden>
                  {text}
                </p>
                <FixSteps steps={fix} className={styles.alertFix} />
                {alert.code === AlertCodeDoctorFail && (
                  <Link to="/admin/doctor">{t('admin.dashboard.openDoctor')}</Link>
                )}
                {release && update && /^https:\/\//.test(update.url) && (
                  <a href={update.url} target="_blank" rel="noreferrer" className={styles.external}>
                    {t('admin.dashboard.releaseNotes')}
                    <ExternalLink aria-hidden="true" />
                  </a>
                )}
              </div>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

// ---- overview ----

function Stat({ label, value, detail }: { label: string; value: ReactNode; detail?: ReactNode }) {
  return (
    <div className={styles.stat}>
      <dt className={styles.statLabel}>{label}</dt>
      <dd className={styles.statValue}>{value}</dd>
      {detail !== undefined && <dd className={styles.statDetail}>{detail}</dd>}
    </div>
  );
}

function Overview({ data, failed }: { data: OpsDashboard; failed: FailedSources }) {
  const { t, i18n } = useTranslation();
  const f = new Format(i18n.language);
  const people = new Set(data.rooms.flatMap((r) => r.participants.map((p) => p.userId))).size;
  const connections = data.rooms.reduce((n, r) => n + r.participants.reduce((m, p) => m + p.connections.length, 0), 0);
  const shares = data.rooms.reduce((n, r) => n + r.shares.length, 0);
  const { transfer, media } = data;
  const pcs = media.peerConnections;
  const roomsOK = !failed.rooms;
  const transferOK = !failed.transfer;
  return (
    <section aria-label={t('admin.dashboard.overview')}>
      <dl className={styles.stats}>
        <Stat
          label={t('admin.dashboard.stat.sending')}
          value={transferOK ? f.bitrate(transfer.egressBps) : <Unavailable />}
          detail={
            transferOK ? t('admin.dashboard.stat.receiving', { rate: f.bitrate(transfer.ingressBps) }) : undefined
          }
        />
        <Stat
          label={t('admin.dashboard.stat.people')}
          value={roomsOK ? f.number(people) : <Unavailable />}
          detail={roomsOK ? t('admin.dashboard.stat.connections', { count: connections }) : undefined}
        />
        <Stat
          label={t('admin.dashboard.stat.shares')}
          value={roomsOK ? f.number(shares) : <Unavailable />}
          detail={
            failed.media
              ? undefined
              : t('admin.dashboard.stat.media', {
                  udp: pcs.udp,
                  tcp443: pcs.tcp443,
                  tcp7882: pcs.tcp7882,
                })
          }
        />
        {transferOK ? (
          <Stat
            label={t('admin.dashboard.stat.month', { month: f.month(transfer.month) })}
            value={f.size(transfer.egressBytes)}
            detail={
              <Lines>
                {transfer.projectedEgressBytes !== undefined && (
                  <span>{t('admin.dashboard.stat.projected', { size: f.size(transfer.projectedEgressBytes) })}</span>
                )}
                <span>
                  {transfer.alertGb > 0
                    ? t('admin.dashboard.stat.alertAt', { size: f.gigabytes(transfer.alertGb) })
                    : t('admin.dashboard.stat.noAlert')}
                </span>
              </Lines>
            }
          />
        ) : (
          <Stat label={t('admin.dashboard.stat.monthUnknown')} value={<Unavailable />} />
        )}
      </dl>
    </section>
  );
}

// ---- rooms ----

function transportName(transport: string | undefined, t: TFunction): string {
  switch (transport) {
    case TransportUDP:
      return t('admin.dashboard.transport.udp');
    case TransportTCP443:
      return t('admin.dashboard.transport.tcp443');
    case TransportTCP7882:
      return t('admin.dashboard.transport.tcp7882');
    case undefined:
    case '':
      return t('admin.dashboard.transport.none');
    default:
      return transport;
  }
}

/** "Web 0.3.0 on iOS": what a connection is. */
function connectionText(c: ConnectionLive, t: TFunction): string {
  return t('admin.dashboard.connection', { kind: clientKindName(c.kind, t), version: c.version, os: osName(c.os, t) });
}

/** "over UDP, 38 ms", or "no media yet" for a connection without a selected pair (and so without an RTT). */
function connectionPath(c: ConnectionLive, t: TFunction): string {
  if (c.transport === undefined || (c.transport as string) === '') return t('admin.dashboard.transport.none');
  return t('admin.dashboard.rooms.via', { transport: transportName(c.transport, t), rtt: c.rttMs });
}

function shareTitle(share: ShareLive, owner: string, t: TFunction): string {
  switch (share.kind) {
    case 'screen':
      return t('admin.dashboard.share.screen', { name: owner });
    case 'window':
      return t('admin.dashboard.share.window', { name: owner });
    case 'tab':
      return t('admin.dashboard.share.tab', { name: owner });
    default:
      return t('admin.dashboard.share.other', { name: owner });
  }
}

function LiveRooms({ rooms, failed }: { rooms: readonly RoomLive[]; failed: FailedSources }) {
  const { t } = useTranslation();
  const live = rooms.filter((r) => r.participants.length > 0 || r.shares.length > 0);
  return (
    <Section title={t('admin.dashboard.rooms.title')}>
      {failed.rooms ? (
        <p>
          <Unavailable />
        </p>
      ) : live.length === 0 ? (
        <Empty>{t('admin.dashboard.rooms.empty')}</Empty>
      ) : (
        <div className={styles.rooms}>
          {live.map((room) => (
            <LiveRoom key={room.id} room={room} />
          ))}
        </div>
      )}
    </Section>
  );
}

function LiveRoom({ room }: { room: RoomLive }) {
  const { t } = useTranslation();
  const names = new Map(room.participants.map((p) => [p.userId, p.username]));
  return (
    <article className={styles.room} aria-label={room.name}>
      <h3 className={styles.roomName}>
        <Link to={`/r/${encodeURIComponent(room.id)}`}>{room.name}</Link>
        <span className={styles.roomCount}>
          {t('admin.dashboard.rooms.people', { count: room.participants.length })}
        </span>
      </h3>
      {room.shares.map((share) => (
        <ShareCard key={share.shareId} share={share} owner={names.get(share.ownerUserId) ?? share.ownerUserId} />
      ))}
      {room.participants.length > 0 && (
        <DataTable
          caption={t('admin.dashboard.rooms.peopleIn', { room: room.name })}
          columns={[
            t('admin.dashboard.rooms.col.person'),
            t('admin.dashboard.rooms.col.connection'),
            t('admin.dashboard.rooms.col.watching'),
          ]}
        >
          {room.participants.map((p) => (
            <Row
              key={p.userId}
              cells={[
                p.username,
                <Lines>
                  {p.connections.map((c) => (
                    <span key={c.id}>
                      {connectionText(c, t)}
                      <Note> {connectionPath(c, t)}</Note>
                    </span>
                  ))}
                </Lines>,
                p.watching.length === 0
                  ? t('admin.dashboard.rooms.watchingNothing')
                  : t('admin.dashboard.rooms.watchingShares', { count: p.watching.length }),
              ]}
            />
          ))}
        </DataTable>
      )}
    </article>
  );
}

function ShareCard({ share, owner }: { share: ShareLive; owner: string }) {
  const { t, i18n } = useTranslation();
  const f = new Format(i18n.language);
  const title = shareTitle(share, owner, t);
  return (
    <div className={styles.share}>
      <p className={styles.shareHead}>
        <span className={styles.shareTitle}>{title}</span>
        <Note>
          {t('admin.dashboard.share.meta', { codec: share.codec === '' ? t('admin.unknown') : share.codec })}{' '}
          <When at={share.startedAt} mode="relative" />
        </Note>
      </p>
      <DataTable
        caption={t('admin.dashboard.share.layersOf', { share: title })}
        columns={[
          t('admin.dashboard.share.col.layer'),
          t('admin.dashboard.share.col.size'),
          t('admin.dashboard.share.col.fps'),
          t('admin.dashboard.share.col.bitrate'),
          t('admin.dashboard.share.col.loss'),
        ]}
      >
        {share.layers.map((layer) => (
          <Row
            key={layer.rid}
            cells={[
              layer.rid === 'f'
                ? t('admin.dashboard.share.layer.high')
                : layer.rid === 'q'
                  ? t('admin.dashboard.share.layer.low')
                  : layer.rid,
              t('admin.dashboard.share.size', { width: layer.width, height: layer.height }),
              f.number(layer.fps, 1),
              f.bitrate(layer.bitrate),
              f.percent(layer.lossPct, 1),
            ]}
          />
        ))}
      </DataTable>
      <p className={styles.shareFoot}>
        {t('admin.dashboard.share.viewers', {
          high: share.viewers.high,
          low: share.viewers.low,
          audio: share.viewers.audio,
        })}{' '}
        <Note>{t('admin.dashboard.share.egress', { rate: f.bitrate(share.egressBps) })}</Note>
      </p>
    </div>
  );
}

// ---- server ----

function Fact({ term, children }: { term: string; children: ReactNode }) {
  return (
    <div className={styles.fact}>
      <dt>{term}</dt>
      <dd>{children}</dd>
    </div>
  );
}

function ServerCard({ data, failed }: { data: OpsDashboard; failed: FailedSources }) {
  const { t, i18n } = useTranslation();
  const f = new Format(i18n.language);
  const { server } = data;
  // Without the server's status the snapshot has only the version (its own) and the parts of other sources: the
  // rest would be zero values (a start in year 1, a certificate of no mode, no address).
  const statusOK = !failed.server;
  return (
    <Section title={t('admin.dashboard.server.title')}>
      <dl className={styles.facts}>
        <Fact term={t('admin.dashboard.server.version')}>{server.version}</Fact>
        {statusOK && <ServerStatusFacts data={data} />}
        {!failed.process && (
          <Fact term={t('admin.dashboard.server.process')}>
            <Lines>
              {/* A platform without the process's numbers (macOS) reports 0. */}
              {server.process.rssBytes > 0 && (
                <span>{t('admin.dashboard.server.memory', { size: f.size(server.process.rssBytes) })}</span>
              )}
              <Note>
                {t('admin.dashboard.server.cpu', {
                  seconds: f.number(server.process.cpuSeconds, 1),
                  cpus: server.process.numCpu,
                  goroutines: server.process.goroutines,
                })}
              </Note>
            </Lines>
          </Fact>
        )}
        {server.update && (
          <Fact term={t('admin.dashboard.server.update')}>
            <Lines>
              <span>
                {t('admin.dashboard.server.latest', { version: server.update.latest })}{' '}
                {server.update.security && <Tag tone="danger">{t('admin.dashboard.server.security')}</Tag>}
              </span>
              <Note>
                {t('admin.dashboard.server.checked')} <When at={server.update.checkedAt} mode="relative" />
              </Note>
            </Lines>
          </Fact>
        )}
      </dl>
      {!statusOK && (
        <p>
          <Unavailable />
        </p>
      )}
    </Section>
  );
}

/** The facts of the server's status: its start, address, certificate, public addresses and media addresses. */
function ServerStatusFacts({ data }: { data: OpsDashboard }) {
  const { t } = useTranslation();
  const { server } = data;
  return (
    <>
      <Fact term={t('admin.dashboard.server.started')}>
        <When at={server.startedAt} mode="relative" fallback={t('admin.unknown')} />
      </Fact>
      <Fact term={t('admin.dashboard.server.origin')}>{server.origin === '' ? t('admin.unknown') : server.origin}</Fact>
      <Fact term={t('admin.dashboard.server.tls')}>
        <Certificate data={data} />
      </Fact>
      <Fact term={t('admin.dashboard.server.publicIp')}>
        <Lines>
          <span>{server.publicIpv4 === '' ? t('admin.dashboard.server.noIpv4') : server.publicIpv4}</span>
          {server.publicIpv6 !== '' && <span>{server.publicIpv6}</span>}
          <Note>{t('admin.dashboard.server.nat', { nat: natName(server, t) })}</Note>
        </Lines>
      </Fact>
      <Fact term={t('admin.dashboard.server.media')}>
        {server.advertised.length === 0 ? (
          t('admin.dashboard.server.noMedia')
        ) : (
          <Lines>
            {server.advertised.map((a) => (
              <span key={`${a.proto} ${a.addr} ${a.via}`}>
                {a.addr} <Note>{transportName(a.via, t)}</Note>
              </span>
            ))}
          </Lines>
        )}
      </Fact>
    </>
  );
}

/**
 * The certificate: its kind and names, whether it is served, its dates, and its last error with doctor's fix for
 * it (04 §8.7, §18). A manual certificate that can't be read, or a first order that failed, raises no alert: this
 * is where the admin reads what went wrong.
 */
function Certificate({ data }: { data: OpsDashboard }) {
  const { t, i18n } = useTranslation();
  const f = new Format(i18n.language);
  const { tls } = data.server;
  if (tls.mode === TLSModeOff) return t('admin.dashboard.server.tlsOff');
  const kind = tlsModeName(tls.mode, t);
  const code = tls.lastErrorCode ?? '';
  return (
    <div className={styles.certificate}>
      <span>
        {tls.names.length > 0 ? t('admin.dashboard.server.tlsFor', { mode: kind, names: f.list(tls.names) }) : kind}{' '}
        {tls.ready ? (
          <Tag tone="success">{t('admin.dashboard.server.tlsReady')}</Tag>
        ) : (
          <Tag tone="warning">{t('admin.dashboard.server.tlsNotReady')}</Tag>
        )}
      </span>
      {tls.notAfter !== undefined && (
        <Note>
          {tls.nextRenewal !== undefined
            ? t('admin.dashboard.server.tlsValidRenews', {
                notAfter: f.date(tls.notAfter),
                nextRenewal: f.date(tls.nextRenewal),
              })
            : t('admin.dashboard.server.tlsValid', { notAfter: f.date(tls.notAfter) })}
        </Note>
      )}
      {code !== '' && <CertificateError data={data} code={code} />}
    </div>
  );
}

function CertificateError({ data, code }: { data: OpsDashboard; code: string }) {
  const { t, i18n } = useTranslation();
  const steps = certificateFixOf(code, data, t, i18n.language);
  return (
    <div className={styles.certificateError}>
      <Note>
        {t('admin.dashboard.server.tlsLastError')} <When at={data.server.tls.lastErrorAt} mode="relative" />
      </Note>
      {/* A code this build can't read (no certificate code at all) is still precise. */}
      {steps.length > 0 ? <FixSteps steps={steps} /> : <p>{code}</p>}
    </div>
  );
}

function DoctorCard({ data, failed }: { data: OpsDashboard; failed: FailedSources }) {
  const { t } = useTranslation();
  const d = data.doctor;
  return (
    <Section title={t('admin.dashboard.doctor.title')}>
      {failed.doctor ? (
        <p>
          <Unavailable />
        </p>
      ) : d ? (
        <>
          <p>
            {t('admin.dashboard.doctor.ran')} <When at={d.ranAt} mode="relative" />
          </p>
          <p className={styles.counts}>
            <Tag tone="success">{t('admin.dashboard.doctor.ok', { count: d.ok })}</Tag>
            <Tag tone={d.warn > 0 ? 'warning' : 'neutral'}>{t('admin.dashboard.doctor.warn', { count: d.warn })}</Tag>
            <Tag tone={d.fail > 0 ? 'danger' : 'neutral'}>{t('admin.dashboard.doctor.fail', { count: d.fail })}</Tag>
          </p>
        </>
      ) : (
        <p className={styles.muted}>{t('admin.dashboard.doctor.none')}</p>
      )}
      <div>
        <Link to="/admin/doctor" className={buttonClass({ size: 'sm' })}>
          {t('admin.dashboard.openDoctor')}
        </Link>
      </div>
    </Section>
  );
}

function ClientsCard({ data, failed }: { data: OpsDashboard; failed: FailedSources }) {
  const { t } = useTranslation();
  return (
    <Section title={t('admin.dashboard.clients.title')}>
      {/* The clients are counted from the rooms. */}
      {failed.rooms ? (
        <p>
          <Unavailable />
        </p>
      ) : data.clients.length === 0 ? (
        <p className={styles.muted}>{t('admin.dashboard.clients.empty')}</p>
      ) : (
        <DataTable
          caption={t('admin.dashboard.clients.title')}
          columns={[
            t('admin.dashboard.clients.col.app'),
            t('admin.dashboard.clients.col.version'),
            t('admin.dashboard.clients.col.count'),
          ]}
        >
          {data.clients.map((c) => (
            <Row
              key={`${c.kind} ${c.version}`}
              cells={[
                clientKindName(c.kind, t),
                <>
                  {c.version} {c.outdated && <Tag tone="warning">{t('admin.dashboard.clients.outdated')}</Tag>}
                </>,
                c.count,
              ]}
            />
          ))}
        </DataTable>
      )}
    </Section>
  );
}

// ---- accounts ----

function registrationName(mode: string, t: TFunction): string {
  switch (mode) {
    case RegistrationModeInvite:
      return t('admin.dashboard.accounts.mode.invite');
    case RegistrationModeApproval:
      return t('admin.dashboard.accounts.mode.approval');
    case RegistrationModeClosed:
      return t('admin.dashboard.accounts.mode.closed');
    default:
      return mode;
  }
}

function AccountsCard({ accounts }: { accounts: DashboardAccounts }) {
  const { t } = useTranslation();
  const { users } = accounts;
  return (
    <Section
      title={t('admin.dashboard.accounts.title')}
      lead={t('admin.dashboard.accounts.registration', { mode: registrationName(accounts.registrationMode, t) })}
    >
      <dl className={styles.stats}>
        <Stat
          label={t('admin.dashboard.accounts.users')}
          value={users.active}
          detail={t('admin.dashboard.accounts.admins', { count: users.admins })}
        />
        <Stat
          label={t('admin.dashboard.accounts.pending')}
          value={users.pending}
          detail={
            users.pending > 0 ? <Link to="/admin/approvals">{t('admin.dashboard.accounts.answer')}</Link> : undefined
          }
        />
        <Stat label={t('admin.dashboard.accounts.disabled')} value={users.disabled} />
        <Stat
          label={t('admin.dashboard.accounts.invites')}
          value={accounts.invites.active}
          detail={t('admin.dashboard.accounts.sessions', { count: accounts.sessions.active })}
        />
      </dl>
      <SecurityEvents events={accounts.securityEvents} />
      <div>
        <Link to="/admin/audit">{t('admin.dashboard.accounts.audit')}</Link>
      </div>
    </Section>
  );
}

function SecurityEvents({ events }: { events: readonly AuditEntry[] }) {
  const { t } = useTranslation();
  if (events.length === 0) return <p className={styles.muted}>{t('admin.dashboard.accounts.noEvents')}</p>;
  return (
    <DataTable
      caption={t('admin.dashboard.accounts.events')}
      columns={[
        t('admin.dashboard.accounts.col.what'),
        t('admin.dashboard.accounts.col.who'),
        t('admin.dashboard.accounts.col.when'),
      ]}
    >
      {events.map((e) => (
        <Row
          key={e.id}
          cells={[
            <Lines>
              <span>{actionLabel(e.action, t)}</span>
              {e.target && <Note>{refName(e.target, t)}</Note>}
            </Lines>,
            actorLabel(e.actor, t),
            <When at={e.at} mode="relative" />,
          ]}
        />
      ))}
    </DataTable>
  );
}
