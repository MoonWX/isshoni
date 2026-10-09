// The e2e fixtures (05 §19.3). Specs import `test` and `expect` from here, not from @playwright/test.
//
// Servers. Every e2e server is a real `isshoni serve` process of the binary under test (ISSHONI_BIN, found by
// global-setup.ts), started by a fixture:
// - `server` (worker-scoped): one set-up server per Playwright worker, shared by that worker's specs;
// - `startServer({ overrides?, setUp? })` (test-scoped): a server of the test's own, stopped when the test ends. A
//   spec that needs a server that isn't set up, another config, a restart, or a change to a server-wide setting
//   starts its own, so no spec depends on what another spec did to a server.
//
// Each server gets deploy/dev/isshoni.e2e.toml as its base config, free ports chosen here, a fresh temporary data
// directory, its own admin socket, and a log file in the Playwright output directory
// (web/test-results/server-<worker>-<n>.log). No port is fixed, so parallel workers, specs with their own server
// and a running `task dev` never meet.
//
// People. `server.createUser()` makes an account over REST, `server.signIn(context, account)` signs a browser
// context in, and `asUser(server)` opens one more browser context for one more person. Each of these requests comes
// from an address of its own (see fromNewAddress), as each friend's would.
//
// Pages. Every context of the harness has the debug flag set, so the page installs window.__isshoni (05 §10.7),
// which stats.ts reads. `fakeDisplay` and `openToneTab` give a page something to share.
import { execFile, spawn, type ChildProcess } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import dgram from 'node:dgram';
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { promisify } from 'node:util';

import {
  test as base,
  request,
  type APIRequestContext,
  type APIResponse,
  type BrowserContext,
  type Page,
  type Video,
} from '@playwright/test';

import { REPO_ROOT, resolveBinary } from './global-setup.ts';

export { expect } from '@playwright/test';

const execFileAsync = promisify(execFile);

// ---- Chrome ----

/** The title of the tone tab (tone.html), which Chrome's tab picker selects by itself (05 §19.3). */
export const TONE_TAB_TITLE = 'isshoni-e2e-tone';

/**
 * The Chrome flags of the e2e runs (05 §19.3):
 * - the tab picker of getDisplayMedia selects the tone tab without a click;
 * - loopback ICE candidates are allowed: the browser and the server share 127.0.0.1;
 * - audio plays without a user gesture, unless `autoplay` is false (the spec of "Tap to unmute" uses
 *   `test.use({ launchOptions: { args: chromeArgs({ autoplay: false }) } })`).
 *
 * Not --use-fake-ui-for-media-stream: with it Chrome answers getDisplayMedia without a picker and never with the
 * tone tab, so the real Share button fails (05 §19.3).
 */
export function chromeArgs(options: { autoplay?: boolean } = {}): string[] {
  const args = [`--auto-select-tab-capture-source-by-title=${TONE_TAB_TITLE}`, '--allow-loopback-in-peer-connection'];
  if (options.autoplay ?? true) args.push('--autoplay-policy=no-user-gesture-required');
  return args;
}

// ---- Types ----

/** A config value as a flag carries it. Lists are comma-separated strings. */
export type ConfigValue = string | number | boolean;

export interface StartServerOptions {
  /**
   * Config keys (04 §4.3) and their values, passed as flags by 04 §4.2's naming rule: `{ 'listen.ice_udp': '' }`
   * becomes `--listen.ice-udp=` (a flag can set an empty string; the environment can't). The fixture owns
   * `public_url`, `data_dir`, `listen.http` and `listen.admin_socket`.
   */
  overrides?: Readonly<Record<string, ConfigValue>>;
  /**
   * Complete the first-run setup with a generated admin (the default). With false the server waits for the setup
   * wizard, and `admin` is null.
   */
  setUp?: boolean;
}

/** An account the harness created, with the password it chose. */
export interface Account {
  readonly id: string;
  readonly username: string;
  readonly password: string;
}

export interface InviteOptions {
  note?: string;
  /** 1–1000; the server's default when absent. */
  maxUses?: number;
  /** 1–720; the server's default when absent. */
  expiresInHours?: number;
}

