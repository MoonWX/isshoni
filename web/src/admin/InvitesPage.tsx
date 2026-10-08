import { useQuery } from '@tanstack/react-query';
import type { TFunction } from 'i18next';
import { useEffect, useId, useRef, useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { useApp } from '../app/context';
import { isAdmin } from '../app/guards';
import { useInfo } from '../app/info';
import { Notice } from '../auth/Notice';
import { useMe } from '../auth/useMe';
import {
  FieldTooLong,
  InviteStateActive,
  InviteStateExpired,
  InviteStateRevoked,
  InviteStateUsedUp,
  RegistrationModeClosed,
  type CreateInviteRequest,
  type CreateInviteResponse,
  type Invite,
} from '../protocol/api.gen';
import { TopicAdminInvites } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { Field, inputClass, TextField } from '../ui/Field';
import { ActionDialog, DialogText } from './ActionDialog';
import { adminApi, invitesQueryOptions, refresh, settingsQueryOptions } from './adminApi';
import { AdminPage, Empty, Loaded, Section, Tag, useHeadingFocusAfter, type TagTone } from './AdminPage';
import { DataTable, Lines, Note, Row } from './DataTable';
import { fieldCodes, runeLength } from './formProblem';
import styles from './InvitesPage.module.css';
import { OneTimeLink } from './OneTimeLink';
import { useAction } from './useAction';
import { When } from './When';

/** The expiry choices of 05 §15.3, in hours: 1 hour, 1 day, 7 days, 30 days. */
export const EXPIRY_CHOICES: readonly number[] = [1, 24, 168, 720];
/** The use-count choices of 05 §15.3. */
export const USES_CHOICES: readonly number[] = [1, 10, 50];
/** An invite's note holds at most 64 characters (03 §5). */
export const NOTE_MAX_LENGTH = 64;
/** The value of the <option> that leaves a choice to the server. */
const SERVER_DEFAULT = '';

/**
 * The choices, with the server's default and the value in force added when they aren't among them (an admin set
 * another default). Each stays listed whatever is selected, so it can be picked again.
 */
function withChoices(choices: readonly number[], ...values: readonly (number | null | undefined)[]): number[] {
  const all = new Set(choices);
  for (const value of values) if (typeof value === 'number') all.add(value);
  return [...all].sort((a, b) => a - b);
}

/** A <select>'s value as a choice: null for "Server default". */
function choiceOf(value: string): number | null {
  return value === SERVER_DEFAULT ? null : Number(value);
}

/** "1 hour", "7 days": whole days as days, anything else as hours. */
function expiryLabel(hours: number, t: TFunction): string {
  return hours % 24 === 0
    ? t('admin.invites.create.days', { count: hours / 24 })
    : t('admin.invites.create.hours', { count: hours });
}

const STATE_TONES: Readonly<Record<string, TagTone>> = {
  [InviteStateActive]: 'success',
  [InviteStateExpired]: 'neutral',
  [InviteStateUsedUp]: 'neutral',
  [InviteStateRevoked]: 'danger',
};

function stateLabel(state: string, t: TFunction): string {
  switch (state) {
    case InviteStateActive:
      return t('admin.invites.state.active');
    case InviteStateExpired:
      return t('admin.invites.state.expired');
    case InviteStateUsedUp:
      return t('admin.invites.state.used_up');
    case InviteStateRevoked:
      return t('admin.invites.state.revoked');
    default:
      // A state from a newer server.
      return state;
  }
}

interface CreateValues {
  /** null: left to the server. */
  expiresInHours: number | null;
  maxUses: number | null;
  note: string;
}

/**
 * /admin/invites (05 §15.3, 03 §7.9): create an invite link, list the invites, revoke one. Admins see every
 * invite; a member who may create invites (RequireInviter let them in) sees their own, which the server filters.
 *
 * The link shows once, right after creation: the server keeps only its hash. The list asks for every state
 * (?state=all) and hides the invites that no longer work unless the box is ticked.
 *
 * A new invite starts with the server's defaults for expiry and uses (the settings of 03 §9). An admin's form reads
 * them from the settings and shows them as values. A member can't read the settings: their form says "Server
 * default" and leaves the field out of the request, and the server applies its setting (03 §7.9). The same goes
 * for an admin for as long as the settings haven't arrived.
 */
export function InvitesPage() {
  const { t, i18n } = useTranslation();
  const { queryClient, ui } = useApp();
  const me = useMe().data;
  const admin = me ? isAdmin(me) : false;
  const info = useInfo().data;
  const invites = useQuery(invitesQueryOptions());
  // The invite defaults are settings (03 §9), which only admins can read.
  const settings = useQuery({ ...settingsQueryOptions(), enabled: admin }).data?.settings;

  /** What was picked; null for a choice left alone, which follows the server's default. */
  const [pickedExpiry, setPickedExpiry] = useState<number | null>(null);
  const [pickedUses, setPickedUses] = useState<number | null>(null);
  const [note, setNote] = useState('');
  const [created, setCreated] = useState<CreateInviteResponse | null>(null);
  const [showInactive, setShowInactive] = useState(false);
  const [revoking, setRevoking] = useState<Invite | null>(null);
  // A revoked invite has no Revoke button anymore, whether its row stays or goes.
  const buttonGone = useHeadingFocusAfter(revoking !== null);
  const showInactiveId = useId();

  // The server's defaults; undefined while this form doesn't know them.
  const defaultExpiry = settings?.inviteDefaultTtlHours;
  const defaultUses = settings?.inviteDefaultMaxUses;
  // What the form shows and sends; null is "Server default": nothing picked, and the default's number not known.
  const expiresInHours = pickedExpiry ?? defaultExpiry ?? null;
  const maxUses = pickedUses ?? defaultUses ?? null;
  const expiryChoices = withChoices(EXPIRY_CHOICES, defaultExpiry, expiresInHours);
  const usesChoices = withChoices(USES_CHOICES, defaultUses, maxUses);

  const create = useAction<CreateValues, CreateInviteResponse, 'note' | 'expiresInHours' | 'maxUses'>({
    fields: ['note', 'expiresInHours', 'maxUses'],
    validate: (v) => fieldCodes({ note: runeLength(v.note.trim()) > NOTE_MAX_LENGTH ? FieldTooLong : null }),
    request: (v) => {
      const trimmed = v.note.trim();
      // Every field is optional (03 §12.4.5): one that is left out gets the setting's default.
      const body: CreateInviteRequest = {
        ...(v.expiresInHours !== null ? { expiresInHours: v.expiresInHours } : {}),
        ...(v.maxUses !== null ? { maxUses: v.maxUses } : {}),
        ...(trimmed !== '' ? { note: trimmed } : {}),
      };
      return adminApi.createInvite(body);
    },
    onSuccess: (res) => {
      refresh(queryClient, TopicAdminInvites);
      setCreated(res);
      setNote('');
    },
  });
  // The <form>'s ref, as its own value: the form is in this component, not behind an ActionDialog.
  const { formRef: createFormRef } = create;

  const revoke = useAction({
    request: (invite: Invite) => adminApi.revokeInvite(invite.id),
    onSuccess: () => {
      refresh(queryClient, TopicAdminInvites);
      ui.getState().toast({ kind: 'success', message: t('admin.invites.revoke.done') });
      buttonGone();
      setRevoking(null);
    },
    onError: () => {
      // Whatever went wrong, the list may be behind (the invite expired or was revoked elsewhere).
      refresh(queryClient, TopicAdminInvites);
    },
  });

  // The new link is why the admin is here: focus goes to it, selected, ready to copy.
  const createdSection = useRef<HTMLDivElement>(null);
  const createdUrl = created?.url;
  useEffect(() => {
    if (createdUrl !== undefined) createdSection.current?.querySelector('input')?.focus();
  }, [createdUrl]);

  const names = new Intl.ListFormat(i18n.language, { style: 'long', type: 'conjunction' });

  return (
    <AdminPage title={t('admin.invites.title')} lead={admin ? t('admin.invites.lead') : t('admin.invites.leadMember')}>
      {info?.registration === RegistrationModeClosed && (
        <Notice tone="info">
          {admin ? (
            <Trans i18nKey="admin.invites.closedAdmin" components={{ a: <Link to="/admin/settings" /> }} />
          ) : (
            t('admin.invites.closed')
          )}
        </Notice>
      )}

      <Section title={t('admin.invites.create.title')}>
        <form
          ref={createFormRef}
          className={styles.form}
          noValidate
          onSubmit={(e) => {
            e.preventDefault();
            create.submit({ expiresInHours, maxUses, note });
          }}
        >
          {create.formError !== null && (
            <div className={styles.wide}>
              <Notice>{create.formError}</Notice>
            </div>
          )}
          <Field label={t('admin.invites.create.expires')} error={create.fieldErrors.expiresInHours}>
            {(control) => (
              <select
                {...control}
                className={inputClass}
                name="expiresInHours"
                value={expiresInHours ?? SERVER_DEFAULT}
                onChange={(e) => {
                  setPickedExpiry(choiceOf(e.target.value));
                }}
              >
                {defaultExpiry === undefined && (
                  <option value={SERVER_DEFAULT}>{t('admin.invites.create.serverDefault')}</option>
                )}
                {expiryChoices.map((hours) => (
                  <option key={hours} value={hours}>
                    {expiryLabel(hours, t)}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <Field label={t('admin.invites.create.uses')} error={create.fieldErrors.maxUses}>
            {(control) => (
              <select
                {...control}
                className={inputClass}
                name="maxUses"
                value={maxUses ?? SERVER_DEFAULT}
                onChange={(e) => {
                  setPickedUses(choiceOf(e.target.value));
                }}
              >
                {defaultUses === undefined && (
                  <option value={SERVER_DEFAULT}>{t('admin.invites.create.serverDefault')}</option>
                )}
                {usesChoices.map((uses) => (
                  <option key={uses} value={uses}>
                    {t('admin.invites.create.times', { count: uses })}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <TextField
            className={styles.wide}
            label={t('admin.invites.create.note')}
            name="note"
            autoComplete="off"
            // In UTF-16 units, so twice the limit in characters: never short of what the server takes.
            maxLength={2 * NOTE_MAX_LENGTH}
            value={note}
            onChange={(e) => {
              setNote(e.target.value);
            }}
            hint={t('admin.invites.create.noteHint')}
            error={create.fieldErrors.note}
          />
          <div className={styles.wide}>
            <Button type="submit" variant="primary" loading={create.busy}>
              {t('admin.invites.create.submit')}
            </Button>
          </div>
        </form>
      </Section>

      {created && (
        <div ref={createdSection}>
          <Section title={t('admin.invites.created.title')}>
            <OneTimeLink
              label={t('admin.invites.created.label')}
              url={created.url}
              hint={t('admin.invites.created.hint')}
            />
            <div>
              <Button
                onClick={() => {
                  setCreated(null);
                }}
              >
                {t('admin.link.done')}
              </Button>
            </div>
          </Section>
        </div>
      )}

      <Loaded query={invites}>
        {(data) => {
          const inactive = data.invites.filter((i) => i.state !== InviteStateActive).length;
          const shown = showInactive ? data.invites : data.invites.filter((i) => i.state === InviteStateActive);
          return (
            <>
              {inactive > 0 && (
                <div className={styles.filter}>
                  <input
                    id={showInactiveId}
                    type="checkbox"
                    checked={showInactive}
                    onChange={(e) => {
                      setShowInactive(e.target.checked);
                    }}
                  />
                  <label htmlFor={showInactiveId}>{t('admin.invites.showInactive', { count: inactive })}</label>
                </div>
              )}
              {shown.length === 0 ? (
                <Empty>{data.invites.length === 0 ? t('admin.invites.empty') : t('admin.invites.emptyActive')}</Empty>
              ) : (
                <DataTable
                  caption={t('admin.invites.title')}
                  columns={[
                    t('admin.invites.col.invite'),
                    t('admin.invites.col.createdBy'),
                    t('admin.invites.col.uses'),
                    t('admin.invites.col.expires'),
                    t('admin.invites.col.state'),
                    '',
                  ]}
                >
                  {shown.map((invite) => {
                    const name = invite.note !== '' ? invite.note : t('admin.invites.noNote');
                    return (
                      <Row
                        key={invite.id}
                        cells={[
                          <Lines>
                            <span className={invite.note === '' ? styles.unnamed : undefined}>{name}</span>
                            <Note>
                              <When at={invite.createdAt} mode="date" />
                            </Note>
                          </Lines>,
                          invite.createdBy?.username ?? t('admin.unknown'),
                          <Lines>
                            <span>{t('admin.invites.usesOf', { uses: invite.uses, max: invite.maxUses })}</span>
                            {invite.redeemedBy.length > 0 && (
                              <Note>
                                {t('admin.invites.usedBy', {
                                  names: names.format(invite.redeemedBy.map((u) => u.username)),
                                })}
                              </Note>
                            )}
                          </Lines>,
                          <When at={invite.expiresAt} />,
                          <Tag tone={STATE_TONES[invite.state] ?? 'neutral'}>{stateLabel(invite.state, t)}</Tag>,
                          invite.state === InviteStateActive ? (
                            <Button
                              size="sm"
                              aria-label={t('admin.invites.revoke.name', { name })}
                              onClick={() => {
                                revoke.clear();
                                setRevoking(invite);
                              }}
                            >
                              {t('admin.invites.revoke.action')}
                            </Button>
                          ) : null,
                        ]}
                      />
                    );
                  })}
                </DataTable>
              )}
            </>
          );
        }}
      </Loaded>

      {revoking && (
        <ActionDialog
          title={t('admin.invites.revoke.title')}
          action={revoke}
          onClose={() => {
            setRevoking(null);
          }}
          onSubmit={() => {
            revoke.submit(revoking);
          }}
          submitLabel={t('admin.invites.revoke.submit')}
          danger
        >
          <DialogText>{t('admin.invites.revoke.body')}</DialogText>
        </ActionDialog>
      )}
    </AdminPage>
  );
}
