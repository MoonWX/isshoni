package itest

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The ICE-TCP checks of 04 §17: media over TCP alone, through the 443 multiplexer in a TLS mode and through
// listen.ice_tcp in off mode. A network that blocks UDP leaves a friend these two ways in.

// watchOverTCP runs one share from a publisher to a viewer whose PeerConnections know nothing but TCP, and checks
// that the viewer gets the media and that both PeerConnections selected a TCP pair to the server's port.
func watchOverTCP(t *testing.T, w *world, port int) {
	t.Helper()
	alice := w.connect(w.member("alice"), protocol.RoleFull)
	sh := alice.publish(7, sfutest.LoopbackTCPSettings())
	alice.waitLive(sh.id)
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackTCPSettings(), 0)
	bob.waitLive(sh.id)
	bob.subscribe(want(sh.id, true))
	video, audio := track(t, viewer, sh.id, webrtc.RTPCodecTypeVideo), track(t, viewer, sh.id, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 30)
	waitPackets(t, audio, 50)
	bob.waitStatus(want(sh.id, true))

	// The viewer decodes what it got: a stream that starts on an SPS, of whole frames in order, each with the fake
	// media's marker where the publisher put it; and the audio's packets one after the other. TCP loses nothing.
	vp := checkVideo(t, "video", video.Packets())
	if runs := layerRuns(vp); runs != "f" {
		t.Errorf("the viewer got the layers %q, want the full layer", runs)
	}
	ap := audio.Packets()
	for _, check := range []func([]sfutest.Packet) error{sfutest.CheckContinuous, sfutest.CheckAudioMarkers} {
		if err := check(ap); err != nil {
			t.Errorf("audio: %v", err)
		}
	}
	for name, r := range map[string]*sfutest.Recorder{"video": video, "audio": audio} {
		if st := r.Stats(); st.Gaps != 0 || st.Lost != 0 || st.Duplicates != 0 {
			t.Errorf("%s stats over TCP = %+v, want no gap", name, st)
		}
	}

	// Both clients reach the server over TCP, on the one port.
	for name, pc := range map[string]*webrtc.PeerConnection{"the publisher": sh.pub.PC(), "the viewer": viewer.PC()} {
		pair := selectedPair(t, name, pc)
		if pair.Local.Protocol != webrtc.ICEProtocolTCP || pair.Remote.Protocol != webrtc.ICEProtocolTCP {
			t.Errorf("%s selected a %s/%s pair, want TCP", name, pair.Local.Protocol, pair.Remote.Protocol)
		}
		if int(pair.Remote.Port) != port || pair.Remote.Address != "127.0.0.1" {
			t.Errorf("%s reaches the server at %s:%d, want 127.0.0.1:%d", name, pair.Remote.Address, pair.Remote.Port, port)
		}
	}
	alice.check()
	bob.check()
}

// TestICETCPVia443 is the check of 04 §17: a TLS-mode server with UDP and listen.ice_tcp both turned off has one
// port for everything. A Go publisher that knows only TCP shares, a Viewer decodes its media, and the WSS
// signaling of both runs on that same port: both clients' selected candidate pairs are TCP with the TLS listener's
// port as the remote port.
func TestICETCPVia443(t *testing.T) {
	w := newWorld(t, servertest.Options{TLS: true, Config: func(c *config.Config) {
		c.Listen.ICEUDP, c.Listen.ICETCP = "", ""
	}})
	addrs := w.srv.Srv.Addrs()
	https, ok := addrs.HTTPS.(*net.TCPAddr)
	if !ok || addrs.ICEUDP != nil || addrs.ICETCP != nil {
		t.Fatalf("Addrs = %+v, want the 443 multiplexer and no ICE listener of its own", addrs)
	}
	// Signaling is WSS on that port.
	u, err := url.Parse(w.srv.WSURL)
	if err != nil || u.Scheme != "wss" || u.Port() != strconv.Itoa(https.Port) {
		t.Fatalf("the signaling URL is %s (%v), want wss on port %d", w.srv.WSURL, err, https.Port)
	}
	watchOverTCP(t, w, https.Port)
	// No other port could have carried it: the server's log names the one.
	if want := " (tls=manual) media udp/off ice-tcp " + strconv.Itoa(https.Port) + `"`; !strings.Contains(w.srv.Logs(), want) {
		t.Errorf("no ready line with %q:\n%s", want, w.srv.Logs())
	}
}

// TestICETCPVia7882 is the same in off mode, where the operator's proxy owns port 443 and the server has no
// multiplexer (04 §8.5): with UDP turned off, media runs over the ICE-TCP listener of listen.ice_tcp.
func TestICETCPVia7882(t *testing.T) {
	w := newWorld(t, servertest.Options{Config: func(c *config.Config) { c.Listen.ICEUDP = "" }})
	addrs := w.srv.Srv.Addrs()
	tcp, ok := addrs.ICETCP.(*net.TCPAddr)
	if !ok || addrs.ICEUDP != nil || addrs.HTTPS != nil {
		t.Fatalf("Addrs = %+v, want the ICE-TCP listener alone", addrs)
	}
	watchOverTCP(t, w, tcp.Port)
}
