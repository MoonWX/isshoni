// A scriptable signaling server behind FakeWebSocket (01 §19 "TypeScript"; 05 §19.1): install() puts a FakeWebSocket
// subclass on globalThis.WebSocket, so the real SignalClient (and everything built on it: 05's connection.ts,
// RoomSession, SubscriberPC, PublisherPC) runs unchanged against it.
//
// By default it behaves like a friendly hub: a new socket opens, a hello gets a welcome (resumed when it carries the
// last token issued for a connection, with a fresh token each time), a ping gets a pong. Each automatic step is a
// microtask: never inside a client call, and no fake time passes, so timings stay exact. Anything that yields to the
// event loop runs the whole chain; with Vitest's fake timers (which leave queueMicrotask alone by default)
//
//   const server = FakeSignalServer.install();
//   client.start();
//   await vi.advanceTimersByTimeAsync(0);   // open, hello, welcome: client.state === 'ready'
//
// and a timer that fires inside advanceTimersByTimeAsync(ms) (a backoff reconnect, a ping) gets its answers at the
// same fake instant. A test that wants to hold a step turns its auto flag off and calls accept(), welcome() and so on
// itself (those act at once). Call uninstall() in afterEach.
import type { ClientRequests, ServerMessageType, ServerMessages } from '../registry.gen';
import type { SignalRequestType } from '../signal-client';
import {
  CloseCodeServiceRestart,
  ErrorCodeServerShutdown,
  ErrorScopeConnection,
  MessageTypeError,
  MessageTypeHello,
  MessageTypeOK,
  MessageTypePing,
  MessageTypePong,
  MessageTypeServerShutdown,
  MessageTypeWelcome,
  ShutdownReasonRestart,
  type Error as WireError,
  type Hello,
  type Ping,
  type Welcome,
} from '../types.gen';
import { makeError, makeWelcome } from './fixtures';
import { FakeWebSocket, type ClientMessage, type FakeWebSocketPeer } from './fakeWebSocket';

export interface FakeSignalServerOptions {
  /** New sockets open (in a microtask). Default true. */
  autoOpen?: boolean;
  /** A hello gets a welcome (in a microtask). Default true. */
  autoWelcome?: boolean;
  /** A ping gets a pong (in a microtask). Default true. */
  autoPong?: boolean;
  /** A hello with the newest token of a known connection resumes it. Default true. */
  resume?: boolean;
  /** Merged into every welcome this server builds. */
  welcome?: Partial<Welcome>;
}

/** What a handle() callback answers: the ok payload, or an error. */
export type HandlerReply<K extends SignalRequestType> = { ok: ClientRequests[K]['result'] } | { error: WireError };

type AnyHandler = (data: unknown, msg: ClientMessage) => { ok: unknown } | { error: WireError };

export class FakeSignalServer implements FakeWebSocketPeer {
  autoOpen: boolean;
  autoWelcome: boolean;
  autoPong: boolean;
  resume: boolean;
  /** Merged into every welcome this server builds (after its own connection id and token). */
  welcomeDefaults: Partial<Welcome>;
  /** Every socket the client created, oldest first. */
  readonly sockets: FakeWebSocket[] = [];
  /** Every welcome sent, oldest first. */
  readonly welcomes: Welcome[] = [];

  readonly #handlers = new Map<string, AnyHandler>();
  /** The newest resume token of each live connection, → its id. */
  readonly #tokens = new Map<string, string>();
  #connections = 0;
  #issued = 0;
  #uninstall: (() => void) | undefined;

  constructor(opts: FakeSignalServerOptions = {}) {
    this.autoOpen = opts.autoOpen ?? true;
    this.autoWelcome = opts.autoWelcome ?? true;
    this.autoPong = opts.autoPong ?? true;
    this.resume = opts.resume ?? true;
    this.welcomeDefaults = opts.welcome ?? {};
  }

