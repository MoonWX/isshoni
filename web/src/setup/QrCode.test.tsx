// QrCode (05 §14.1, §20): uqr's matrix drawn as one SVG path, with the quiet zone around it.
import { render, screen } from '@testing-library/react';
import { encode } from 'uqr';
import { describe, expect, it } from 'vitest';

import { QR_QUIET_ZONE, QrCode, qrPathData } from './QrCode';

const LINK = 'https://watch.example.com/invite#EXAMPLEinviteTOKEN0123456789abcd';

/** The cells a path from qrPathData() paints, as "x,y". */
function paintedCells(d: string): Set<string> {
  const cells = new Set<string>();
  const runs = d.match(/M\d+ \d+h\d+v1h-\d+z/g) ?? [];
  // Nothing else in the path: every command belongs to a run.
  expect(runs.join('')).toBe(d);
  for (const run of runs) {
    const [x, y, n] = (/M(\d+) (\d+)h(\d+)/.exec(run) ?? []).slice(1).map(Number);
    if (x === undefined || y === undefined || n === undefined) throw new Error(`bad run ${run}`);
    for (let i = 0; i < n; i++) cells.add(`${String(x + i)},${String(y)}`);
  }
  return cells;
}

function darkCells(matrix: readonly (readonly boolean[])[]): Set<string> {
  const cells = new Set<string>();
  matrix.forEach((row, y) => {
    row.forEach((dark, x) => {
      if (dark) cells.add(`${String(x)},${String(y)}`);
    });
  });
  return cells;
}

describe('qrPathData', () => {
  it('draws one rectangle per run of dark modules, row by row', () => {
    const matrix = [
      [true, true, false, true],
      [false, false, false, false],
      [false, true, true, true],
    ];
    expect(qrPathData(matrix)).toBe('M0 0h2v1h-2zM3 0h1v1h-1zM1 2h3v1h-3z');
  });

  it('draws nothing for an empty matrix', () => {
    expect(qrPathData([])).toBe('');
    expect(qrPathData([[false, false]])).toBe('');
  });
});

describe('QrCode', () => {
  it('paints exactly the dark modules of the link’s code, inside a light square with the quiet zone', () => {
    const { container } = render(<QrCode value={LINK} label="QR code of the invite link" />);
    const svg = screen.getByRole('img', { name: 'QR code of the invite link' });
    const qr = encode(LINK, { ecc: 'M', border: 0 });
    // An invite link of this length is a small code, quick to scan.
    expect(qr.size).toBeLessThanOrEqual(37);

    const path = container.querySelector('path');
    expect(paintedCells(path?.getAttribute('d') ?? '')).toEqual(darkCells(qr.data));

    const span = qr.size + 2 * QR_QUIET_ZONE;
    expect(svg).toHaveAttribute('viewBox', `-4 -4 ${String(span)} ${String(span)}`);
    const light = container.querySelector('rect');
    expect(light).toHaveAttribute('x', '-4');
    expect(light).toHaveAttribute('y', '-4');
    expect(light).toHaveAttribute('width', String(span));
    expect(light).toHaveAttribute('height', String(span));
    // Drawn before the modules, so it is under them.
    expect(light?.nextElementSibling).toBe(path);
    expect(svg).toHaveAttribute('width', '200');
  });

  it('takes its size and draws a new code for a new value', () => {
    const { container, rerender } = render(<QrCode value={LINK} label="QR" size={120} />);
    expect(screen.getByRole('img', { name: 'QR' })).toHaveAttribute('height', '120');
    const before = container.querySelector('path')?.getAttribute('d');
    rerender(<QrCode value={`${LINK}x`} label="QR" size={120} />);
    const after = container.querySelector('path')?.getAttribute('d') ?? '';
    expect(after).not.toBe(before);
    expect(paintedCells(after)).toEqual(darkCells(encode(`${LINK}x`, { ecc: 'M', border: 0 }).data));
  });
});