/** An invite as the server returns it once, at creation (03 §7.9). */
export interface InviteLink {
  readonly id: string;
  /** `<url>/invite#<token>` */
  readonly url: string;
  readonly token: string;
}

export interface E2EServer {
  /** The public URL, `http://127.0.0.1:<port>`, without a trailing slash. */
  readonly url: string;
  /** The admin that the setup created; null on a server started with `setUp: false`. */
  readonly admin: Account | null;
  /** The file with the server's stdout and stderr (JSON lines). */
  readonly logPath: string;
  /**
   * Mints the one-time setup link (`isshoni setup-url --json`): `<url>/setup#<token>`. It works only while the
   * server has no admin; each call cancels the link before it.
   */
  setupUrl(): Promise<string>;
  /** Creates an invite as the admin (POST /api/v1/invites). */
  createInvite(options?: InviteOptions): Promise<InviteLink>;
  /**
   * Creates an active member: the admin's one-use invite, then the sign-up, both over REST. `username` must be
   * new on this server; the default is `uniqueName('friend')`.
   */
  createUser(username?: string): Promise<Account>;
  /**
   * Logs `account` in over REST with the context's cookie jar, so the context's pages are signed in. Use one
   * browser context per server: a cookie ignores the port, so the session of a second e2e server would replace
   * the first one's.
   */
  signIn(context: BrowserContext, account: Pick<Account, 'username' | 'password'>): Promise<void>;
  /** Stops the process (SIGTERM, waits for the exit) and starts it again with the same data, ports and flags. */
  restart(): Promise<void>;
  /** Stops the process and removes its data directory. A server is stopped anyway when its test or worker ends. */
  stop(): Promise<void>;
}

/** One more person in a test: a browser context of their own, signed in, with a page that has not navigated yet. */
export interface UserSession {
  readonly account: Account;
  readonly context: BrowserContext;
  readonly page: Page;
}

export interface TestFixtures {
  /** Starts a server of the test's own; see StartServerOptions. It is stopped when the test ends. */
  startServer: (options?: StartServerOptions) => Promise<E2EServer>;
  /**
   * Opens a new browser context signed in as `account` (default: a new member, `server.createUser()`), with the
   * debug flag set. Its video is kept like the default context's; it is closed when the test ends.
   */
  asUser: (server: E2EServer, account?: Account) => Promise<UserSession>;
  /** Attaches the logs of the servers a failed test could have used to its report. */
  serverLogs: undefined;
}

export interface WorkerFixtures {
  /** This worker's server: set up, with an admin. Don't restart it or change its server-wide settings. */
  server: E2EServer;
}

// ---- Names, passwords, addresses ----

/** A username no other call returned: `<label>-<6 hex digits>`. */
export function uniqueName(label: string): string {
  return `${label}-${randomBytes(3).toString('hex')}`;
}

function newPassword(): string {
  return randomBytes(18).toString('base64url');
}

let addresses = 0;

/**
 * An X-Forwarded-For header with an address that no earlier call of this worker returned (the documentation
 * ranges of RFC 5737). An e2e server trusts the header from a loopback peer (off mode on a loopback listener,
 * 04 §8.5), so each account request of the harness has a client address of its own, as each friend has. Without
 * it, everything would come from 127.0.0.1 and share one `auth-ip` bucket (03 §7.3: 20 sign-ups and logins, then
 * one per 15 s), which the specs that share a worker's server would empty between them.
 */
function fromNewAddress(): Record<string, string> {
  const n = addresses++;
  const range = ['198.51.100', '203.0.113', '192.0.2'][Math.floor(n / 254) % 3] ?? '198.51.100';
  return { 'X-Forwarded-For': `${range}.${String((n % 254) + 1)}` };
}

// ---- Page helpers ----

/** platform.storage.session keys the page reads (lib/stats/debugHandle.ts, platform/browser/fakeDisplay.ts). */
const DEBUG_FLAG_KEY = 'isshoni.debug';
const FAKE_DISPLAY_KEY = 'isshoni.e2e.fakeDisplay';

