// Web Push on the room page (05 §11.2, §16.3): a lazy chunk that rooms/ loads through roomPushSlots.tsx, so the
// room's own chunk doesn't carry it, and with it the `account` namespace (05 §16.5).
//
// - RoomPushPrompts (mounted with the page): reads this device's push state again on every mount, and with it does
//   the start-up re-post of the subscription (pushState.ts: also after a login in the same page, which mounts the
//   room again); counts the time the page is watched; lets a click on a notification navigate inside the app
//   (useNotificationOpen.ts); and shows the iOS sheet:
//   - in a Safari tab (`needs-install`): the Home Screen steps, at most every 14 days;
//   - on the first launch of the Home Screen app, where push works: "Turn on notifications?", once.
// - NotificationsCard (in the stage's empty state, "Nobody is sharing yet."): after 3 minutes of watching, "Get a
//   notification when friends start sharing? [Turn on] [Not now]" where the browser and the server can push and it
//   is off; dismissal kept 30 days. On Android it also offers "Install app" while the browser has an install prompt
//   (05 §16.3 "Install prompts"), and offers that alone where push is on or can't be.
//
// While the in-app browser banner shows (auth/inAppBanner.ts), the sheet and the card stay hidden: in a chat app's
// browser neither can be followed. They appear once the banner is dismissed. Nothing here asks for the
// notification permission by itself: only a click on Turn on does (05 §16.3).
import '../i18n/lazy/account';

