import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { useApp } from '../app/context';
import { Notice } from '../auth/Notice';
import { CodeRoomNameTaken, FieldRequired, FieldTooLong, type Field, type Room } from '../protocol/api.gen';
import { TopicRooms } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { TextField } from '../ui/Field';
import { ActionDialog, DialogText } from './ActionDialog';
import { adminApi, refresh, roomsQueryOptions } from './adminApi';
import { AdminPage, Loaded, Section, Tag, useHeadingFocusAfter } from './AdminPage';
import { DataTable, Inline, Row } from './DataTable';
import { fieldCodes, runeLength } from './formProblem';
import styles from './RoomsPage.module.css';
import { useAction } from './useAction';
import { When } from './When';

/** A room name holds 1 to 40 characters (03 §8). */
export const ROOM_NAME_MAX_LENGTH = 40;

/**
 * Where the browser stops the typing. It counts UTF-16 units and the server characters, and one character is at most
 * two units, so this never refuses a name the server would take; checkRoomName says when a name is too long.
 */
const ROOM_NAME_INPUT_MAX = 2 * ROOM_NAME_MAX_LENGTH;

/** required or too_long, else null. The server checks the rest (03 §8: PRECIS Nickname, case-insensitive names). */
export function checkRoomName(value: string): Field | null {
  const name = value.trim();
  if (name === '') return FieldRequired;
  if (runeLength(name) > ROOM_NAME_MAX_LENGTH) return FieldTooLong;
  return null;
}

/**
 * /admin/rooms (05 §15.3, 03 §8): list, create, rename and delete rooms. The default room (Lounge) can be renamed
 * but never deleted, so there is always a room to land in: it has no Delete button, and a server that is asked all
 * the same answers 409 room_is_default.
 *
 * Friends see the room list only once there is a second room (showRoomList), so this page is where it starts.
 */