/** Sets a sessionStorage key before any script of every page of `target` runs. */
async function setSessionKey(target: BrowserContext | Page, key: string, value: string): Promise<void> {
  await target.addInitScript(
    ([k, v]) => {
      try {
        sessionStorage.setItem(k, v);
      } catch {
        // about:blank and other pages without storage
      }
    },
    [key, value] as const,
  );
}

/**
 * Makes the pages of `target` install window.__isshoni (05 §10.7). The `context` fixture and `asUser` already do
 * this; a spec needs it only for a context it made with browser.newContext().
 */
export async function enableDebug(target: BrowserContext | Page): Promise<void> {
  await setSessionKey(target, DEBUG_FLAG_KEY, '1');
}

/** A value of the fake-display seam (05 §19.3): the surface a pick reports, with `+audio` for a 1 kHz tone. */
export type FakeDisplay = `${'browser' | 'window' | 'monitor'}${'' | '+audio'}`;

/**
 * Makes "Share" on the pages of `target` capture a canvas (and a tone) without a picker (05 §19.3). Headless runs
 * need it, since headless Chrome can't accept the picker, and so do whole-screen cases, which no flag can pick.
 * Call it before the page loads.
 */
export async function fakeDisplay(target: BrowserContext | Page, value: FakeDisplay): Promise<void> {
  await setSessionKey(target, FAKE_DISPLAY_KEY, value);
}

/**
 * Opens the tone tab in `context`: e2e/tone.html, served through context.route under the server's origin. With
 * chromeArgs() a headful Chrome picks this tab when a page of the same context clicks Share. The page sharing
 * twice brings itself to the front before the second pick (05 §19.3).
 */
export async function openToneTab(context: BrowserContext, server: Pick<E2EServer, 'url'>): Promise<Page> {
  const file = path.join(import.meta.dirname, 'tone.html');
  let body: string;
  try {
    body = await fsp.readFile(file, 'utf8');
  } catch (err) {
    throw new Error('web/e2e/tone.html does not exist yet: it comes with the first spec that shares (README S66)', {
      cause: err,
    });
  }
  // A dot in the last segment: no route of the app has one (05 §5).
  const url = `${server.url}/e2e/tone.html`;
  await context.route(url, (route) => route.fulfill({ contentType: 'text/html; charset=utf-8', body }));
  const page = await context.newPage();
  await page.goto(url);
  return page;
}

// ---- Server processes ----

const E2E_CONFIG = path.join(REPO_ROOT, 'deploy', 'dev', 'isshoni.e2e.toml');

/** Keys a spec can't override: the fixture derives the URL, the data directory and the socket from them. */
const FIXTURE_KEYS = ['public_url', 'data_dir', 'listen.http', 'listen.admin_socket'];

const READY_TIMEOUT_MS = 20_000;
/** The server stops within 10 s (04 §6.4); after this long it is killed. */
const STOP_TIMEOUT_MS = 15_000;
/** How the server reports a port that another process holds (04 §6.1 step 6). */
const PORT_IN_USE = /is in use|address already in use/i;

/** A config key as its flag (04 §4.2): the path with "_" → "-", always with "=", which also carries "". */
function configFlag(key: string, value: ConfigValue): string {
  return `--${key.replaceAll('_', '-')}=${String(value)}`;
}

/**
 * The environment of the e2e server and CLI processes: this process's without any ISSHONI_* variable (a config
 * key in the developer's shell, or CI's ISSHONI_VERSION, must not reach the server), plus the data directory.
 */
function serverEnv(dataDir: string): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [name, value] of Object.entries(process.env)) {
    if (!name.startsWith('ISSHONI_')) env[name] = value;
  }
  env['ISSHONI_DATA_DIR'] = dataDir;
  // The data is temporary on purpose. Where the tests themselves run in a container (a dev container, a job run
  // with act), the server would refuse a data directory that is not a mounted volume (04 §5.1).
  env['ISSHONI_ALLOW_EPHEMERAL_DATA'] = '1';
  return env;
}

