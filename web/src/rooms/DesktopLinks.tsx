// The desktop app's deep links on the page (docs/m2/03 §13.2): what the SPA does with Go's deeplink.open event, and
// with App.PendingLink() for a link that came before the page ran. Go decides the table's rows (App.HandleLink); the
// page does what each answer asks:
//
// - {roomId} (the active server): the room's page (for "", the SPA's own last room, else "/", which picks the
//   server's default room), then App.LinkHandled(). A share live in another room asks M1's switch question first; a
//   link to the room the app is already in sends playback.stop again (handoff.ts requestStop).
// - {add} (an unknown server): /desktop/setup?add=<origin>, the wizard's "Add server {host}?" before any network
//   request (SetupPage answers it).
// - {switch, live} (another known server while a share is live): "You're sharing in {room}. Open {target}? Your
//   share will stop." Open stops the share and makes the server active (the window reloads); Go keeps the link's
//   room pending as {roomId}, which the reloaded page opens (its first pull); Cancel is App.LinkHandled().
// - {error: too_many_servers}: /desktop/setup, whose server step says "You've added 20 servers. Remove one first."
//   once, then tells Go (App.LinkHandled), so no later reload brings it back.
//
// A link that arrives while one of these questions is open is ignored (Go ignores its own; the room question is the
// page's). Nothing is done while the user isn't signed in or the wizard or the relink page is showing: those pages
// read the pending link themselves (SetupPage), and Go keeps it until then; once the page is ready again it pulls
// App.PendingLink() again, so a room link that came meanwhile still opens. Only in the desktop app: InRoomBar mounts
// this once, lazily (a chunk of its own), for every page of the app. Its texts are room.* keys of the main catalog:
// two of them are M1's room switch question.
import { useEffect, useEffectEvent, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useLocation, useNavigate } from 'react-router';

import { useApp } from '../app/context';
import { DesktopPaths } from '../app/guards';
import { useMeQuery } from '../app/me';
import { createLogger } from '../lib/log';
import type { NativeBridge, PendingLink } from '../platform/desktop/bridge';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import { roomPath } from './hooks';
import { roomOf, useRooms } from './roomsQuery';
import { peekRoomRuntime } from './runtime';

const log = createLogger('links');

/** The server of a {switch} answer (Go's LinkAdd). */
export interface LinkTarget {
  origin: string;
  host: string;
  hostUnicode: string;
  roomId: string;
}

/** Every shape of Go's PendingLink (03 §6.3); bridge.ts names the two the router needs. */
export type AnyPendingLink = PendingLink | { switch: LinkTarget; live?: { roomId: string } } | { error: string };

/** What a link asks of the page, decided from its answer and the page's state (desktopLinkAction). */
export type LinkAction =
  | { kind: 'none' }
  /** Navigate to `to`; `again`: the room the page is in, whose tabs get playback.stop again. */
  | { kind: 'room'; to: string; again: boolean }
  /** M1's switch question: a share is live in another room. */
  | { kind: 'askRoom'; roomId: string }
  | { kind: 'add'; to: string }
  | { kind: 'askServer'; target: LinkTarget; liveRoomId: string }
  | { kind: 'tooMany'; to: string };