export function RoomsPage() {
  const { t } = useTranslation();
  const { queryClient, ui } = useApp();
  const rooms = useQuery(roomsQueryOptions());
  const [name, setName] = useState('');
  const [renaming, setRenaming] = useState<Room | null>(null);
  const [deleting, setDeleting] = useState<Room | null>(null);
  const rowGone = useHeadingFocusAfter(deleting !== null);

  const create = useAction({
    fields: ['name'],
    validate: (v: string) => fieldCodes({ name: checkRoomName(v) }),
    request: (v) => adminApi.createRoom({ name: v.trim() }),
    onSuccess: (res) => {
      refresh(queryClient, TopicRooms);
      ui.getState().toast({ kind: 'success', message: t('admin.rooms.create.done', { name: res.room.name }) });
      setName('');
    },
    codeFields: { [CodeRoomNameTaken]: 'name' },
  });
  const { formRef: createFormRef } = create;

  const remove = useAction({
    request: (room: Room) => adminApi.deleteRoom(room.id),
    onSuccess: (_res, room) => {
      refresh(queryClient, TopicRooms);
      ui.getState().toast({ kind: 'success', message: t('admin.rooms.delete.done', { name: room.name }) });
      rowGone();
      setDeleting(null);
    },
    onError: () => {
      // The list may be behind: another admin deleted the room, or made it the last one.
      refresh(queryClient, TopicRooms);
    },
  });

  return (
    <AdminPage title={t('admin.rooms.title')} lead={t('admin.rooms.lead')}>
      <Section title={t('admin.rooms.create.title')}>
        <form
          ref={createFormRef}
          className={styles.form}
          noValidate
          onSubmit={(e) => {
            e.preventDefault();
            create.submit(name);
          }}
        >
          {create.formError !== null && <Notice>{create.formError}</Notice>}
          <div className={styles.row}>
            <TextField
              className={styles.name}
              label={t('admin.rooms.name')}
              name="name"
              autoComplete="off"
              required
              maxLength={ROOM_NAME_INPUT_MAX}
              value={name}
              onChange={(e) => {
                setName(e.target.value);
              }}
              hint={t('admin.rooms.nameHint', { max: ROOM_NAME_MAX_LENGTH })}
              error={create.fieldErrors.name}
            />
            <Button type="submit" variant="primary" loading={create.busy} className={styles.submit}>
              {t('admin.rooms.create.submit')}
            </Button>
          </div>
        </form>
      </Section>

      <Loaded query={rooms}>
        {(data) => (
          <DataTable
            caption={t('admin.rooms.title')}
            columns={[t('admin.rooms.col.room'), t('admin.rooms.col.live'), t('admin.rooms.col.created'), '']}
          >
            {data.rooms.map((room) => (
              <Row
                key={room.id}
                cells={[
                  <Inline>
                    <Link to={`/r/${encodeURIComponent(room.id)}`}>{room.name}</Link>
                    {room.isDefault && <Tag tone="accent">{t('admin.rooms.default')}</Tag>}
                  </Inline>,
                  room.live.participants === 0 && room.live.shares === 0
                    ? t('admin.rooms.nobody')
                    : t('admin.rooms.live', {
                        people: t('admin.rooms.people', { count: room.live.participants }),
                        shares: t('admin.rooms.shares', { count: room.live.shares }),
                      }),
                  <When at={room.createdAt} mode="date" />,
                  <>
                    <Button
                      size="sm"
                      aria-label={t('admin.rooms.rename.name', { name: room.name })}
                      onClick={() => {
                        setRenaming(room);
                      }}
                    >
                      {t('admin.rooms.rename.action')}
                    </Button>
                    {!room.isDefault && (
                      <Button
                        size="sm"
                        aria-label={t('admin.rooms.delete.name', { name: room.name })}
                        onClick={() => {
                          remove.clear();
                          setDeleting(room);
                        }}
                      >
                        {t('admin.rooms.delete.action')}
                      </Button>
                    )}
                  </>,
                ]}
              />
            ))}
          </DataTable>
        )}
      </Loaded>

      {renaming && (
        <RenameRoomDialog
          room={renaming}
          onClose={() => {
            setRenaming(null);
          }}
        />
      )}
      {deleting && (
        <ActionDialog
          title={t('admin.rooms.delete.title', { name: deleting.name })}
          action={remove}
          onClose={() => {
            setDeleting(null);
          }}
          onSubmit={() => {
            remove.submit(deleting);
          }}
          submitLabel={t('admin.rooms.delete.submit')}
          danger
        >
          <DialogText>{t('admin.rooms.delete.body')}</DialogText>
          {deleting.live.participants > 0 && (
            <DialogText>{t('admin.rooms.delete.bodyLive', { count: deleting.live.participants })}</DialogText>
          )}
        </ActionDialog>
      )}
    </AdminPage>
  );
}

function RenameRoomDialog({ room, onClose }: { room: Room; onClose: () => void }) {
  const { t } = useTranslation();
  const { queryClient, ui } = useApp();
  const [name, setName] = useState(room.name);
  const action = useAction({
    fields: ['name'],
    validate: (v: string) => fieldCodes({ name: checkRoomName(v) }),
    request: (v) => adminApi.patchRoom(room.id, { name: v.trim() }),
    onSuccess: (res) => {
      refresh(queryClient, TopicRooms);
      ui.getState().toast({
        kind: 'success',
        message: t('admin.rooms.rename.done', { from: room.name, to: res.room.name }),
      });
      onClose();
    },
    codeFields: { [CodeRoomNameTaken]: 'name' },
  });
  return (
    <ActionDialog
      title={t('admin.rooms.rename.title', { name: room.name })}
      size="md"
      action={action}
      onClose={onClose}
      onSubmit={() => {
        // The same name is not a request.
        if (name.trim() === room.name) onClose();
        else action.submit(name);
      }}
      submitLabel={t('admin.rooms.rename.submit')}
    >
      <TextField
        label={t('admin.rooms.name')}
        name="name"
        autoComplete="off"
        required
        maxLength={ROOM_NAME_INPUT_MAX}
        value={name}
        onChange={(e) => {
          setName(e.target.value);
        }}
        hint={t('admin.rooms.nameHint', { max: ROOM_NAME_MAX_LENGTH })}
        error={action.fieldErrors.name}
      />
    </ActionDialog>
  );
}
