// The one <video> of a tile or of the stage (05 §10.2): `autoplay playsinline muted disableRemotePlayback`, with a
// srcObject that holds only that share's video. Videos are ALWAYS muted, so they autoplay everywhere, iOS included;
// every share's sound comes from the single <audio> element of audioOut.ts.
//
// Autoplay can still be refused: iOS Low Power Mode blocks even muted video (05 §10.3). Inside a ViewerLayout the
// element is therefore also started through the viewer's VideoPlayback, which notices a refusal, so that TapToStart
// can ask for the tap that starts it. Outside one, the `autoplay` attribute is all there is.
import { useContext, useEffect, useMemo, useRef } from 'react';

import { cx } from '../ui/cx';
import { ViewerContext } from './context';
import styles from './ShareVideo.module.css';

export interface ShareVideoProps {
  /** A received video track (a share of someone else) … */
  track?: MediaStreamTrack | undefined;
  /** … or a whole stream: the local preview of a share this page publishes. It wins over track. */
  stream?: MediaStream | null | undefined;
  className?: string | undefined;
}

export function ShareVideo({ track, stream, className }: ShareVideoProps) {
  const ref = useRef<HTMLVideoElement>(null);
  const videos = useContext(ViewerContext)?.videos;
  // One MediaStream per track: a new track (the SFU reused a transceiver, or the sub PC was rebuilt) is a new source.
  const source = useMemo(() => stream ?? (track ? new MediaStream([track]) : null), [stream, track]);

  useEffect(() => {
    const el = ref.current;
    if (!el || el.srcObject === source) return;
    el.srcObject = source;
    if (source) videos?.play(el);
    else videos?.forget(el);
  }, [source, videos]);

  // Let go of the stream when the tile unmounts (the share ended, or the room page was left).
  useEffect(() => {
    const el = ref.current;
    return () => {
      if (!el) return;
      el.srcObject = null;
      videos?.forget(el);
    };
  }, [videos]);

  return (
    <video
      ref={ref}
      className={cx(styles.video, className)}
      autoPlay
      playsInline
      muted
      disableRemotePlayback
      // Not a tab stop: the tile's button and the stage's toolbar are the controls.
      tabIndex={-1}
    />
  );
}