  /** Creates a server and makes globalThis.WebSocket create its sockets until uninstall(). */
  static install(opts?: FakeSignalServerOptions): FakeSignalServer {
    const server = new FakeSignalServer(opts);
    const peer: FakeWebSocketPeer = server;
    class ServerSocket extends FakeWebSocket {
      constructor(url: string | URL, protocols?: string | string[]) {
        super(url, protocols);
        this.peer = peer;
        peer.connected(this);
      }
    }
    const g = globalThis as { WebSocket?: unknown };
    const had = Object.prototype.hasOwnProperty.call(g, 'WebSocket');
    const previous = g.WebSocket;
    g.WebSocket = ServerSocket;
    server.#uninstall = () => {
      if (had) {
        g.WebSocket = previous;
      } else {
        delete g.WebSocket;
      }
    };
    return server;
  }

  /** Restores globalThis.WebSocket. */
  uninstall(): void {
    this.#uninstall?.();
    this.#uninstall = undefined;
  }

  /** The newest socket; throws when the client made none. */
  get socket(): FakeWebSocket {
    const s = this.sockets.at(-1);
    if (s === undefined) {
      throw new Error('FakeSignalServer: the client has not opened a socket');
    }
    return s;
  }

  /** The client's messages on one socket, or on all of them (oldest first), optionally of one type. */
  messages(type?: string, socket?: FakeWebSocket): ClientMessage[] {
    const sockets = socket !== undefined ? [socket] : this.sockets;
    return sockets.flatMap((s) => s.messages).filter((m) => type === undefined || m.type === type);
  }

  /** The newest client message of type (on any socket); throws when there is none. */
  last(type: string): ClientMessage {
    const m = this.messages(type).at(-1);
    if (m === undefined) {
      throw new Error(`FakeSignalServer: the client sent no ${type}`);
    }
    return m;
  }

  /** The hello data sent on each socket, oldest first. */
  get hellos(): Hello[] {
    return this.messages(MessageTypeHello).map((m) => m.data as Hello);
  }

  /** Forgets every connection, like a server restart: the next hello is not resumed. */
  restart(): void {
    this.#tokens.clear();
  }

  /** Answers requests of type from now on (in a microtask), with handler's ok or error. */
  handle<K extends SignalRequestType>(
    type: K,
    handler: (data: ClientRequests[K]['data'], msg: ClientMessage) => HandlerReply<K>,
  ): void {
    this.#handlers.set(type, handler as AnyHandler);
  }

  // ---- Server → client, at once ----

  /** Opens a socket that is still connecting. */
  accept(socket: FakeWebSocket = this.socket): void {
    socket.accept();
  }

  /**
   * Answers the socket's hello with a welcome: resumed with the same connection id when the hello carries the newest
   * token of a known connection (and resume is on), else a new connection; a fresh token either way. overrides win.
   */
  welcome(overrides: Partial<Welcome> = {}, socket: FakeWebSocket = this.socket): Welcome {
    const hello = socket.messages.find((m) => m.type === MessageTypeHello);
    if (hello?.id === undefined) {
      throw new Error('FakeSignalServer: welcome() before the socket sent hello');
    }
    const token = (hello.data as Partial<Hello> | undefined)?.resumeToken;
    const resumedId = this.resume && token !== undefined ? this.#tokens.get(token) : undefined;
    if (token !== undefined) {
      this.#tokens.delete(token);
    }
    const connectionId = resumedId ?? `c_fake${String(++this.#connections).padStart(12, '0')}`;
    const resumeToken = `r1.fake-token-${String(++this.#issued)}`;
    const w = makeWelcome({
      connectionId,
      resumeToken,
      resumed: resumedId !== undefined,
      ...this.welcomeDefaults,
      ...overrides,
    });
    this.#tokens.set(w.resumeToken, w.connectionId);
    this.welcomes.push(w);
    socket.deliver({ type: MessageTypeWelcome, re: hello.id, data: w });
    return w;
  }

  /** Sends a notification. */
  send<K extends ServerMessageType>(type: K, data: ServerMessages[K], socket: FakeWebSocket = this.socket): void {
    socket.deliver({ type, data });
  }

  /** Answers a request (the message, or its id) with ok. */
  reply(request: ClientMessage | string, result: object = {}, socket: FakeWebSocket = this.socket): void {
    socket.deliver({ type: MessageTypeOK, re: idOf(request), data: result });
  }

  /** Answers a request (the message, or its id) with an error. */
  replyError(request: ClientMessage | string, error: WireError, socket: FakeWebSocket = this.socket): void {
    socket.deliver({ type: MessageTypeError, re: idOf(request), data: error });
  }

  /** Sends an error notification (no re), e.g. a connection- or session-scope error before a close. */
  error(error: WireError, socket: FakeWebSocket = this.socket): void {
    socket.deliver({ type: MessageTypeError, data: error });
  }

  /** Closes the socket with a close frame. */
  close(code: number, reason = '', socket: FakeWebSocket = this.socket): void {
    socket.closeFromServer(code, reason);
  }

  /** The network fails under the socket: close 1006 without a close frame. */
  drop(socket: FakeWebSocket = this.socket): void {
    socket.fail();
  }

  /** What the hub sends when it stops (01 §19 "Shutdown"): server.shutdown, error{server_shutdown}, close 1012. */
  shutdown(reconnectInMs: number, socket: FakeWebSocket = this.socket): void {
    this.send(MessageTypeServerShutdown, { reason: ShutdownReasonRestart, reconnectInMs }, socket);
    this.error(makeError(ErrorCodeServerShutdown, ErrorScopeConnection), socket);
    socket.closeFromServer(CloseCodeServiceRestart);
  }

  // ---- FakeWebSocketPeer ----

  connected(socket: FakeWebSocket): void {
    this.sockets.push(socket);
    if (this.autoOpen) {
      later(() => {
        if (socket.readyState === FakeWebSocket.CONNECTING) {
          socket.accept();
        }
      });
    }
  }

  received(socket: FakeWebSocket, text: string): void {
    const msg = JSON.parse(text) as ClientMessage;
    if (msg.type === MessageTypeHello && this.autoWelcome) {
      later(() => {
        if (socket.readyState === FakeWebSocket.OPEN) {
          this.welcome({}, socket);
        }
      });
    } else if (msg.type === MessageTypePing && this.autoPong) {
      const t = (msg.data as Partial<Ping> | undefined)?.t ?? 0;
      later(() => {
        this.send(MessageTypePong, { t, serverTimeMs: Date.now() }, socket);
      });
    } else if (msg.id !== undefined) {
      const handler = this.#handlers.get(msg.type);
      if (handler !== undefined) {
        later(() => {
          const answer = handler(msg.data, msg);
          if ('error' in answer) {
            this.replyError(msg, answer.error, socket);
          } else {
            this.reply(msg, answer.ok as object, socket);
          }
        });
      }
    }
  }

  closedByClient(): void {
    // Nothing to do: the client's close code is on the socket (clientClose) for tests to check.
  }
}

function idOf(request: ClientMessage | string): string {
  const id = typeof request === 'string' ? request : request.id;
  if (id === undefined) {
    throw new Error('FakeSignalServer: that message is a notification, not a request');
  }
  return id;
}

/** After the current client call returns, like a network round trip, but without letting (fake) time pass. */
function later(fn: () => void): void {
  queueMicrotask(fn);
}
