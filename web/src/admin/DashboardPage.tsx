import { useQuery } from '@tanstack/react-query';
import type { TFunction } from 'i18next';
import { CircleAlert, CircleCheck, ExternalLink, Info, TriangleAlert } from 'lucide-react';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

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
import { alertText } from './dashboardText';
import { DataTable, Lines, Note, Row } from './DataTable';
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
 * A section whose source failed is empty in the snapshot, and an alert dashboard.source_failed says which; the
 * page shows what it has. A poll that fails keeps the last snapshot on screen and says it is not current.
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
  return (
    <>
      {data.alerts.length > 0 && <Alerts data={data} />}
      <Overview data={data} />
      <LiveRooms rooms={data.rooms} />
      <div className={styles.columns}>
        <ServerCard data={data} />
        <div className={styles.stack}>
          <DoctorCard data={data} />
          <ClientsCard data={data} />
        </div>
      </div>
      {data.accounts && <AccountsCard accounts={data.accounts} />}
    </>
  );
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
                {fix !== '' && <p className={styles.alertFix}>{fix}</p>}
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

function Overview({ data }: { data: OpsDashboard }) {
  const { t, i18n } = useTranslation();
  const f = new Format(i18n.language);
  const people = new Set(data.rooms.flatMap((r) => r.participants.map((p) => p.userId))).size;
  const connections = data.rooms.reduce((n, r) => n + r.participants.reduce((m, p) => m + p.connections.length, 0), 0);
  const shares = data.rooms.reduce((n, r) => n + r.shares.length, 0);
  const { transfer, media } = data;
  const pcs = media.peerConnections;
  return (
    <section aria-label={t('admin.dashboard.overview')}>
      <dl className={styles.stats}>
        <Stat
          label={t('admin.dashboard.stat.sending')}
          value={f.bitrate(transfer.egressBps)}
          detail={t('admin.dashboard.stat.receiving', { rate: f.bitrate(transfer.ingressBps) })}
        />
        <Stat
          label={t('admin.dashboard.stat.people')}
          value={f.number(people)}
          detail={t('admin.dashboard.stat.connections', { count: connections })}
        />
        <Stat
          label={t('admin.dashboard.stat.shares')}
          value={f.number(shares)}
          detail={t('admin.dashboard.stat.media', {
            udp: pcs.udp,
            tcp443: pcs.tcp443,
            tcp7882: pcs.tcp7882,
          })}
        />
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

/** "web 0.3.0 on iOS": what a connection is. */
function connectionText(c: ConnectionLive, t: TFunction): string {
  return t('admin.dashboard.connection', { kind: c.kind, version: c.version, os: c.os });
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

function LiveRooms({ rooms }: { rooms: readonly RoomLive[] }) {
  const { t } = useTranslation();
  const live = rooms.filter((r) => r.participants.length > 0 || r.shares.length > 0);
  return (
    <Section title={t('admin.dashboard.rooms.title')}>
      {live.length === 0 ? (
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
                      <Note>
                        {' '}
                        {t('admin.dashboard.rooms.via', {
                          transport: transportName(c.transport, t),
                          rtt: c.rttMs,
                        })}
                      </Note>
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

function ServerCard({ data }: { data: OpsDashboard }) {
  const { t, i18n } = useTranslation();
  const f = new Format(i18n.language);
  const { server } = data;
  const { tls } = server;
  return (
    <Section title={t('admin.dashboard.server.title')}>
      <dl className={styles.facts}>
        <Fact term={t('admin.dashboard.server.version')}>{server.version}</Fact>
        <Fact term={t('admin.dashboard.server.started')}>
          <When at={server.startedAt} mode="relative" fallback={t('admin.unknown')} />
        </Fact>
        <Fact term={t('admin.dashboard.server.origin')}>
          {server.origin === '' ? t('admin.unknown') : server.origin}
        </Fact>
        <Fact term={t('admin.dashboard.server.tls')}>
          {tls.mode === TLSModeOff ? (
            t('admin.dashboard.server.tlsOff')
          ) : (
            <Lines>
              <span>
                {tls.names.length > 0
                  ? t('admin.dashboard.server.tlsFor', { mode: tls.mode, names: f.list(tls.names) })
                  : tls.mode}{' '}
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
              {tls.lastErrorCode !== undefined && tls.lastErrorCode !== '' && (
                <Note>{t('admin.dashboard.server.tlsLastError', { code: tls.lastErrorCode })}</Note>
              )}
            </Lines>
          )}
        </Fact>
        <Fact term={t('admin.dashboard.server.publicIp')}>
          <Lines>
            <span>{server.publicIpv4 === '' ? t('admin.dashboard.server.noIpv4') : server.publicIpv4}</span>
            {server.publicIpv6 !== '' && <span>{server.publicIpv6}</span>}
            <Note>{t('admin.dashboard.server.nat', { nat: server.nat })}</Note>
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
    </Section>
  );
}

function DoctorCard({ data }: { data: OpsDashboard }) {
  const { t } = useTranslation();
  const d = data.doctor;
  return (
    <Section title={t('admin.dashboard.doctor.title')}>
      {d ? (
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

function ClientsCard({ data }: { data: OpsDashboard }) {
  const { t } = useTranslation();
  return (
    <Section title={t('admin.dashboard.clients.title')}>
      {data.clients.length === 0 ? (
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
                c.kind,
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
