// The debug overlay, "Stats for nerds" (05 §10.7): what this page's stats collector measures, per tile, and the
// server's view of this connection (01's stats.watch). Shift+D switches it (prefsStore.debug, kept on the device),
// and so does `?debug=1` in the URL, which turns it on. ViewerLayout mounts it inside the layout, so it shows over a
// fullscreen stage too.
//
// It is a view of viewer.debug (debugStats.ts): while it is mounted it says it is open, and attachViewerStats
// (statsLink.ts), run by the owner of the collector, makes the collector sample every second and fills in the
// samples and the server's stats. Per tile: the received video (size, fps, rate, codec), the decoder, the frames,
// the network as the video saw it, the sound, and what the server forwards of it. Above them the page's PCs, and
// below them what this page sends and what the server receives of it.
//
// No IP addresses: a sample keeps only the selected pair's candidate type and transport, and ServerStats has none.
import { X } from 'lucide-react';
import { useEffect, useId, useMemo, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';

import type { PrefsStore } from '../app/prefs';
import type { StatsSample } from '../lib/stats/summarize';
import { TrackKindAudio, TrackKindVideo, type ServerStats } from '../protocol/types.gen';
import { cx } from '../ui/cx';
import styles from './DebugOverlay.module.css';
import {
  audioLine,
  debugNumbers,
  decoderLine,
  downlinkLine,
  framesLine,
  outboundLine,
  outboundName,
  pcLine,
  pcName,
  serverLayerLine,
  serverSubLine,
  videoLine,
  videoNetworkLine,
  type DebugNumbers,
} from './debugText';
import type { ViewerServices } from './services';
import { shareTitle } from './shareView';
import type { ViewerShare } from './viewerStore';

/** The URL parameter that turns the overlay on (05 §10.7). */
export const DEBUG_PARAM = 'debug';

export interface DebugOverlayProps {
  /** The viewer: its tiles, and the overlay's state (viewer.debug). */
  viewer: Pick<ViewerServices, 'store' | 'debug'>;
  /** The device's preferences: `debug` says whether the overlay is on. */
  prefs: PrefsStore;
  className?: string | undefined;
}

/** The overlay while prefsStore.debug is on; `?debug=1` turns that on when the layout mounts. */
export function DebugOverlay({ viewer, prefs, className }: DebugOverlayProps) {
  const on = useStore(prefs, (s) => s.debug);
  useEffect(() => {
    if (new URLSearchParams(globalThis.location.search).get(DEBUG_PARAM) === '1') prefs.getState().setDebug(true);
  }, [prefs]);
  if (!on) return null;
  return (
    <DebugPanel
      viewer={viewer}
      className={className}
      onClose={() => {
        prefs.getState().setDebug(false);
      }}
    />
  );
}

export interface DebugPanelProps {
  viewer: Pick<ViewerServices, 'store' | 'debug'>;
  onClose: () => void;
  className?: string | undefined;
}

/** The overlay itself. While it is mounted, viewer.debug says it is open. */
export function DebugPanel({ viewer, onClose, className }: DebugPanelProps) {
  const { t, i18n } = useTranslation();
  const titleId = useId();
  const { debug, store } = viewer;
  const sample = useStore(debug, (s) => s.sample);
  const server = useStore(debug, (s) => s.server);
  const shares = useStore(store, (s) => s.shares);
  const focusedId = useStore(store, (s) => s.focusedShareId);
  const n = useMemo(() => debugNumbers(i18n.language), [i18n.language]);

  useEffect(() => {
    debug.getState().setOpen(true);
    return () => {
      debug.getState().setOpen(false);
    };
  }, [debug]);

  // The tiles as the layout shows them, the stage first. This page's own preview is under "Sending".
  const tiles = [...shares.filter((s) => s.id === focusedId), ...shares.filter((s) => s.id !== focusedId)].filter(
    (s) => !s.local,
  );

  return (
    <aside className={cx(styles.overlay, className)} aria-labelledby={titleId} data-debug-overlay="">
      <div className={styles.header}>
        <h2 id={titleId} className={styles.title}>
          {t('viewer.debug.title')}
        </h2>
        <button
          type="button"
          className={styles.close}
          aria-label={t('viewer.debug.close')}
          title={t('viewer.debug.close')}
          onClick={onClose}
        >
          <X aria-hidden="true" />
        </button>
      </div>
      {sample === null ? (
        <p className={styles.note}>{t('viewer.debug.waiting')}</p>
      ) : (
        <>
          <Connections sample={sample} server={server} n={n} />
          {tiles.map((share) => (
            <TileStats key={share.id} share={share} sample={sample} server={server} n={n} />
          ))}
          <Sending sample={sample} server={server} n={n} />
        </>
      )}
    </aside>
  );
}

function Section({ title, children, shareId }: { title: string; children: ReactNode; shareId?: string }) {
  const id = useId();
  return (
    <section className={styles.section} aria-labelledby={id} data-debug-share={shareId}>
      <h3 id={id} className={styles.heading}>
        {title}
      </h3>
      {children}
    </section>
  );
}

/** One label and its value; null and '' values are left out. */
function Row({ label, value }: { label: string; value: string | null }) {
  if (value === null || value === '') return null;
  return (
    <div className={styles.row}>
      <dt>{label}</dt>
      <dd>{value}</dd>
    </div>
  );
}

interface PartProps {
  sample: StatsSample;
  server: ServerStats | null;
  n: DebugNumbers;
}

/** The page's PCs, and the server's estimate of this connection's downlink. */
function Connections({ sample, server, n }: PartProps) {
  const { t } = useTranslation();
  return (
    <Section title={t('viewer.debug.connections')}>
      {sample.pcs.length === 0 ? (
        <p className={styles.note}>{t('viewer.debug.noPc')}</p>
      ) : (
        <dl className={styles.rows}>
          {sample.pcs.map((pc) => (
            <Row key={`${pc.pc} ${String(pc.gen)}`} label={pcName(t, pc)} value={pcLine(t, n, pc)} />
          ))}
          {server?.downlinkEstimate !== undefined && (
            <Row label={t('viewer.debug.downlink')} value={downlinkLine(t, n, server.downlinkEstimate)} />
          )}
        </dl>
      )}
    </Section>
  );
}

/** One tile: what this page receives of the share, and what the server forwards of it. */
function TileStats({ share, sample, server, n }: PartProps & { share: ViewerShare }) {
  const { t } = useTranslation();
  const received = sample.shares[share.id];
  const video = received?.video;
  const audio = received?.audio;
  const forwarded = server?.subs.filter((s) => s.shareId === share.id) ?? [];
  const forwardedVideo = forwarded.find((s) => s.kind === TrackKindVideo);
  const forwardedAudio = forwarded.find((s) => s.kind === TrackKindAudio);
  const nothing = video === undefined && audio === undefined && forwarded.length === 0;
  return (
    <Section title={shareTitle(t, share)} shareId={share.id}>
      {nothing ? (
        <p className={styles.note}>{t('viewer.debug.noTileStats')}</p>
      ) : (
        <dl className={styles.rows}>
          {video && (
            <>
              <Row label={t('viewer.debug.label.video')} value={videoLine(t, n, video)} />
              <Row label={t('viewer.debug.label.decoder')} value={decoderLine(t, video)} />
              <Row label={t('viewer.debug.label.frames')} value={framesLine(t, n, video)} />
              <Row label={t('viewer.debug.label.network')} value={videoNetworkLine(t, n, video)} />
            </>
          )}
          {audio && <Row label={t('viewer.debug.label.audio')} value={audioLine(t, n, audio)} />}
          {forwardedVideo && (
            <Row label={t('viewer.debug.label.serverVideo')} value={serverSubLine(t, n, forwardedVideo)} />
          )}
          {forwardedAudio && (
            <Row label={t('viewer.debug.label.serverAudio')} value={serverSubLine(t, n, forwardedAudio)} />
          )}
        </dl>
      )}
    </Section>
  );
}

/** What this page sends (its share's layers and sound), and what the server receives of it. */
function Sending({ sample, server, n }: PartProps) {
  const { t } = useTranslation();
  const layers = server?.layers ?? [];
  if (sample.outbound.length === 0 && layers.length === 0) return null;
  return (
    <Section title={t('viewer.debug.sending')}>
      <dl className={styles.rows}>
        {sample.outbound.map((o) => (
          <Row key={`${o.shareId} ${o.kind} ${o.rid ?? ''}`} label={outboundName(t, o)} value={outboundLine(t, n, o)} />
        ))}
        {layers.map((l) => (
          <Row
            key={`server ${l.shareId} ${l.kind} ${l.rid ?? ''}`}
            label={t('viewer.debug.serverReceives', { name: outboundName(t, l) })}
            value={serverLayerLine(t, n, l)}
          />
        ))}
      </dl>
    </Section>
  );
}