/** The page's state that a link's action depends on. */
export interface LinkContext {
  /** The session's room (RoomSession.roomId), null when it is in none or there is no runtime yet. */
  roomId: string | null;
  /** The page's own share is live (the desktop's native share, adopted by the session). */
  sharing: boolean;
  /** prefsStore.lastRoomId. */
  lastRoomId: string | null;
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** The action of one of Go's answers (03 §13.2, the table in the header). An unknown shape does nothing. */
export function desktopLinkAction(p: unknown, ctx: LinkContext): LinkAction {
  if (!isObject(p)) return { kind: 'none' };
  if (isObject(p['add']) && typeof p['add']['origin'] === 'string') {
    return { kind: 'add', to: `${DesktopPaths.setup}?add=${encodeURIComponent(p['add']['origin'])}` };
  }
  const sw = p['switch'];
  if (isObject(sw) && typeof sw['origin'] === 'string') {
    const live = isObject(p['live']) && typeof p['live']['roomId'] === 'string' ? p['live']['roomId'] : '';
    const target: LinkTarget = {
      origin: sw['origin'],
      host: typeof sw['host'] === 'string' ? sw['host'] : '',
      hostUnicode: typeof sw['hostUnicode'] === 'string' ? sw['hostUnicode'] : '',
      roomId: typeof sw['roomId'] === 'string' ? sw['roomId'] : '',
    };
    return { kind: 'askServer', target, liveRoomId: live };
  }
  if (p['error'] === 'too_many_servers') return { kind: 'tooMany', to: DesktopPaths.setup };
  if (typeof p['roomId'] !== 'string') return { kind: 'none' };
  // "" is the SPA's own last room; without one, "/" picks the server's default room (RootRedirect).
  const target = p['roomId'] !== '' ? p['roomId'] : (ctx.lastRoomId ?? '');
  if (target === '') return { kind: 'room', to: '/', again: false };
  if (target === ctx.roomId) return { kind: 'room', to: roomPath(target), again: true };
  // A link never stops a share silently (02 §12.3): joining another room would end it (01 §8.4).
  if (ctx.sharing && ctx.roomId !== null) return { kind: 'askRoom', roomId: target };
  return { kind: 'room', to: roomPath(target), again: false };
}

/** The pages that read the pending link themselves, or come before a signed-in page. */
function waitsForLink(pathname: string): boolean {
  return pathname.startsWith(DesktopPaths.setup) || pathname.startsWith(DesktopPaths.relink);
}

type Question = { kind: 'room'; roomId: string } | { kind: 'server'; target: LinkTarget; liveRoomId: string };

export function DesktopLinks() {
  const app = useApp();
  const bridge = app.platform.desktop?.bridge ?? null;
  if (bridge === null) return null;
  return <Links bridge={bridge} />;
}

function Links({ bridge }: { bridge: NativeBridge }) {
  const app = useApp();
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const me = useMeQuery();
  const [question, setQuestion] = useState<Question | null>(null);
  const [busy, setBusy] = useState(false);
  const signedIn = me.data != null;
  const ready = signedIn && !waitsForLink(pathname);

  const handled = (): void => {
    bridge.app.linkHandled().catch((err: unknown) => {
      log.debug('LinkHandled failed', { error: err });
    });
  };

  const act = useEffectEvent((p: AnyPendingLink | null | undefined): void => {
    if (p == null) return;
    if (question !== null) {
      log.debug('a link arrived while a question is open; ignored');
      return;
    }
    const runtime = peekRoomRuntime(app);
    const action = desktopLinkAction(p, {
      roomId: runtime?.session.roomId ?? null,
      sharing: runtime?.session.share != null,
      lastRoomId: app.prefs.getState().lastRoomId,
    });
    switch (action.kind) {
      case 'none':
        return;
      case 'room':
        handled();
        void navigate(action.to);
        if (action.again) runtime?.handoff.requestStop();
        return;
      case 'askRoom':
        setQuestion({ kind: 'room', roomId: action.roomId });
        return;
      case 'add':
      case 'tooMany':
        // The wizard asks (SetupPage reads the same pending link).
        void navigate(action.to);
        return;
      case 'askServer':
        setQuestion({ kind: 'server', target: action.target, liveRoomId: action.liveRoomId });
        return;
    }
  });

  // A link that came before the page (the one that started the app, a reload after a server switch), and one that
  // came while the page wasn't ready (signed out, the relink page: Go keeps it pending until App.LinkHandled), pulled
  // each time the page becomes ready; then every link Go forwards. A pull after the first leaves the wizard's
  // answers ({add}, {error}) to the wizard: the page typically just came from it, and its pending link stays there
  // until the wizard is done with it.
  const pulled = useRef(false);
  const pulledBefore = useRef(false);
  useEffect(() => {
    if (!ready) {
      pulled.current = false;
      return;
    }
    if (pulled.current) return;
    pulled.current = true;
    const first = !pulledBefore.current;
    pulledBefore.current = true;
    bridge.app.pendingLink().then(
      (p: AnyPendingLink | null) => {
        if (!first && isObject(p) && ('add' in p || 'error' in p)) return;
        act(p);
      },
      (err: unknown) => {
        log.debug('PendingLink failed', { error: err });
      },
    );
  }, [bridge, ready]);
  useEffect(() => {
    if (!ready) return undefined;
    return bridge.on('deeplink.open', (p: AnyPendingLink) => {
      act(p);
    });
  }, [bridge, ready]);

  const close = (): void => {
    setQuestion(null);
    setBusy(false);
  };

  if (question === null) return null;
  if (question.kind === 'room') {
    return (
      <RoomQuestion
        roomId={question.roomId}
        onAnswer={(go) => {
          handled();
          close();
          if (go) void navigate(roomPath(question.roomId));
        }}
      />
    );
  }
  return (
    <ServerQuestion
      target={question.target}
      liveRoomId={question.liveRoomId}
      busy={busy}
      onCancel={() => {
        handled();
        close();
      }}
      onOpen={() => {
        setBusy(true);
        // The share first (Auth.SwitchServer refuses while one is live), then the server: the window reloads.
        bridge.share
          .stop()
          .then(() => bridge.auth.switchServer(question.target.origin))
          .catch((err: unknown) => {
            log.warn('the link’s server switch failed', { error: err });
            handled();
            close();
          });
      }}
    />
  );
}

/** M1's switch question (05 §11.1), for a link to another room while the page shares. */
function RoomQuestion({ roomId, onAnswer }: { roomId: string; onAnswer: (go: boolean) => void }) {
  const { t } = useTranslation();
  const rooms = useRooms();
  const name = roomOf(rooms.data, roomId)?.name ?? roomId;
  return (
    <Dialog
      open
      size="sm"
      title={t('room.switcher.confirm.title', { room: name })}
      onClose={() => {
        onAnswer(false);
      }}
      footer={
        <>
          <Button
            onClick={() => {
              onAnswer(false);
            }}
          >
            {t('common.cancel')}
          </Button>
          <Button
            variant="danger"
            onClick={() => {
              onAnswer(true);
            }}
          >
            {t('room.switcher.confirm.action')}
          </Button>
        </>
      }
    >
      <p>{t('room.switcher.confirm.body')}</p>
    </Dialog>
  );
}

/** 02 §12.3's question for a link to another known server while a share is live. */
function ServerQuestion({
  target,
  liveRoomId,
  busy,
  onCancel,
  onOpen,
}: {
  target: LinkTarget;
  liveRoomId: string;
  busy: boolean;
  onCancel: () => void;
  onOpen: () => void;
}) {
  const { t } = useTranslation();
  const rooms = useRooms();
  const host = target.hostUnicode !== '' ? target.hostUnicode : target.host;
  const liveName = roomOf(rooms.data, liveRoomId)?.name ?? liveRoomId;
  return (
    <Dialog
      open
      size="sm"
      title={t('room.deepLink.switch.title', { target: host })}
      dismissible={!busy}
      onClose={onCancel}
      footer={
        <>
          <Button disabled={busy} onClick={onCancel}>
            {t('common.cancel')}
          </Button>
          <Button variant="danger" loading={busy} onClick={onOpen}>
            {t('room.deepLink.switch.action')}
          </Button>
        </>
      }
    >
      <p>{t('room.deepLink.switch.body', { room: liveName, target: host })}</p>
    </Dialog>
  );
}
