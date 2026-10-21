package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

//go:embed web/clock.html
var clockPage []byte

// The clock page loads nothing: its script and style are inline, and it has no other resource.
const clockCSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; base-uri 'none'; " +
	"form-action 'none'; frame-ancestors 'self'"

// clockHandler serves the clock page at / and /clock.html; every other path is 404.
func clockHandler() http.Handler {
	page := func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Length", strconv.Itoa(len(clockPage)))
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", clockCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write(clockPage)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", page)
	mux.HandleFunc("GET /clock.html", page)
	return mux
}

// serveClock serves the clock page on ln until ctx ends, then shuts the server down within 2 s.
func serveClock(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           clockHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serving the clock page: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := srv.Shutdown(sctx)
	if serr := <-errc; serr != nil && !errors.Is(serr, http.ErrServerClosed) && err == nil {
		err = serr
	}
	return err
}

// runServe is `g2g serve`.
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("serve", stderr, "g2g serve [-port N]",
		"Serves the clock page on 127.0.0.1 until Ctrl+C.")
	port := fs.Int("port", 8765, "the port on 127.0.0.1 (0 picks a free one)")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintf(stderr, "g2g serve: -port %d is not a port\n", *port)
		return exitInconclusive
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintf(stderr, "g2g serve: %v\n", err)
		return exitInconclusive
	}
	fmt.Fprintf(stdout, "g2g: the clock page is at http://%s/ (?cuts=0 turns the content cuts off); Ctrl+C stops\n",
		ln.Addr())
	if err := serveClock(ctx, ln); err != nil {
		fmt.Fprintf(stderr, "g2g serve: %v\n", err)
		return exitInconclusive
	}
	return exitOK
}