interface Ports {
  readonly http: number;
  readonly iceUdp: number;
  readonly iceTcp: number;
}

/** Binds port 0, reads the port, closes: three ports that were free a moment ago, held together so they differ. */
async function freePorts(): Promise<Ports> {
  const tcp = (host?: string) =>
    new Promise<net.Server>((resolve, reject) => {
      const srv = net.createServer();
      srv.once('error', reject);
      srv.listen({ port: 0, host }, () => {
        resolve(srv);
      });
    });
  const udp = () =>
    new Promise<dgram.Socket>((resolve, reject) => {
      const sock = dgram.createSocket('udp4');
      sock.once('error', reject);
      sock.bind(0, () => {
        resolve(sock);
      });
    });
  const closed = (s: net.Server | dgram.Socket) =>
    new Promise<void>((resolve) => {
      s.close(() => {
        resolve();
      });
    });
  const http = await tcp('127.0.0.1');
  const iceTcp = await tcp();
  const iceUdp = await udp();
  const ports = {
    http: (http.address() as net.AddressInfo).port,
    iceTcp: (iceTcp.address() as net.AddressInfo).port,
    iceUdp: iceUdp.address().port,
  };
  await Promise.all([closed(http), closed(iceTcp), closed(iceUdp)]);
  return ports;
}

let sockets = 0;

/**
 * `<os tmpdir>/isshoni-e2e-<pid>-<n>.sock`. It is short on purpose: a socket path has at most 103 bytes (macOS),
 * so a long TMPDIR falls back to /tmp.
 */
