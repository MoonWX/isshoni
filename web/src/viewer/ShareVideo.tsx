// The one <video> of a tile or of the stage (05 §10.2): `autoplay playsinline muted disableRemotePlayback`, with a
// srcObject that holds only that share's video. Videos are ALWAYS muted, so they autoplay everywhere, iOS included;
// every share's sound comes from the single <audio> element of audioOut.ts.
import { useEffect, useMemo, useRef } from 'react';

import { cx } from '../ui/cx';
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
  // One MediaStream per track: a new track (the SFU reused a transceiver, or the sub PC was rebuilt) is a new source.
  const source = useMemo(() => stream ?? (track ? new MediaStream([track]) : null), [stream, track]);

  useEffect(() => {
    const el = ref.current;
    if (el && el.srcObject !== source) el.srcObject = source;
  }, [source]);

  // Let go of the stream when the tile unmounts (the share ended, or the room page was left).
  useEffect(() => {
    const el = ref.current;
    return () => {
      if (el) el.srcObject = null;
    };
  }, []);

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
