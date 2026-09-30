// The in-memory log (05 §10.7): the last 500 lines, for the route error boundary and, later (M5), "Copy
// diagnostics". Lines never hold SDP bodies, ICE candidates, tokens, passwords or IP addresses (05 §20): attributes
// with such names are replaced, and IP addresses in text are masked.

export type LogLevel = 'debug' | 'info' | 'warn' | 'error';

/** Structured values of a line. Errors are stored as {name, message}. */
export type LogAttrs = Readonly<Record<string, unknown>>;

export interface LogLine {
  /** Date.now() when it was logged. */
  readonly at: number;
  readonly level: LogLevel;
  /** Who logged it: "boot", "rest", "room", "sub-pc", … */
  readonly component: string;
  readonly msg: string;
  readonly attrs?: LogAttrs;
}

export interface Logger {
  debug(msg: string, attrs?: LogAttrs): void;
  info(msg: string, attrs?: LogAttrs): void;
  warn(msg: string, attrs?: LogAttrs): void;
  error(msg: string, attrs?: LogAttrs): void;
  /** A logger for a sub-component: "room" → "room/share". */
  child(component: string): Logger;
}

export const MAX_LOG_LINES = 500;

const LEVELS: Readonly<Record<LogLevel, number>> = { debug: 10, info: 20, warn: 30, error: 40 };

/** Attribute names whose values are never kept (compared case-insensitively, as substrings). */
const SECRET_NAMES = ['sdp', 'candidate', 'token', 'password', 'secret', 'cookie', 'auth', 'endpoint'];

const REDACTED = '[redacted]';
const IPV4 = /\b(?:\d{1,3}\.){3}\d{1,3}\b/g;
/**
 * A token with at least two colons (word characters, dots, colons and a %zone): an IPv6 candidate, checked by
 * isIPv6. Whole tokens, so "std::vector" isn't cut apart (no lookbehind: Safari 15 can't parse it).
 */
const IPV6_CANDIDATE = /[\w.%]*:[\w.%]*:[\w.:%]*/g;
const HEX_GROUP = /^[0-9a-f]{0,4}$/i;

/** "fe80::1", "::1", "2001:db8:0:0:0:0:0:1"; not "12:30:45" (no "::" and fewer than 7 colons). */
function isIPv6(s: string): boolean {
  const groups = s.split(':');
  if (!groups.every((g) => HEX_GROUP.test(g))) return false;
  if (s.includes('::')) return groups.length <= 9;
  return groups.length === 8 && groups.every((g) => g !== '');
}

const lines: LogLine[] = [];
let consoleLevel: LogLevel | 'off' = defaultConsoleLevel();

function defaultConsoleLevel(): LogLevel | 'off' {
  if (import.meta.env.MODE === 'test') return 'off';
  return import.meta.env.DEV ? 'debug' : 'warn';
}

/** Masks IPv4 and IPv6 addresses in free text. */
export function maskAddresses(text: string): string {
  return text.replace(IPV4, '[ip]').replace(IPV6_CANDIDATE, (token) => {
    const zone = token.indexOf('%');
    const addr = zone < 0 ? token : token.slice(0, zone);
    return isIPv6(addr) ? '[ip]' + (zone < 0 ? '' : token.slice(zone)) : token;
  });
}

function isSecretName(name: string): boolean {
  const lower = name.toLowerCase();
  return SECRET_NAMES.some((s) => lower.includes(s));
}

function sanitize(value: unknown, depth: number): unknown {
  if (typeof value === 'string') return maskAddresses(value);
  if (value instanceof Error) return { name: value.name, message: maskAddresses(value.message) };
  if (value === null || typeof value !== 'object') return value;
  if (depth >= 3) return '[…]';
  if (Array.isArray(value)) return value.slice(0, 20).map((v) => sanitize(v, depth + 1));
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(value)) {
    out[k] = isSecretName(k) ? REDACTED : sanitize(v, depth + 1);
  }
  return out;
}

/** The attributes as they are stored: secrets replaced, addresses masked, values below 3 levels of nesting cut. */
export function sanitizeAttrs(attrs: LogAttrs): LogAttrs {
  return sanitize(attrs, 0) as LogAttrs;
}

function write(level: LogLevel, component: string, msg: string, attrs?: LogAttrs): void {
  const line: LogLine = {
    at: Date.now(),
    level,
    component,
    msg: maskAddresses(msg),
    ...(attrs ? { attrs: sanitizeAttrs(attrs) } : {}),
  };
  lines.push(line);
  if (lines.length > MAX_LOG_LINES) lines.splice(0, lines.length - MAX_LOG_LINES);
  if (consoleLevel !== 'off' && LEVELS[level] >= LEVELS[consoleLevel]) {
    const fn = level === 'error' ? console.error : level === 'warn' ? console.warn : console.log;
    if (line.attrs) fn(`[${component}] ${line.msg}`, line.attrs);
    else fn(`[${component}] ${line.msg}`);
  }
}

export function createLogger(component: string): Logger {
  return {
    debug: (msg, attrs) => {
      write('debug', component, msg, attrs);
    },
    info: (msg, attrs) => {
      write('info', component, msg, attrs);
    },
    warn: (msg, attrs) => {
      write('warn', component, msg, attrs);
    },
    error: (msg, attrs) => {
      write('error', component, msg, attrs);
    },
    child: (sub) => createLogger(`${component}/${sub}`),
  };
}

/** A copy of the kept lines, oldest first. */
export function logLines(): readonly LogLine[] {
  return lines.slice();
}

/** Empties the log (tests; later, "Copy diagnostics" doesn't need it). */
export function clearLog(): void {
  lines.length = 0;
}

/** Which lines also go to the console: 'off' in tests, 'debug' in dev builds, 'warn' in production. */
export function setConsoleLevel(level: LogLevel | 'off'): void {
  consoleLevel = level;
}