import { Bell, Download } from 'lucide-react';
import { useEffect, useId, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';
import { createStore, type StoreApi } from 'zustand/vanilla';

import { useApp, usePrefs } from '../app/context';
import { useInAppBanner } from '../auth/inAppBanner';
import { Notice } from '../auth/Notice';
import type { Platform } from '../platform/types';
import { ClientOSAndroid, ClientOSIOS } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { Sheet } from '../ui/Sheet';
import { HomeScreenSteps } from './PushHelp';
import {
  APP_LAUNCH_DISMISSAL,
  CARD_DISMISSAL,
  CARD_DISMISSAL_MS,
  HOME_SCREEN_DISMISSAL,
  HOME_SCREEN_DISMISSAL_MS,
  pushErrorMessage,
  usePush,
} from './pushState';
import styles from './RoomPush.module.css';
import { useNotificationOpen } from './useNotificationOpen';

/** The card comes after this much watching: time the room page was open and visible, in one visit. */
export const CARD_AFTER_MS = 3 * 60_000;

/** Per platform (each test's has its own): whether the page has been watched long enough for the card. */
const watchedStores = new WeakMap<Platform, StoreApi<{ watched: boolean }>>();

function watchedStore(platform: Platform): StoreApi<{ watched: boolean }> {
  let store = watchedStores.get(platform);
  if (!store) {
    store = createStore(() => ({ watched: false }));
    watchedStores.set(platform, store);
  }
  return store;
}

/**
 * Calls onDue once the page has been visible for ms in total since the hook mounted. Hidden time doesn't count
 * (a tab in the background, a phone in a pocket); the count starts over with the next mount.
 */
export function useVisibleFor(ms: number, onDue: () => void): void {
  const due = useRef(onDue);
  useEffect(() => {
    due.current = onDue;
  });
  useEffect(() => {
    const doc = globalThis.document;
    let watched = 0;
    let since: number | null = doc.visibilityState === 'hidden' ? null : Date.now();
    let timer: ReturnType<typeof setTimeout> | undefined;
    let done = false;
    const schedule = (): void => {
      clearTimeout(timer);
      if (since === null || done) return;
      timer = setTimeout(
        () => {
          done = true;
          due.current();
        },
        Math.max(0, ms - watched - (Date.now() - since)),
      );
    };
    const onVisibility = (): void => {
      if (doc.visibilityState === 'hidden') {
        if (since !== null) watched += Date.now() - since;
        since = null;
        clearTimeout(timer);
      } else if (since === null) {
        since = Date.now();
        schedule();
      }
    };
    doc.addEventListener('visibilitychange', onVisibility);
    schedule();
    return () => {
      doc.removeEventListener('visibilitychange', onVisibility);
      clearTimeout(timer);
    };
  }, [ms]);
}

/** Whether this device runs the iOS Home Screen app (installed, so the browser tab's sheet is no longer for it). */
function isIOSApp(platform: Platform): boolean {
  return platform.client.os === ClientOSIOS && platform.pwa?.installState() === 'installed';
}

/** Mounted with the room page: the push state, the watch time, notification clicks, the iOS sheet. */
export function RoomPushPrompts() {
  const { platform } = useApp();
  const { refresh } = usePush();
  // Every visit to the room: a new session (a login in this page after a logout or a revoked session) gets the
  // subscription posted again (03 §12.4.6). A read that is running already is shared.
  useEffect(() => {
    void refresh();
  }, [refresh]);
  useNotificationOpen();
  useVisibleFor(CARD_AFTER_MS, () => {
    watchedStore(platform).setState({ watched: true });
  });
  return <HomeScreenSheet />;
}

type SheetMode = 'install' | 'enable';

/** The iOS sheet: the Home Screen steps in a Safari tab, or the first launch's offer in the Home Screen app. */
function HomeScreenSheet() {
  const { t } = useTranslation();
  const { platform, prefs, ui } = useApp();
  const push = usePush();
  const banner = useInAppBanner();
  const homeScreenDismissed = usePrefs((s) => s.isDismissed(HOME_SCREEN_DISMISSAL, HOME_SCREEN_DISMISSAL_MS));
  const appLaunchDismissed = usePrefs((s) => s.isDismissed(APP_LAUNCH_DISMISSAL));
  // The mode the sheet opened in, kept while it closes: the <dialog> stays in the page, closed (05 §16.6).
  const [shown, setShown] = useState<SheetMode | null>(null);
  const [open, setOpen] = useState(false);
  // This sheet's Turn on failed (push.error has the reason; an older failure elsewhere on the page isn't shown).
  const [failed, setFailed] = useState(false);

  let due: SheetMode | null = null;
  if (!banner.showing) {
    if (push.support === 'needs-install') {
      if (!homeScreenDismissed) due = 'install';
    } else if (push.support === 'supported' && push.status === 'off' && !appLaunchDismissed && isIOSApp(platform)) {
      due = 'enable';
    }
  }
  if (due !== null && shown === null) {
    setShown(due);
    setOpen(true);
  }
  if (shown === null) return null;

  const close = (): void => {
    setOpen(false);
    if (shown === 'install') {
      prefs.getState().dismiss(HOME_SCREEN_DISMISSAL);
    } else {
      // Once is enough: the room's card doesn't ask again in the next 30 days either.
      prefs.getState().dismiss(APP_LAUNCH_DISMISSAL);
      prefs.getState().dismiss(CARD_DISMISSAL);
    }
  };

  if (shown === 'install') {
    return (
      <Sheet
        open={open}
        onClose={close}
        title={t('account.push.homeScreen.title')}
        footer={
          <Button variant="primary" onClick={close}>
            {t('account.push.homeScreen.done')}
          </Button>
        }
      >
        <HomeScreenSteps />
      </Sheet>
    );
  }
  return (
    <Sheet
      open={open}
      onClose={close}
      title={t('account.push.appLaunch.title')}
      footer={
        <>
          <Button onClick={close} disabled={push.busy}>
            {t('account.push.appLaunch.notNow')}
          </Button>
          <Button
            variant="primary"
            icon={<Bell />}
            loading={push.busy}
            onClick={() => {
              setFailed(false);
              void push.enable().then((outcome) => {
                if (outcome === 'failed') {
                  setFailed(true); // the reason shows in the sheet
                  return;
                }
                if (outcome === 'on') ui.getState().toast({ kind: 'success', message: t('account.push.enabled') });
                close();
              });
            }}
          >
            {t('account.push.appLaunch.turnOn')}
          </Button>
        </>
      }
    >
      <div className={styles.sheetBody}>
        <p>{t('account.push.appLaunch.body')}</p>
        {failed && push.error !== null && <Notice>{pushErrorMessage(push.error, t)}</Notice>}
      </div>
    </Sheet>
  );
}

/** The one-time card of the room's empty state. */
export function NotificationsCard() {
  const { t } = useTranslation();
  const { platform, prefs, ui } = useApp();
  const push = usePush();
  const banner = useInAppBanner();
  const watched = useStore(watchedStore(platform), (s) => s.watched);
  const dismissed = usePrefs((s) => s.isDismissed(CARD_DISMISSAL, CARD_DISMISSAL_MS));
  // promptInstall() changes what installState() says; this re-reads it.
  const [, setInstallTries] = useState(0);
  // What the last Turn on came to, when that is worth a line: blocked, or a failure (push.error has the reason).
  const [outcome, setOutcome] = useState<'denied' | 'failed' | null>(null);
  const titleId = useId();

  const offerPush = push.support === 'supported' && push.status === 'off';
  const offerInstall = platform.client.os === ClientOSAndroid && platform.pwa?.installState() === 'prompt';
  const blocked = outcome === 'denied';
  if (!watched || dismissed || banner.showing || (!offerPush && !offerInstall && !blocked)) return null;

  const notNow = (): void => {
    prefs.getState().dismiss(CARD_DISMISSAL);
  };

  // Named by its question, not "Notifications": that is the name of the toasts' region (app/Toasts.tsx).
  return (
    <section className={styles.card} aria-labelledby={titleId}>
      <Bell className={styles.icon} aria-hidden="true" />
      <div className={styles.body}>
        {offerPush || blocked ? (
          <p id={titleId} className={styles.title}>
            {t('account.push.card.title')}
          </p>
        ) : (
          <>
            <p id={titleId} className={styles.title}>
              {t('account.push.card.installTitle')}
            </p>
            <p>{t('account.push.card.installBody')}</p>
          </>
        )}
        {blocked && <p role="alert">{t('account.push.denied')}</p>}
        {outcome === 'failed' && push.error !== null && <Notice>{pushErrorMessage(push.error, t)}</Notice>}
        <div className={styles.actions}>
          {offerPush && (
            <Button
              variant="primary"
              size="sm"
              icon={<Bell />}
              loading={push.busy}
              onClick={() => {
                void push.enable().then((result) => {
                  if (result === 'on') {
                    // A one-time card: once on, it isn't offered again for the dismissal's time either.
                    prefs.getState().dismiss(CARD_DISMISSAL);
                    ui.getState().toast({ kind: 'success', message: t('account.push.enabled') });
                    setOutcome(null);
                  } else {
                    setOutcome(result === 'denied' || result === 'failed' ? result : null);
                  }
                });
              }}
            >
              {t('account.push.card.turnOn')}
            </Button>
          )}
          {offerInstall && (
            <Button
              size="sm"
              icon={<Download />}
              onClick={() => {
                void platform.pwa?.promptInstall().then(() => {
                  setInstallTries((n) => n + 1);
                });
              }}
            >
              {t('account.push.card.install')}
            </Button>
          )}
          <Button size="sm" variant="ghost" onClick={notNow} disabled={push.busy}>
            {t('account.push.card.notNow')}
          </Button>
        </div>
      </div>
    </section>
  );
}
