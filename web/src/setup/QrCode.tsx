// A QR code as React SVG (05 §14.1, §20): uqr gives the module matrix, and the dark modules become one <path>. No
// raw HTML and no image: nothing here goes past React's escaping, and the code scales sharply at any size.
//
// The code stays dark on light in both themes and in forced-colors mode: phone cameras read dark modules on a light
// background, and some readers fail on an inverted code. Around it is the quiet zone that the QR standard asks for
// (4 modules), part of the light square itself, so the page's background color never touches the code.
import { useMemo } from 'react';
import { encode } from 'uqr';

import { cx } from '../ui/cx';
import styles from './QrCode.module.css';

/** The light margin around the code, in modules (ISO/IEC 18004 asks for 4). */
export const QR_QUIET_ZONE = 4;

/**
 * The dark modules of a QR matrix (`matrix[y][x]`, true for dark) as SVG path data in module units: one
 * `M<x> <y>h<n>v1h-<n>z` rectangle per horizontal run of dark modules.
 */
export function qrPathData(matrix: readonly (readonly boolean[])[]): string {
  const parts: string[] = [];
  matrix.forEach((row, y) => {
    let x = 0;
    while (x < row.length) {
      if (!row[x]) {
        x++;
        continue;
      }
      const start = x;
      while (x < row.length && row[x]) x++;
      const n = x - start;
      parts.push(`M${String(start)} ${String(y)}h${String(n)}v1h-${String(n)}z`);
    }
  });
  return parts.join('');
}

export interface QrCodeProps {
  /** What the code holds: the invite link. */
  value: string;
  /** The image's accessible name (translated): "QR code of the invite link". */
  label: string;
  /** The rendered width and height in CSS pixels, quiet zone included. Default 200. */
  size?: number;
  className?: string;
}

/**
 * Error correction M (15%): an invite link of about 70 characters becomes a version 5 code (37 modules), coarse
 * enough to scan from a screen across a room, and it survives a little glare.
 */
export function QrCode({ value, label, size = 200, className }: QrCodeProps) {
  const qr = useMemo(() => encode(value, { ecc: 'M', border: 0 }), [value]);
  const path = useMemo(() => qrPathData(qr.data), [qr]);
  const span = qr.size + 2 * QR_QUIET_ZONE;
  const origin = String(-QR_QUIET_ZONE);
  return (
    <svg
      className={cx(styles.qr, className)}
      role="img"
      aria-label={label}
      width={size}
      height={size}
      viewBox={`${origin} ${origin} ${String(span)} ${String(span)}`}
      shapeRendering="crispEdges"
    >
      <rect className={styles.light} x={origin} y={origin} width={span} height={span} />
      <path className={styles.dark} d={path} />
    </svg>
  );
}
