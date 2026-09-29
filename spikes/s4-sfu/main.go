// Command s4-sfu is spike S4 of the isshoni plan: a minimal Pion SFU that
// receives simulcast H.264 + Opus screen shares from browsers and forwards a
// per-viewer layer, switching layers on keyframes. See README.md.
package main

import (
	"embed"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pion/ice/v4"
)

//go:embed web
var webFiles embed.FS

func main() {
	addr := flag.String("addr", "localhost:8080", "HTTP listen address (browsers need localhost or HTTPS)")
	udpPort := flag.Int("udp", 7882, "UDP port for all media (ICE mux)")
	loopback := flag.Bool("loopback", false, "also offer loopback ICE candidates (e.g. when offline)")
	certFile := flag.String("tls-cert", "", "TLS certificate (enables HTTPS, e.g. from mkcert, for testing other devices)")
	keyFile := flag.String("tls-key", "", "TLS key")
	flag.Parse()

	var muxOpts []ice.UDPMuxFromPortOption
	if *loopback {
		muxOpts = append(muxOpts, ice.UDPMuxFromPortWithLoopback())
	}
	mux, err := ice.NewMultiUDPMuxFromPort(*udpPort, muxOpts...)
	if err != nil {
		slog.Error("udp mux", "err", err)
		os.Exit(1)
	}
	sfu, err := NewSFU(mux, *loopback)
	if err != nil {
		slog.Error("sfu", "err", err)
		os.Exit(1)
	}

	web, _ := fs.Sub(webFiles, "web")
	http.Handle("/", http.FileServerFS(web))
	http.HandleFunc("/ws", sfu.ServeWS)
	http.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) { // unattended test runs post results here
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		name := fmt.Sprintf("report-%d.json", time.Now().UnixNano())
		if err := os.WriteFile(name, body, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		slog.Info("report saved", "file", name, "from", r.UserAgent())
	})

	slog.Info("s4-sfu listening", "http", *addr, "udp", *udpPort)
	var ips []string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				ips = append(ips, ipn.IP.String())
			}
		}
	}
	slog.Info("media is offered on these addresses (UDP "+strconv.Itoa(*udpPort)+"); if browsers show 'connection failed', allow this program through the firewall",
		"addresses", strings.Join(ips, ", "))
	if *certFile != "" {
		err = http.ListenAndServeTLS(*addr, *certFile, *keyFile, nil)
	} else {
		err = http.ListenAndServe(*addr, nil)
	}
	slog.Error("server stopped", "err", err)
	os.Exit(1)
}
