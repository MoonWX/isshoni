package signaltest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
)

const (
	clientReadLimit = 1 << 20 // the client's read limit: above every server message
	clientQueue     = 4096    // messages the reader queues before it stops reading
)

// DialOptions configure Dial.
type DialOptions struct {
	Header http.Header // request headers: CookieHeader, Origin, ClientIPHeader
	NoPong bool        // don't answer the server's ping frames
	Paused bool        // don't read until Resume: a client that stops reading
}

// RefusedError is Dial's error when the server answered the upgrade with an HTTP status instead of 101.
type RefusedError struct {
	Status     int
	RetryAfter string // the Retry-After header
	Err        error
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("signaltest: upgrade refused with %d: %v", e.Status, e.Err)
}

func (e *RefusedError) Unwrap() error { return e.Err }

// Client is a raw protocol client for hub tests: one WebSocket and a reader goroutine that queues every message it
// receives. Recv returns them in order, then the reader's final error. Pause and Resume stop and restart the reader,
// which also stops answering ping frames and, on a PipeNet, blocks the server's writes.
type Client struct {
	WS *websocket.Conn

	msgs     chan Frame
	done     chan struct{}
	err      error         // the reader's final error; read it after done is closed
	quit     chan struct{} // closed by Close: the reader stops waiting
	quitOnce sync.Once
	ids      atomic.Int64

	mu   sync.Mutex
	gate chan struct{} // non-nil while paused: the reader waits for it to close before its next read
}

// errClientClosed is the reader's error after Close interrupted it.
var errClientClosed = errors.New("signaltest: client closed")

// Dial opens a WebSocket to url through hc. When the server refuses the upgrade the error is a *RefusedError.
func Dial(ctx context.Context, hc *http.Client, url string, o DialOptions) (*Client, error) {
	opts := &websocket.DialOptions{HTTPClient: hc, HTTPHeader: o.Header}
	if o.NoPong {
		opts.OnPingReceived = func(context.Context, []byte) bool { return false }
	}
	ws, resp, err := websocket.Dial(ctx, url, opts)
	if resp != nil && resp.Body != nil { // nil after a successful Dial; a copy of the start of the body otherwise
		defer resp.Body.Close()
	}
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, &RefusedError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After"), Err: err}
		}
		return nil, fmt.Errorf("signaltest: dial: %w", err)
	}
	ws.SetReadLimit(clientReadLimit)
	c := &Client{WS: ws, msgs: make(chan Frame, clientQueue), done: make(chan struct{}),
		quit: make(chan struct{})}
	if o.Paused {
		c.Pause()
	}
	go c.read(context.WithoutCancel(ctx)) // the reader outlives the dial
	return c, nil
}

func (c *Client) read(ctx context.Context) {
	defer close(c.done)
	for {
		c.mu.Lock()
		gate := c.gate
		c.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-c.quit:
				c.err = errClientClosed
				return
			}
		}
		typ, b, err := c.WS.Read(ctx)
		if err != nil {
			c.err = err
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		env, err := protocol.ParseEnvelope(b)
		if err != nil {
			c.err = fmt.Errorf("signaltest: bad message from the server: %w", err)
			_ = c.WS.CloseNow()
			return
		}
		select {
		case c.msgs <- Frame{Envelope: env, Raw: b}:
		case <-c.quit:
			c.err = errClientClosed
			return
		}
	}
}

// Pause stops the reader before its next read. A read already waiting for a frame completes and queues it.
func (c *Client) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate == nil {
		c.gate = make(chan struct{})
	}
}

// Resume restarts a paused reader.
func (c *Client) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate != nil {
		close(c.gate)
		c.gate = nil
	}
}

// Send sends one message; id "" sends none.
func (c *Client) Send(t protocol.MessageType, id string, data any) error {
	b, err := protocol.Marshal(t, id, "", data)
	if err != nil {
		return fmt.Errorf("signaltest: encode: %w", err)
	}
	return c.SendRaw(b)
}

// NextID returns a fresh request id.
func (c *Client) NextID() string { return strconv.FormatInt(c.ids.Add(1), 10) }

// SendRaw sends one text frame as is.
func (c *Client) SendRaw(b []byte) error {
	if err := c.WS.Write(context.Background(), websocket.MessageText, b); err != nil {
		return fmt.Errorf("signaltest: write: %w", err)
	}
	return nil
}

// SendBinary sends one binary frame.
func (c *Client) SendBinary(b []byte) error {
	if err := c.WS.Write(context.Background(), websocket.MessageBinary, b); err != nil {
		return fmt.Errorf("signaltest: write: %w", err)
	}
	return nil
}

