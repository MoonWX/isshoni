// Command relserve serves a locally built test release over HTTPS the way GitHub Releases serves a real one, so the
// installer tests can run deploy/install.sh against it (docs/m1/06-deploy-and-ci.md §4.13 and §11.2, "06" below).
// deploy/test/distro-test.sh starts it and points the installer at it with ISSHONI_DOWNLOAD_BASE:
//
//	go -C tools run ./relserve -dir DIR -latest 0.0.1-ci.2 -ca-out DIR/ca.pem -addr 127.0.0.1:0 -ready-file DIR/addr
//
// # What it serves
//
// -dir holds one or more release roots. A release root is a directory with a download/ directory in it, laid out
// like the URLs of a GitHub repository's /releases:
//
//	DIR/download/v0.0.1-ci.1/isshoni_0.0.1-ci.1_linux_amd64.tar.gz
//	DIR/download/v0.0.1-ci.1/checksums.txt
//	DIR/download/v0.0.1-ci.1/checksums.txt.sig
//	DIR/tampered/download/v0.0.1-ci.1/…        a second root, a mirror with a changed file (scenario C6)
//
// For a release root at the URL path P ("" for DIR itself, "/tampered" above), GET and HEAD answer:
//
//	P/download/v<version>/<file>   the file
//	P/latest                       302 to https://<host>P/tag/v<latest>, an absolute URL as GitHub sends it
//	P/latest/download/<file>       302 to https://<host>P/download/v<latest>/<file>
//	P/tag/v<version>               200 when that version is in the root (the page a browser would land on)
//
// <latest> is the -latest flag; without it the two latest URLs answer 404. Everything else is 404: there are no
// directory listings, and nothing outside -dir is ever opened (the files are read through an os.Root, which also
// refuses a symbolic link that leaves the directory).
//
// # TLS
//
// The installer downloads over HTTPS only, so relserve makes a certificate authority and a server certificate when
// it starts, both in memory and valid for two days. The CA certificate goes to -ca-out, for the test machine to
// trust (update-ca-certificates, update-ca-trust). The CA's private key is never written anywhere: once the process
// ends, nothing can be signed with it again. The server certificate names the -hosts (DNS names and IP addresses).
//
// -addr may end in :0 for a free port. Once the listener is up, the address that was bound, host:port, is written to
// -ready-file (through a rename, so a reader never sees half of it) and printed on stdout. relserve then serves
// until it gets SIGINT or SIGTERM.
//
// Exit status: 0 after a signal, 1 when it could not start or serve, 2 on a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/semver"
)

// defaultHosts are the names a test reaches relserve by: this machine, and the host as a container sees it
// (podman's name and Docker's).
const defaultHosts = "localhost,127.0.0.1,::1,host.containers.internal,host.docker.internal"

// shutdownTimeout bounds the wait for requests in flight when relserve is told to stop.
const shutdownTimeout = 5 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run parses args, starts the server and serves until ctx is done.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("relserve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "the directory with the release roots (required)")
	addr := fs.String("addr", "127.0.0.1:8443", "the address to listen on; a port of 0 picks a free one")
	latest := fs.String("latest", "", "the version that /latest redirects to, like 0.0.1-ci.2 (default: /latest answers 404)")
	hosts := fs.String("hosts", defaultHosts, "comma-separated DNS names and IP addresses for the server certificate")
	caOut := fs.String("ca-out", "", "write the CA certificate (PEM) to this file (required)")
	readyFile := fs.String("ready-file", "", "write the bound address, host:port, to this file once the server listens")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: relserve -dir DIR -ca-out FILE [-addr HOST:PORT] [-latest VERSION] [-hosts LIST] [-ready-file FILE]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "relserve: "+format+"\n", a...)
		fs.Usage()
		return 2
	}
	switch {
	case fs.NArg() != 0:
		return usage("unexpected argument %q", fs.Arg(0))
	case *dir == "":
		return usage("missing -dir")
	case *caOut == "":
		return usage("missing -ca-out")
	case *latest != "" && !validVersion(*latest):
		return usage("-latest %q is not a version like 0.1.0 or 0.0.1-ci.2 (no leading v)", *latest)
	}
	names := splitList(*hosts)
	if len(names) == 0 {
		return usage("-hosts names no host")
	}

	if err := serve(ctx, options{
		dir: *dir, addr: *addr, latest: *latest, hosts: names, caOut: *caOut, readyFile: *readyFile,
	}, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "relserve: %v\n", err)
		return 1
	}
	return 0
}

type options struct {
	dir, addr, latest string
	hosts             []string
	caOut, readyFile  string
}

// serve listens, writes the CA certificate and the ready file, and serves until ctx is done.
func serve(ctx context.Context, o options, stdout, stderr io.Writer) error {
	root, err := os.OpenRoot(o.dir)
	if err != nil {
		return fmt.Errorf("-dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	ca, err := newAuthority(o.hosts, time.Now())
	if err != nil {
		return fmt.Errorf("make the test CA: %w", err)
	}
	if err := writeFileAtomic(o.caOut, ca.certPEM); err != nil {
		return fmt.Errorf("-ca-out: %w", err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", o.addr)
	if err != nil {
		return fmt.Errorf("-addr: %w", err)
	}
	bound := ln.Addr().String()

	srv := &http.Server{
		Handler:           newHandler(root, o.latest, stderr),
		TLSConfig:         ca.serverConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          nil,
	}
	served := make(chan error, 1)
	go func() { served <- srv.ServeTLS(ln, "", "") }()

	if o.readyFile != "" {
		if err := writeFileAtomic(o.readyFile, []byte(bound+"\n")); err != nil {
			_ = srv.Close()
			<-served
			return fmt.Errorf("-ready-file: %w", err)
		}
	}
	if _, err := fmt.Fprintf(stdout, "relserve: serving %s on https://%s (CA certificate: %s)\n", o.dir, bound, o.caOut); err != nil {
		_ = srv.Close()
		<-served
		return err
	}

	select {
	case err := <-served:
		return err // the listener failed: ServeTLS never returns nil
	case <-ctx.Done():
	}
	// The caller's context is done, so the wait for requests in flight needs one of its own.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(stopCtx); err != nil {
		_ = srv.Close()
	}
	<-served
	return nil
}

// validVersion reports whether v is a SemVer version without the leading "v" and without build metadata, which is
// how release versions appear in asset names and tags (06 §3).
func validVersion(v string) bool {
	return semver.IsValid("v"+v) && semver.Canonical("v"+v) == "v"+v
}

// splitList splits a comma-separated list and drops empty items.
func splitList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// writeFileAtomic writes data to path through a temporary file in the same directory and a rename, so a reader
// that waits for the file never sees a part of it.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	// CreateTemp makes the file 0600; the CA certificate and the address are for other programs to read.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // G302: a public certificate and a port, see above
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
