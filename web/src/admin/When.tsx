import { useTranslation } from 'react-i18next';

import { formatDateTime, formatRelativeTime, parseWireTime } from '../lib/time';

export interface WhenProps {
  /** A wire timestamp (03 §3.3); absent or unreadable shows `fallback`. */
  at: string | undefined;
  /**
   * absolute: "Oct 1, 2026, 2:00 PM"; date: "Oct 1, 2026" and relative: "3 minutes ago", both with the full date and
   * time as a tooltip.
   */
  mode?: 'absolute' | 'date' | 'relative';
  /** What to show without a time (translated): "Never". */
  fallback?: string;
}

/** A point in time as a <time> element, formatted with Intl in the UI language (05 §16.5). */
export function When({ at, mode = 'absolute', fallback = '' }: WhenProps) {
  const { i18n } = useTranslation();
  const date = parseWireTime(at);
  if (date === undefined) return fallback;
  const absolute = formatDateTime(date, i18n.language);
  if (mode === 'absolute') return <time dateTime={date.toISOString()}>{absolute}</time>;
  return (
    <time dateTime={date.toISOString()} title={absolute}>
      {mode === 'relative'
        ? formatRelativeTime(date, i18n.language)
        : new Intl.DateTimeFormat(i18n.language, { dateStyle: 'medium' }).format(date)}
    </time>
  );
}