// Frame is one message the client received: the parsed envelope and the frame's bytes.
type Frame struct {
	Envelope protocol.Envelope
	Raw      []byte
}

// Recv returns the next message. After the socket has closed and every queued message has been read, it returns the
// reader's error: for a close frame, a websocket.CloseError (see websocket.CloseStatus).
func (c *Client) Recv(ctx context.Context) (protocol.Envelope, error) {
	f, err := c.RecvFrame(ctx)
	return f.Envelope, err
}

// RecvFrame is Recv with the frame's bytes, for tests that compare what several clients received.
func (c *Client) RecvFrame(ctx context.Context) (Frame, error) {
	select {
	case f := <-c.msgs:
		return f, nil
	default:
	}
	select {
	case f := <-c.msgs:
		return f, nil
	case <-c.done:
		select {
		case f := <-c.msgs:
			return f, nil
		default:
			return Frame{}, c.err
		}
	case <-ctx.Done():
		return Frame{}, fmt.Errorf("signaltest: recv: %w", ctx.Err())
	}
}

// Pending returns the number of queued messages.
func (c *Client) Pending() int { return len(c.msgs) }

// Hello sends h as a hello request and returns the welcome. When the server answers with an error message the error
// is a *protocol.Error.
func (c *Client) Hello(ctx context.Context, h protocol.Hello) (protocol.Welcome, error) {
	id := c.NextID()
	if err := c.Send(protocol.MessageTypeHello, id, h); err != nil {
		return protocol.Welcome{}, err
	}
	env, err := c.Recv(ctx)
	if err != nil {
		return protocol.Welcome{}, err
	}
	switch env.Type {
	case protocol.MessageTypeWelcome:
		if env.Re != id {
			return protocol.Welcome{}, fmt.Errorf("signaltest: welcome re %q, want %q", env.Re, id)
		}
		return protocol.Decode[protocol.Welcome](env)
	case protocol.MessageTypeError:
		pe, err := protocol.Decode[protocol.Error](env)
		if err != nil {
			return protocol.Welcome{}, err
		}
		return protocol.Welcome{}, &pe
	}
	return protocol.Welcome{}, fmt.Errorf("signaltest: got %s, want welcome", env.Type)
}

// ExpectError reads the next message, which must be an error, and returns it with the envelope's re.
func (c *Client) ExpectError(ctx context.Context) (protocol.Error, string, error) {
	env, err := c.Recv(ctx)
	if err != nil {
		return protocol.Error{}, "", err
	}
	if env.Type != protocol.MessageTypeError {
		return protocol.Error{}, "", fmt.Errorf("signaltest: got %s, want error", env.Type)
	}
	pe, err := protocol.Decode[protocol.Error](env)
	return pe, env.Re, err
}

// Closed is closed when the reader has ended: the socket is closed.
func (c *Client) Closed() <-chan struct{} { return c.done }

// CloseStatus waits for the socket to close and returns its close code: the code of the server's close frame, or -1
// when there was none. Queued messages stay queued.
func (c *Client) CloseStatus(ctx context.Context) (websocket.StatusCode, error) {
	c.Resume()
	select {
	case <-c.done:
		return websocket.CloseStatus(c.err), nil
	case <-ctx.Done():
		return 0, fmt.Errorf("signaltest: close status: %w", ctx.Err())
	}
}

// Err returns the reader's final error once Closed is closed, else nil.
func (c *Client) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// Close closes the socket without a close handshake and waits for the reader.
func (c *Client) Close() {
	c.quitOnce.Do(func() { close(c.quit) })
	_ = c.WS.CloseNow()
	<-c.done
}

// CloseWith closes the socket with a close handshake and the given code.
func (c *Client) CloseWith(code websocket.StatusCode) error {
	err := c.WS.Close(code, "")
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("signaltest: close: %w", err)
	}
	return nil
}

// DefaultHello is a web client's hello: protocol 1, role full, H.264 and Opus caps.
func DefaultHello() protocol.Hello {
	return protocol.Hello{
		Protocol:    protocol.Version,
		MinProtocol: protocol.MinVersion,
		Features:    []protocol.Feature{},
		Client:      protocol.ClientInfo{Kind: protocol.ClientKindWeb, Version: "0.1.0", OS: protocol.ClientOSMacOS, Browser: "chrome"},
		Role:        protocol.RoleFull,
		Caps: protocol.Caps{
			Decode:         []protocol.CodecKey{protocol.CodecH264ConstrainedBaseline, protocol.CodecH264High, protocol.CodecOpus},
			Encode:         []protocol.CodecKey{protocol.CodecH264ConstrainedBaseline, protocol.CodecH264High, protocol.CodecOpus},
			Simulcast:      true,
			DisplayCapture: true,
		},
	}
}
