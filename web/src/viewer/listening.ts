// "Now listening to bo" (05 §16.6): when the sound moves to another share, the Announcer's polite live region says
// whose it is now, for screen readers; the page shows it with the speaker buttons. No React here (05 §3).
import type { TFunction } from 'i18next';

import { i18n } from '../i18n';
import { sharerName } from './shareView';
import type { ViewerStore } from './viewerStore';

/**
 * Announces each move of the sound (viewerStore.audibleShareId) to a share that was there before: a pick, a
 * speaker button, the stage's "Listen here", the end of the share that was heard. The sound of a new share needs no
 * words: its room.event ("bo started sharing") is announced at the same moment, and a second message would take its
 * place in the region. That covers a re-published share too, which takes the sound of the one it replaces under a
 * new id (01 §10.6). Returns the function that stops it.
 */
export function attachListeningAnnouncer(
  store: ViewerStore,
  announce: (text: string) => void,
  t: TFunction = i18n.t,
): () => void {
  return store.subscribe((s, prev) => {
    const id = s.audibleShareId;
    if (id === null || id === prev.audibleShareId || !prev.shares.some((x) => x.id === id)) return;
    const share = s.shares.find((x) => x.id === id);
    if (share === undefined) return;
    announce(
      share.own ? t('viewer.announce.listeningOwn') : t('viewer.announce.listening', { name: sharerName(t, share) }),
    );
  });
}
