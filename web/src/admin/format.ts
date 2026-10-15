// Numbers, sizes, rates, times and lists for the dashboard and the Doctor page, in the UI language (05 §16.5: Intl
// with i18n.language). Sizes and rates are in units of 10³, as providers bill them.
import { formatDateTime, parseWireTime } from '../lib/time';

export class Format {
  constructor(readonly lang: string) {}

  /** "a, b, and c". */
  list(items: readonly string[]): string {
    return new Intl.ListFormat(this.lang, { style: 'long', type: 'conjunction' }).format(items);
  }

  /** A number with exactly `digits` decimals (none by default), grouped: "1,834.2". */
  number(n: number, digits = 0): string {
    return new Intl.NumberFormat(this.lang, { maximumFractionDigits: digits, minimumFractionDigits: digits }).format(n);
  }

  private unit(unit: string, value: number, digits: number): string {
    return new Intl.NumberFormat(this.lang, {
      style: 'unit',
      unit,
      unitDisplay: 'short',
      maximumFractionDigits: digits,
      minimumFractionDigits: digits,
    }).format(value);
  }

  /** A byte count in GB or MB of 10⁹ and 10⁶ bytes: "412.0 GB", "187 MB". */
  size(bytes: number): string {
    if (Math.abs(bytes) >= 1e9) return this.unit('gigabyte', bytes / 1e9, 1);
    return this.unit('megabyte', bytes / 1e6, 0);
  }

  /** A whole number of gigabytes (a limit an admin set): "1,000 GB". */
  gigabytes(gb: number): string {
    return this.unit('gigabyte', gb, 0);
  }

  /** A rate in bits per second: "41.2 Mb/s", "290 kb/s". */
  bitrate(bps: number): string {
    if (Math.abs(bps) >= 1e6) return this.unit('megabit-per-second', bps / 1e6, 1);
    return this.unit('kilobit-per-second', bps / 1e3, 0);
  }

  /** "84%" for 84. */
  percent(pct: number, digits = 0): string {
    return new Intl.NumberFormat(this.lang, {
      style: 'percent',
      maximumFractionDigits: digits,
      minimumFractionDigits: digits,
    }).format(pct / 100);
  }

  /** A span of time in one unit: "45 seconds", "7 minutes", "3 days". */
  span(value: number, unit: 'second' | 'minute' | 'hour' | 'day'): string {
    return new Intl.NumberFormat(this.lang, { style: 'unit', unit, unitDisplay: 'long' }).format(value);
  }

  /** A wire timestamp to the minute; the text itself when it isn't one. */
  time(wire: string): string {
    const d = parseWireTime(wire);
    return d ? formatDateTime(d, this.lang) : wire;
  }

  /** A wire timestamp's day (in UTC, as the server's dates are meant). */
  date(wire: string): string {
    const d = parseWireTime(wire);
    return d ? new Intl.DateTimeFormat(this.lang, { dateStyle: 'medium', timeZone: 'UTC' }).format(d) : wire;
  }

  /** "2026-09" as "September 2026". */
  month(month: string): string {
    const m = /^(\d{4})-(\d{2})$/.exec(month);
    if (!m) return month;
    const at = Date.UTC(Number(m[1]), Number(m[2]) - 1, 1);
    return new Intl.DateTimeFormat(this.lang, { year: 'numeric', month: 'long', timeZone: 'UTC' }).format(at);
  }
}
