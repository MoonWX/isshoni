// A stand-in for the browser's WebSocket (05 §3, web/src/protocol/testing/): the client side behaves like the real
// one (readyState, send, close with the browser's code rules, on* handlers and addEventListener), and the test drives
// the server side by hand: accept, deliver, closeFromServer, fail. FakeSignalServer (fakeSignalServer.ts) builds the
// protocol on top of it; tests that need a single raw socket can use this class alone.
//
// Nothing here fires synchronously inside a client call: the close event after the client's close() comes in a
// microtask, later as in a browser (but without fake time passing). Everything the test calls fires at once.

/** A message the client sent, parsed. */
export interface ClientMessage {
  type: string;
  id?: string;
  data?: unknown;
}

/** The server side of fake sockets (FakeSignalServer): told about every socket, frame and client close. */
export interface FakeWebSocketPeer {
  connected(socket: FakeWebSocket): void;
  received(socket: FakeWebSocket, text: string): void;
  closedByClient(socket: FakeWebSocket, code: number | undefined, reason: string | undefined): void;
}

type Handler<E extends Event> = ((this: FakeWebSocket, ev: E) => unknown) | null;

export class FakeWebSocket extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  readonly CONNECTING = 0;
  readonly OPEN = 1;
  readonly CLOSING = 2;
  readonly CLOSED = 3;

  readonly url: string;
  readonly protocol = '';
  readonly extensions = '';
  readonly bufferedAmount = 0;
  binaryType: BinaryType = 'blob';
  readyState: number = FakeWebSocket.CONNECTING;

  onopen: Handler<Event> = null;
  onmessage: Handler<MessageEvent> = null;
  onclose: Handler<CloseEvent> = null;
  onerror: Handler<Event> = null;

  /** The text frames the client sent, in order. */
  readonly frames: string[] = [];
  /** The client's close() call: its code and reason. */
  clientClose: { code: number | undefined; reason: string | undefined } | undefined;
  /** The close event the client got (from either side), once it fired. */
  closeEvent: { code: number; reason: string; wasClean: boolean } | undefined;
  /** Set before the constructor returns by the subclass FakeSignalServer.install() puts on globalThis. */
  peer: FakeWebSocketPeer | undefined;

  constructor(url: string | URL, protocols?: string | string[]) {
    super();
    this.url = new URL(url).href;
    if (protocols !== undefined && protocols.length > 0) {
      throw new Error('FakeWebSocket: the isshoni client uses no subprotocols (01 D1)');
    }
  }

  /** The client's frames, parsed as envelopes (frames that aren't JSON objects are skipped). */
  get messages(): ClientMessage[] {
    const out: ClientMessage[] = [];
    for (const frame of this.frames) {
      const v: unknown = JSON.parse(frame);
      if (typeof v === 'object' && v !== null && typeof (v as { type?: unknown }).type === 'string') {
        out.push(v as ClientMessage);
      }
    }
    return out;
  }

  // ---- The client side (the WebSocket API) ----

  send(data: string | ArrayBufferLike | Blob | ArrayBufferView): void {
    if (this.readyState === FakeWebSocket.CONNECTING) {
      throw new DOMException('Still in CONNECTING state.', 'InvalidStateError');
    }
    if (typeof data !== 'string') {
      throw new TypeError('FakeWebSocket: the signaling protocol sends text frames only (01 §3.3)');
    }
    if (this.readyState !== FakeWebSocket.OPEN) {
      return; // a browser drops frames sent while closing or closed
    }
    this.frames.push(data);
    this.peer?.received(this, data);
  }

  close(code?: number, reason?: string): void {
    if (code !== undefined && code !== 1000 && (code < 3000 || code > 4999)) {
      throw new DOMException(`The close code ${String(code)} is neither 1000 nor in 3000-4999.`, 'InvalidAccessError');
    }
    if (this.readyState === FakeWebSocket.CLOSING || this.readyState === FakeWebSocket.CLOSED) {
      return;
    }
    this.clientClose = { code, reason };
    this.readyState = FakeWebSocket.CLOSING;
    this.peer?.closedByClient(this, code, reason);
    queueMicrotask(() => {
      this.#finish(code ?? 1005, reason ?? '', true);
    });
  }

  // ---- The server side (called by tests or FakeSignalServer) ----

  /** The connection opens: readyState OPEN and an open event. */
  accept(): void {
    if (this.readyState !== FakeWebSocket.CONNECTING) {
      throw new Error(`FakeWebSocket: accept() in readyState ${String(this.readyState)}`);
    }
    this.readyState = FakeWebSocket.OPEN;
    this.#fire(new Event('open'), this.onopen);
  }

  /** The server sends a frame: an object is JSON-encoded, a string goes as is. Dropped unless open. */
  deliver(message: unknown): void {
    if (this.readyState !== FakeWebSocket.OPEN) {
      return;
    }
    const data = typeof message === 'string' ? message : JSON.stringify(message);
    this.#fire(new MessageEvent('message', { data }), this.onmessage);
  }

  /** The server closes the connection with a close frame. */
  closeFromServer(code = 1000, reason = ''): void {
    this.#finish(code, reason, true);
  }

  /** The network fails: an error event, then close 1006 without a close frame. */
  fail(): void {
    if (this.readyState === FakeWebSocket.CLOSED) {
      return;
    }
    this.#fire(new Event('error'), this.onerror);
    this.#finish(1006, '', false);
  }

  #finish(code: number, reason: string, wasClean: boolean): void {
    if (this.readyState === FakeWebSocket.CLOSED) {
      return;
    }
    this.readyState = FakeWebSocket.CLOSED;
    this.closeEvent = { code, reason, wasClean };
    this.#fire(closeEvent(code, reason, wasClean), this.onclose);
  }

  #fire<E extends Event>(ev: E, handler: Handler<E>): void {
    handler?.call(this, ev);
    this.dispatchEvent(ev);
  }
}

function closeEvent(code: number, reason: string, wasClean: boolean): CloseEvent {
  if (typeof CloseEvent === 'function') {
    return new CloseEvent('close', { code, reason, wasClean });
  }
  return Object.assign(new Event('close'), { code, reason, wasClean });
}