function adminSocketPath(): string {
  const name = `isshoni-e2e-${String(process.pid)}-${String(++sockets)}.sock`;
  const inTmp = path.join(os.tmpdir(), name);
  return Buffer.byteLength(inTmp) <= 103 ? inTmp : path.join('/tmp', name);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function isReady(url: string): Promise<boolean> {
  try {
    const res = await fetch(`${url}/readyz`, { signal: AbortSignal.timeout(2_000) });
    await res.body?.cancel();
    return res.ok;
  } catch {
    return false;
  }
}

/** The JSON body of `res`, which must have `status`; else an error with the server's answer (codes, no secrets). */
async function jsonOf(res: APIResponse, status: number, what: string): Promise<unknown> {
  if (res.status() !== status) {
    throw new Error(`${what}: want ${String(status)}, got ${String(res.status())} ${await res.text()}`);
  }
  return res.json();
}

/**
 * POSTs one of the public account requests (setup, sign-up, login) from an address of its own and returns the JSON
 * body of the answer, which must have `status`. A 503 means that the server's budget of password hashes is spent
 * (03 §7.3 `auth-hash`: 20 at once, then 5 per second for the whole server), which a spec with many people can
 * do: the request waits as long as the answer says and goes again.
 */
async function postAccount(api: APIRequestContext, url: string, data: object, status: number): Promise<unknown> {
  for (let attempt = 1; ; attempt++) {
    const res = await api.post(url, { data, headers: fromNewAddress() });
    if (res.status() === 503 && attempt < 6) {
      const seconds = Number(res.headers()['retry-after']);
      await sleep(Math.min(Number.isFinite(seconds) && seconds > 0 ? seconds : 1, 5) * 1000);
      continue;
    }
    return jsonOf(res, status, `POST ${url}`);
  }
}

/** The server processes of this worker that run now: killed when the worker exits without its teardown. */
const running = new Set<ChildProcess>();
process.once('exit', () => {
  for (const child of running) child.kill('SIGKILL');
});

/** Every server this worker started, in order; serverLogs reads it. */
const started: Server[] = [];
let servers = 0;

interface Process {
  readonly child: ChildProcess;
  /** Settles when the process is gone, also when it never started. */
  readonly gone: Promise<void>;
}

function alive(child: ChildProcess): boolean {
  return child.pid !== undefined && child.exitCode === null && child.signalCode === null;
}

class Server implements E2EServer {
  url = '';
  admin: Account | null = null;

  private ports: Ports | null = null;
  private proc: Process | null = null;
  private api: APIRequestContext | null = null;
  private stopped = false;
  private readonly bin = resolveBinary();
  private readonly socket = adminSocketPath();

  constructor(
    private readonly dataDir: string,
    readonly logPath: string,
    private readonly overrides: Readonly<Record<string, ConfigValue>>,
  ) {}

  get isRunning(): boolean {
    return this.proc !== null && alive(this.proc.child);
  }

  /**
   * Starts the process and waits until it is ready. A first start picks the ports, and picks new ones once when
   * the server found one of them taken; a restart keeps the ports it has.
   */
  async start(): Promise<void> {
    const first = this.ports === null;
    for (let attempt = 1; ; attempt++) {
      this.ports ??= await freePorts();
      this.url = `http://127.0.0.1:${String(this.ports.http)}`;
      const logged = fs.existsSync(this.logPath) ? fs.statSync(this.logPath).size : 0;
      const outcome = await this.run(this.ports);
      if (outcome === 'ready') return;

      const log = (await fsp.readFile(this.logPath)).subarray(logged).toString('utf8');
      if (first && attempt === 1 && outcome === 'exited' && PORT_IN_USE.test(log)) {
        this.ports = null;
        continue;
      }
      const why =
        outcome === 'exited'
          ? 'exited before it was ready'
          : `was not ready within ${String(READY_TIMEOUT_MS / 1000)} s`;
      const tail = log.trimEnd().split('\n').slice(-20).join('\n');
      throw new Error(`the e2e server ${why}. The end of ${path.basename(this.logPath)}:\n${tail}`);
    }
  }

  private flags(ports: Ports): string[] {
    const values = new Map<string, ConfigValue>([
      ['public_url', this.url],
      ['listen.http', `127.0.0.1:${String(ports.http)}`],
      ['listen.ice_udp', `:${String(ports.iceUdp)}`],
      ['listen.ice_tcp', `:${String(ports.iceTcp)}`],
      ['listen.admin_socket', this.socket],
    ]);
    for (const [key, value] of Object.entries(this.overrides)) values.set(key, value);
    return ['--config', E2E_CONFIG, ...[...values].map(([key, value]) => configFlag(key, value))];
  }

  private async run(ports: Ports): Promise<'ready' | 'exited' | 'timeout'> {
    // A socket file that an earlier run of this server left behind would say "ready" for this one (see below).
    fs.rmSync(this.socket, { force: true });
    // The process writes to the file itself: nothing is lost when this worker dies, and nothing waits on a pipe.
    const log = fs.openSync(this.logPath, 'a');
    let child: ChildProcess;
    try {
      child = spawn(this.bin, ['serve', ...this.flags(ports)], {
        cwd: REPO_ROOT,
        env: serverEnv(this.dataDir),
        stdio: ['ignore', log, log],
      });
    } finally {
      fs.closeSync(log);
    }
    const gone = new Promise<void>((resolve) => {
      child.once('exit', () => {
        running.delete(child);
        resolve();
      });
      // The binary could not be started at all: there is no exit event then.
      child.once('error', (err) => {
        running.delete(child);
        fs.appendFileSync(this.logPath, `e2e: could not start ${this.bin}: ${err.message}\n`);
        resolve();
      });
    });
    running.add(child);
    this.proc = { child, gone };

    // Ready means that /readyz says so and that it is this process that answers: the admin socket is bound after
    // the HTTP port (04 §6.1 step 6), so it exists only when this process holds the port.
    const deadline = Date.now() + READY_TIMEOUT_MS;
    while (alive(child)) {
      if (fs.existsSync(this.socket) && (await isReady(this.url)) && alive(child)) return 'ready';
      if (Date.now() >= deadline) {
        await this.terminate();
        return 'timeout';
      }
      await Promise.race([sleep(50), gone]);
    }
    await gone;
    this.proc = null;
    return 'exited';
  }

  /** SIGTERM, then the exit; SIGKILL when the graceful shutdown takes too long. */
  private async terminate(): Promise<void> {
    const proc = this.proc;
    this.proc = null;
    if (!proc || !alive(proc.child)) return;
    proc.child.kill('SIGTERM');
    const exited = await Promise.race([proc.gone.then(() => true), sleep(STOP_TIMEOUT_MS).then(() => false)]);
    if (!exited) {
      proc.child.kill('SIGKILL');
      await proc.gone;
    }
  }

  /** Completes the first-run setup over REST (03 §7.8) with a generated admin, whose session `api` keeps. */
  async setUp(): Promise<void> {
    const token = new URL(await this.setupUrl()).hash.slice(1);
    const account = { username: uniqueName('admin'), password: newPassword() };
    const api = await request.newContext({ baseURL: this.url });
    this.api = api;
    const body = await postAccount(api, '/api/v1/auth/setup/complete', { token, ...account }, 201);
    this.admin = { id: (body as { user: { id: string } }).user.id, ...account };
  }

  async setupUrl(): Promise<string> {
    const args = ['setup-url', '--json', '--config', E2E_CONFIG, configFlag('listen.admin_socket', this.socket)];
    try {
      const { stdout } = await execFileAsync(this.bin, args, {
        cwd: REPO_ROOT,
        env: serverEnv(this.dataDir),
        timeout: 15_000,
      });
      return (JSON.parse(stdout) as { url: string }).url;
    } catch (err) {
      // The CLI's own message says why (exit 7: an admin exists; exit 4: the server doesn't answer).
      const stderr = (err as { stderr?: unknown }).stderr;
      throw new Error(
        `isshoni setup-url failed: ${typeof stderr === 'string' && stderr !== '' ? stderr.trim() : String(err)}`,
        {
          cause: err,
        },
      );
    }
  }

  private adminApi(): APIRequestContext {
    if (!this.api || !this.admin) {
      throw new Error('this e2e server has no admin (startServer({ setUp: false })): only a set-up server can do this');
    }
    return this.api;
  }

  async createInvite(options: InviteOptions = {}): Promise<InviteLink> {
    const res = await this.adminApi().post('/api/v1/invites', { data: options });
    const body = (await jsonOf(res, 201, 'POST /api/v1/invites')) as { invite: { id: string }; url: string };
    return { id: body.invite.id, url: body.url, token: new URL(body.url).hash.slice(1) };
  }

  async createUser(username: string = uniqueName('friend')): Promise<Account> {
    const invite = await this.createInvite({ note: 'e2e', maxUses: 1 });
    const account = { username, password: newPassword() };
    // A context of its own: the sign-up answers with the new member's session cookie, which must not replace the
    // admin's.
    const anonymous = await request.newContext({ baseURL: this.url });
    try {
      const data = { inviteToken: invite.token, ...account };
      const body = await postAccount(anonymous, '/api/v1/auth/register', data, 201);
      return { id: (body as { user: { id: string } }).user.id, ...account };
    } finally {
      await anonymous.dispose();
    }
  }

  async signIn(context: BrowserContext, account: Pick<Account, 'username' | 'password'>): Promise<void> {
    const data = { username: account.username, password: account.password };
    await postAccount(context.request, `${this.url}/api/v1/auth/login`, data, 200);
  }

  async restart(): Promise<void> {
    if (this.stopped) throw new Error('restart(): this e2e server was stopped');
    await this.terminate();
    await this.start();
    if (this.api) {
      // The admin's session is in the database; its connections to the old process are not worth keeping.
      const state = await this.api.storageState();
      await this.api.dispose();
      this.api = await request.newContext({ baseURL: this.url, storageState: state });
    }
  }

  async stop(): Promise<void> {
    if (this.stopped) return;
    this.stopped = true;
    await this.api?.dispose();
    this.api = null;
    await this.terminate();
    await fsp.rm(this.dataDir, { recursive: true, force: true });
    await fsp.rm(this.socket, { force: true });
  }
}

/** Starts one server. `where` is the test's or the worker's info: the output directory and the worker's index. */
async function launch(
  options: StartServerOptions,
  where: { readonly project: { readonly outputDir: string }; readonly workerIndex: number },
): Promise<Server> {
  const overrides = options.overrides ?? {};
  const owned = FIXTURE_KEYS.find((key) => key in overrides);
  if (owned !== undefined) throw new Error(`startServer: the fixture sets ${owned} itself; it can't be overridden`);

  await fsp.mkdir(where.project.outputDir, { recursive: true });
  const logPath = path.join(where.project.outputDir, `server-${String(where.workerIndex)}-${String(++servers)}.log`);
  const dataDir = await fsp.mkdtemp(path.join(os.tmpdir(), 'isshoni-e2e-'));
  const server = new Server(dataDir, logPath, overrides);
  started.push(server);
  try {
    await server.start();
    if (options.setUp ?? true) await server.setUp();
  } catch (err) {
    await server.stop();
    throw err;
  }
  return server;
}

// ---- Fixtures ----

export const test = base.extend<TestFixtures, WorkerFixtures>({
  server: [
    // eslint-disable-next-line no-empty-pattern -- Playwright reads the fixture names from this pattern
    async ({}, use, workerInfo) => {
      const server = await launch({}, workerInfo);
      await use(server);
      await server.stop();
    },
    { scope: 'worker' },
  ],

  // eslint-disable-next-line no-empty-pattern -- as above
  startServer: async ({}, use, testInfo) => {
    const own: Server[] = [];
    await use(async (options = {}) => {
      const server = await launch(options, testInfo);
      own.push(server);
      return server;
    });
    await Promise.all(own.map((server) => server.stop()));
  },

  // The default context's pages install window.__isshoni.
  context: async ({ context }, use) => {
    await enableDebug(context);
    await use(context);
  },

  asUser: async ({ browser, video }, use, testInfo) => {
    // Playwright records and keeps videos only for the contexts of its own `context` fixture, so this fixture
    // does the same for its contexts, by the `video` option of the config ('off', 'on', or kept after a failure).
    const mode = typeof video === 'string' ? video : video.mode;
    const videoDir = mode === 'off' ? null : await fsp.mkdtemp(path.join(os.tmpdir(), 'isshoni-e2e-video-'));
    const sessions: { context: BrowserContext; videos: Video[] }[] = [];

    await use(async (server, account) => {
      const context = await browser.newContext(videoDir === null ? {} : { recordVideo: { dir: videoDir } });
      const videos: Video[] = [];
      context.on('page', (page) => {
        const v = page.video();
        if (v) videos.push(v);
      });
      sessions.push({ context, videos });
      await enableDebug(context);
      const who = account ?? (await server.createUser());
      await server.signIn(context, who);
      return { account: who, context, page: await context.newPage() };
    });

    const keep = mode === 'on' || (mode !== 'off' && testInfo.status !== testInfo.expectedStatus);
    let saved = 0;
    for (const { context, videos } of sessions) {
      await context.close(); // a video file is complete once its context is closed
      if (!keep) continue;
      for (const v of videos) {
        const file = testInfo.outputPath(`video-user-${String(++saved)}.webm`);
        try {
          await v.saveAs(file);
          testInfo.attachments.push({ name: 'video', path: file, contentType: 'video/webm' });
        } catch {
          // a page that closed before it drew a frame has no video
        }
      }
    }
    if (videoDir !== null) await fsp.rm(videoDir, { recursive: true, force: true });
  },

  serverLogs: [
    // eslint-disable-next-line no-empty-pattern -- as above
    async ({}, use, testInfo) => {
      const before = started.length;
      await use(undefined);
      if (testInfo.status === testInfo.expectedStatus) return;
      // The servers this test started, and the ones that run now (the worker's). attach() copies the file.
      const logs = started.filter((server, i) => i >= before || server.isRunning).map((server) => server.logPath);
      for (const file of logs) {
        if (fs.existsSync(file)) await testInfo.attach(path.basename(file), { path: file, contentType: 'text/plain' });
      }
    },
    { auto: true },
  ],
});
