import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import type { TFunction } from 'i18next';
import { useEffect, useRef, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router';

import { useApp } from '../app/context';
import { AuditOutcomeDenied, type AuditEntry, type AuditRef } from '../protocol/api.gen';
import { Button } from '../ui/Button';
import { Field, inputClass } from '../ui/Field';
import { auditQueryOptions, usersQueryOptions, type AuditFilters } from './adminApi';
import { AdminPage, Empty, Loaded, Tag } from './AdminPage';
import styles from './AuditPage.module.css';
import { actionLabel, actorLabel, detailLines, refName, targetKindLabel } from './auditText';
import { DataTable, Inline, Lines, Note, Row } from './DataTable';
import { When } from './When';

/**
 * The groups the Action filter offers: the server matches the action by prefix (03 §6 AuditQuery.ActionPrefix), so
 * "user." is every account action.
 */
const ACTION_GROUPS = ['auth.', 'user.', 'invite.', 'room.', 'settings.', 'session.', 'setup.', 'secrets.'] as const;

function groupLabel(prefix: string, t: TFunction): string | null {
  switch (prefix) {
    case 'auth.':
      return t('admin.audit.filter.actions.auth');
    case 'user.':
      return t('admin.audit.filter.actions.user');
    case 'invite.':
      return t('admin.audit.filter.actions.invite');
    case 'room.':
      return t('admin.audit.filter.actions.room');
    case 'settings.':
      return t('admin.audit.filter.actions.settings');
    case 'session.':
      return t('admin.audit.filter.actions.session');
    case 'setup.':
      return t('admin.audit.filter.actions.setup');
    case 'secrets.':
      return t('admin.audit.filter.actions.secrets');
    default:
      return null;
  }
}

/** A filter value from the address bar: an action prefix or an ID, never longer than either can be. */
function param(params: URLSearchParams, name: string): string {
  const value = params.get(name) ?? '';
  return value.length <= 64 ? value : '';
}

interface Choice {
  readonly id: string;
  readonly name: string;
}

/**
 * The options of a "who" filter: every user, plus the filtered ID when it isn't a current user (a deleted user, a
 * room, an invite), named as the log's rows name it.
 */
function whoChoices(users: readonly Choice[], id: string, name: string | undefined): Choice[] {
  const choices = [...users].sort((a, b) => a.name.localeCompare(b.name));
  if (id !== '' && !choices.some((c) => c.id === id)) choices.push({ id, name: name ?? id });
  return choices;
}

/**
 * /admin/audit (05 §15.3, 03 §10): the log of logins and admin actions, newest first, kept for 30 days. Filters by
 * action group, by who did it and by who or what it was done to; a name in a row is a shortcut to its filter.
 * "Load more" asks for the next page with the previous one's nextBefore, until the server answers null.
 *
 * The filters live in the address (?action=user.&actor=<id>&target=<id>), so a filtered log can be linked to and
 * the back button undoes a filter. IP addresses show here and nowhere else in the admin pages (03 §12.4.8).
 */
export function AuditPage() {
  const { t } = useTranslation();
  const { ui } = useApp();
  const [params, setParams] = useSearchParams();
  const filters: AuditFilters = {
    action: param(params, 'action'),
    actor: param(params, 'actor'),
    target: param(params, 'target'),
  };
  const filtered = filters.action !== '' || filters.actor !== '' || filters.target !== '';
  const log = useInfiniteQuery(auditQueryOptions(filters));
  // Only for the filters' options: without it they offer what the rows name.
  const users = useQuery(usersQueryOptions()).data?.users ?? [];
  const entries = log.data?.pages.flatMap((page) => page.entries) ?? [];

  const setFilter = (name: keyof AuditFilters, value: string): void => {
    setParams((prev) => {
      const next = new URLSearchParams(prev);
      if (value === '') next.delete(name);
      else next.set(name, value);
      return next;
    });
  };

  // "Load more" is gone once the last page is in: focus goes to the line that says so.
  const end = useRef<HTMLParagraphElement>(null);
  const loadedMore = useRef(false);
  const { hasNextPage } = log;
  useEffect(() => {
    if (!hasNextPage && loadedMore.current) end.current?.focus();
    loadedMore.current = false;
  }, [hasNextPage, entries.length]);

  const userChoices = users.map((u) => ({ id: u.id, name: u.username }));
  const actorName = entries.find((e) => e.actor.id === filters.actor)?.actor.name;
  const targetName = entries.find((e) => e.target?.id === filters.target)?.target?.name;
  const actionChoices: string[] = [...ACTION_GROUPS];
  if (filters.action !== '' && !actionChoices.includes(filters.action)) actionChoices.push(filters.action);

  return (
    <AdminPage title={t('admin.audit.title')} lead={t('admin.audit.lead')}>
      <form
        className={styles.filters}
        aria-label={t('admin.audit.filter.label')}
        onSubmit={(e) => {
          e.preventDefault();
        }}
      >
        <Field label={t('admin.audit.filter.action')}>
          {(control) => (
            <select
              {...control}
              className={inputClass}
              value={filters.action}
              onChange={(e) => {
                setFilter('action', e.target.value);
              }}
            >
              <option value="">{t('admin.audit.filter.anyAction')}</option>
              {actionChoices.map((prefix) => (
                <option key={prefix} value={prefix}>
                  {groupLabel(prefix, t) ?? actionLabel(prefix, t)}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label={t('admin.audit.filter.actor')}>
          {(control) => (
            <select
              {...control}
              className={inputClass}
              value={filters.actor}
              onChange={(e) => {
                setFilter('actor', e.target.value);
              }}
            >
              <option value="">{t('admin.audit.filter.anyone')}</option>
              {whoChoices(userChoices, filters.actor, actorName).map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label={t('admin.audit.filter.target')}>
          {(control) => (
            <select
              {...control}
              className={inputClass}
              value={filters.target}
              onChange={(e) => {
                setFilter('target', e.target.value);
              }}
            >
              <option value="">{t('admin.audit.filter.anything')}</option>
              {whoChoices(userChoices, filters.target, targetName).map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          )}
        </Field>
        {filtered && (
          <Button
            className={styles.clear}
            onClick={() => {
              setParams({});
            }}
          >
            {t('admin.audit.filter.clear')}
          </Button>
        )}
      </form>

      <Loaded query={log}>
        {() =>
          entries.length === 0 ? (
            <Empty>{filtered ? t('admin.audit.emptyFiltered') : t('admin.audit.empty')}</Empty>
          ) : (
            <>
              <DataTable
                caption={t('admin.audit.title')}
                columns={[
                  t('admin.audit.col.action'),
                  t('admin.audit.col.when'),
                  t('admin.audit.col.actor'),
                  t('admin.audit.col.target'),
                  t('admin.audit.col.ip'),
                  t('admin.audit.col.detail'),
                ]}
              >
                {entries.map((entry) => (
                  <Row
                    key={entry.id}
                    cells={[
                      <Inline>
                        <span>{actionLabel(entry.action, t)}</span>
                        {entry.outcome === AuditOutcomeDenied && <Tag tone="danger">{t('admin.audit.denied')}</Tag>}
                      </Inline>,
                      <When at={entry.at} />,
                      <Who
                        who={entry.actor}
                        text={actorLabel(entry.actor, t)}
                        hint={t('admin.audit.filter.byActor', { name: refName(entry.actor, t) })}
                        active={filters.actor !== ''}
                        onPick={(id) => {
                          setFilter('actor', id);
                        }}
                      />,
                      entry.target ? (
                        <Target target={entry.target} active={filters.target !== ''} onPick={setFilter} />
                      ) : null,
                      entry.ip !== undefined && entry.ip !== '' ? (
                        <span className={styles.mono}>{entry.ip}</span>
                      ) : null,
                      detail(entry),
                    ]}
                  />
                ))}
              </DataTable>
              {log.hasNextPage ? (
                <div className={styles.more}>
                  <Button
                    loading={log.isFetchingNextPage}
                    onClick={() => {
                      loadedMore.current = true;
                      void log.fetchNextPage().then((res) => {
                        const count = res.data?.pages.reduce((n, page) => n + page.entries.length, 0);
                        if (count !== undefined) ui.getState().announce(t('admin.audit.showing', { count }));
                      });
                    }}
                  >
                    {t('admin.audit.loadMore')}
                  </Button>
                </div>
              ) : (
                <p ref={end} tabIndex={-1} className={styles.end}>
                  {t('admin.audit.end')}
                </p>
              )}
              {log.isFetchNextPageError && (
                <p className={styles.failed} role="alert">
                  {t('admin.audit.loadMoreFailed')}
                </p>
              )}
            </>
          )
        }
      </Loaded>
    </AdminPage>
  );
}

interface WhoProps {
  who: AuditRef;
  /** The name as the row shows it (translated for cli, system and anonymous). */
  text: string;
  /** What a click does (translated): the button's tooltip and accessible description. */
  hint: string;
  /** The log is already filtered by this column: the name is plain text. */
  active: boolean;
  onPick: (id: string) => void;
}

/** An actor or target. With an ID it is a button that filters the log by it. */
function Who({ who, text, hint, active, onPick }: WhoProps) {
  const { id } = who;
  if (id === undefined || id === '' || active) return <span>{text}</span>;
  return (
    <button
      type="button"
      className={styles.who}
      title={hint}
      onClick={() => {
        onPick(id);
      }}
    >
      {text}
    </button>
  );
}

interface TargetProps {
  target: AuditRef;
  active: boolean;
  onPick: (name: keyof AuditFilters, id: string) => void;
}

function Target({ target, active, onPick }: TargetProps) {
  const { t } = useTranslation();
  const kind = targetKindLabel(target.kind, t);
  // "Settings" and "Setup" are the whole target: there is one of each, with no name.
  const named = (target.name ?? '') !== '' || (target.id ?? '') !== '';
  if (!named) return <span>{kind ?? t('admin.unknown')}</span>;
  const name = refName(target, t);
  return (
    <Lines>
      <Who
        who={target}
        text={name}
        hint={t('admin.audit.filter.byTarget', { name })}
        active={active}
        onPick={(id) => {
          onPick('target', id);
        }}
      />
      {kind !== null && <Note>{kind}</Note>}
    </Lines>
  );
}

/** The row's details as a list of short lines; null when there are none (the cell is then empty). */
function detail(entry: AuditEntry): ReactNode {
  const lines = detailLines(entry.detail);
  if (lines.length === 0) return null;
  return (
    <ul className={styles.detail}>
      {lines.map((line) => (
        <li key={line}>{line}</li>
      ))}
    </ul>
  );
}
