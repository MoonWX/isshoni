package doctor

import (
	"errors"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

func TestCheckUDPBuffers(t *testing.T) {
	const sysctlFix = `printf 'net.core.rmem_max=8388608\nnet.core.wmem_max=8388608\n' | sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system`
	low := func(w *world) {
		w.files["proc/sys/net/core/rmem_max"].Data = []byte("212992\n")
		w.files["proc/sys/net/core/wmem_max"].Data = []byte("212992\n")
	}
	t.Run("ok", func(t *testing.T) {
		w := newWorld(t)
		c := w.check("udp_buffers")
		want(t, c, api.DoctorStatusOK, codeUDPBuffersOK, "")
		contains(t, c.Message, "net.core.rmem_max is 8388608, the media sockets have 8388608")
	})
	t.Run("the limits are low", func(t *testing.T) {
		w := newWorld(t)
		low(w)
		c := w.check("udp_buffers")
		want(t, c, api.DoctorStatusWarn, codeUDPBuffersLow, fixUDPBuffersSysctl)
		if c.Message != "net.core.rmem_max is 212992, isshoni wants 8388608" {
			t.Errorf("message %q", c.Message)
		}
		if c.Fix != sysctlFix {
			t.Errorf("fix:\n got %q\nwant %q", c.Fix, sysctlFix)
		}
		if c.Params["rmemMax"] != int64(212992) || c.Params["want"] != int64(8388608) {
			t.Errorf("params %v", c.Params)
		}
	})
	t.Run("only the send limit is low", func(t *testing.T) {
		w := newWorld(t)
		w.files["proc/sys/net/core/wmem_max"].Data = []byte("212992\n")
		c := w.check("udp_buffers")
		want(t, c, api.DoctorStatusWarn, codeUDPBuffersLow, fixUDPBuffersSysctl)
		contains(t, c.Message, "net.core.wmem_max is 212992, isshoni wants 8388608")
	})
	t.Run("isshoni's sysctl file is there and not loaded", func(t *testing.T) {
		for _, file := range sysctlFiles {
			w := newWorld(t)
			low(w)
			w.files[file] = &fsFile{Data: []byte("net.core.rmem_max=8388608\n")}
			c := w.check("udp_buffers")
			want(t, c, api.DoctorStatusWarn, codeUDPBuffersLow, fixUDPBuffersSysctlReload)
			if c.Fix != "sudo sysctl --system" {
				t.Errorf("%s: fix %q", file, c.Fix)
			}
		}
	})
	t.Run("in Docker the host has the sysctl", func(t *testing.T) {
		w := newWorld(t)
		low(w)
		w.container("docker", true)
		w.files[sysctlFiles[0]] = &fsFile{} // a file inside the container says nothing about the host
		c := w.check("udp_buffers")
		want(t, c, api.DoctorStatusWarn, codeUDPBuffersLow, fixUDPBuffersSysctl)
		if c.Fix != "on the Docker host: "+sysctlFix {
			t.Errorf("fix %q", c.Fix)
		}
	})
	t.Run("the limits were raised after the server started", func(t *testing.T) {
		w := newWorld(t)
		w.live.UDPRcvBufBytes, w.live.UDPSndBufBytes = 212992, 212992
		c := w.check("udp_buffers")
		want(t, c, api.DoctorStatusWarn, codeUDPBuffersLow, fixUDPBuffersRestart)
		contains(t, c.Message, "the media sockets receive into 212992 bytes, isshoni wants 8388608")
		contains(t, c.Fix, "restart isshoni so that its sockets get the larger buffers: sudo systemctl restart isshoni")
	})
	t.Run("a custom target", func(t *testing.T) {
		w := newWorld(t, "--network.udp-buffer-bytes", "16777216")
		c := w.check("udp_buffers")
		want(t, c, api.DoctorStatusWarn, codeUDPBuffersLow, fixUDPBuffersSysctl)
		contains(t, c.Fix, "net.core.rmem_max=16777216")
	})
	t.Run("UDP off", func(t *testing.T) {
		w := newWorld(t, "--listen.ice-udp", "")
		want(t, w.check("udp_buffers"), api.DoctorStatusOK, codeUDPBuffersUDPOff, "")
	})
	t.Run("offline reads the limits", func(t *testing.T) {
		w := newWorld(t)
		w.live = nil
		c := w.check("udp_buffers")
		want(t, c, api.DoctorStatusOK, codeUDPBuffersOK, "")
		contains(t, c.Message, "net.core.rmem_max is 8388608, net.core.wmem_max is 8388608")
		low(w)
		want(t, w.check("udp_buffers"), api.DoctorStatusWarn, codeUDPBuffersLow, fixUDPBuffersSysctl)
	})
	t.Run("nothing to read", func(t *testing.T) {
		w := newWorld(t)
		w.live = nil
		delete(w.files, "proc/sys/net/core/rmem_max")
		want(t, w.check("udp_buffers"), api.DoctorStatusSkip, codeUDPBuffersUnknown, "")
		// A container that hides the sysctls still has the sockets of its running server.
		w.live = healthyStatus()
		c := w.check("udp_buffers")
		want(t, c, api.DoctorStatusOK, codeUDPBuffersOK, "")
		contains(t, c.Message, "the media sockets have 8388608 bytes to receive and 8388608 to send")
	})
	t.Run("not Linux", func(t *testing.T) {
		w := newWorld(t)
		w.env.GOOS = "darwin"
		w.live = nil
		c := w.check("udp_buffers")
		want(t, c, api.DoctorStatusSkip, codeSkipUnsupported, "")
		contains(t, c.Message, "not available on darwin")
		// With a server its sockets are checked, without a sysctl to name.
		w.live = healthyStatus()
		want(t, w.check("udp_buffers"), api.DoctorStatusOK, codeUDPBuffersOK, "")
		w.live.UDPRcvBufBytes = 65536
		want(t, w.check("udp_buffers"), api.DoctorStatusWarn, codeUDPBuffersLow, "")
	})
}

// container: the runtime, and the network mode from the default route's interface (a veth end in a bridge
// network).
func TestCheckContainer(t *testing.T) {
	w := newWorld(t, "--tls.mode", "ip")
	want(t, w.check("container"), api.DoctorStatusOK, codeContainerNone, "")

	bridge := func(w *world) {
		w.files["sys/class/net/eth0/iflink"].Data = []byte("17\n")
		w.live.LocalIPv4, w.live.PublicIPv4Method, w.live.NAT = "172.18.0.2", "stun", api.NATKindOneToOne
		w.env.Interfaces = &fakeIfaces{ifs: []netx.Interface{iface("lo", "127.0.0.1"), iface("eth0", "172.18.0.2")}, route: "172.18.0.2"}
	}
	w.container("docker", true)
	bridge(w)
	rep := Run(t.Context(), w.build(), api.BandwidthInput{}, []string{"container"})
	if rep.Env.Container != api.ContainerKindDocker {
		t.Errorf("env.container = %q", rep.Env.Container)
	}
	c := rep.Checks[0]
	want(t, c, api.DoctorStatusInfo, codeContainerDetected, "")
	if want := "Docker, bridge network: publish TCP 80, 443, 7882 and UDP 7882 under the same numbers (443:443, " +
		"7882:7882/udp), because isshoni tells browsers the container's ports"; c.Message != want {
		t.Errorf("message:\n got %q\nwant %q", c.Message, want)
	}

	// The host's network: the interface is a real one.
	w = newWorld(t, "--tls.mode", "ip")
	w.container("podman", true)
	c = w.check("container")
	want(t, c, api.DoctorStatusInfo, codeContainerDetected, "")
	contains(t, c.Message, "Podman, host network: isshoni uses the host's ports directly")

	// Offline the default route is asked of the kernel; without the interface's files the mode is unknown.
	w = newWorld(t, "--tls.mode", "ip")
	w.container("containerd", true)
	w.live = nil
	delete(w.files, "sys/class/net/eth0/iflink")
	c = w.check("container")
	want(t, c, api.DoctorStatusInfo, codeContainerDetected, "")
	if c.Params["kind"] != "other" || c.Params["network"] != networkUnknown {
		t.Errorf("params %v", c.Params)
	}
	contains(t, c.Message, "a container: ports published from a bridge network must keep their numbers")

	// A Docker bridge has no IPv6 by default, which public_ip says in its own words (04 §7.6).
	w = newWorld(t, "--tls.mode", "ip")
	w.container("docker", true)
	bridge(w)
	w.live.PublicIPv6 = ""
	pub := w.check("public_ip")
	want(t, pub, api.DoctorStatusInfo, codePublicIPNoIPv6, "")
	contains(t, pub.Message, "; IPv6 media off (Docker bridge)")
}

func TestCheckLANPrivacy(t *testing.T) {
	w := newWorld(t)
	want(t, w.check("lan_privacy"), api.DoctorStatusOK, codeLANPrivacyNotMacOS, "")
	w.env.GOOS = "darwin"
	c := w.check("lan_privacy")
	want(t, c, api.DoctorStatusInfo, codeLANPrivacyMacOS, "")
	contains(t, c.Message, "Local Network permission")
}

func TestCheckTransfer(t *testing.T) {
	tests := []struct {
		name    string
		t       *api.TransferInfo
		status  api.DoctorStatus
		code    string
		fix     string
		message string
	}{
		{"no limit", &api.TransferInfo{Month: "2026-09", EgressBytes: 12_300_000_000}, api.DoctorStatusOK, codeTransferNoLimit, "",
			"12.3 GB out in 2026-09; no alert limit set"},
		{"below", &api.TransferInfo{Month: "2026-09", EgressBytes: 799_999_999_999, AlertGB: 1000}, api.DoctorStatusOK, codeTransferOK, "",
			"800.0 GB of 1000 GB out in 2026-09 (79 %)"},
		{"80 %", &api.TransferInfo{Month: "2026-09", EgressBytes: 800_000_000_000, AlertGB: 1000, ProjectedEgressBytes: 1_100_000_000_000},
			api.DoctorStatusWarn, codeTransferHigh, fixTransferReview,
			"800.0 GB of 1000 GB out in 2026-09 (80 %); at this rate the month ends at 1100.0 GB"},
		{"over", &api.TransferInfo{Month: "2026-09", EgressBytes: 1_250_000_000_000, AlertGB: 1000}, api.DoctorStatusWarn, codeTransferOver, fixTransferReview,
			"1250.0 GB of 1000 GB out in 2026-09 (125 %)"},
		{"not reported", nil, api.DoctorStatusSkip, codeSkipNotReported, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.live.Transfer = tt.t
			c := w.check("transfer")
			want(t, c, tt.status, tt.code, tt.fix)
			if tt.message != "" && c.Message != tt.message {
				t.Errorf("message %q, want %q", c.Message, tt.message)
			}
		})
	}
}

func TestCheckRelease(t *testing.T) {
	const url = "https://github.com/MoonWX/isshoni/releases/tag/v0.3.1"
	tests := []struct {
		name    string
		running string
		update  *api.UpdateInfo
		status  api.DoctorStatus
		code    string
		fix     string
		message string
	}{
		{"the latest", "0.3.0", &api.UpdateInfo{Latest: "0.3.0"}, api.DoctorStatusOK, codeReleaseOK, "", "0.3.0 is the latest release"},
		{"ahead of the feed", "0.4.0-rc.1", &api.UpdateInfo{Latest: "0.3.0"}, api.DoctorStatusOK, codeReleaseOK, "", ""},
		{"a newer release", "0.3.0", &api.UpdateInfo{Latest: "0.3.1", URL: url}, api.DoctorStatusWarn, codeReleaseUpdate, fixReleaseUpgrade,
			"isshoni 0.3.1 is available (this is 0.3.0)"},
		{"a security release", "0.3.0", &api.UpdateInfo{Latest: "0.3.1", URL: url, Security: true}, api.DoctorStatusWarn,
			codeReleaseSecurityUpdate, fixReleaseUpgrade, "isshoni 0.3.1 is available and fixes a security problem (this is 0.3.0)"},
		{"0.10 is newer than 0.9", "0.9.0", &api.UpdateInfo{Latest: "0.10.0"}, api.DoctorStatusWarn, codeReleaseUpdate, fixReleaseUpgrade, ""},
		{"no check yet, or switched off", "0.3.0", nil, api.DoctorStatusOK, codeReleaseNotChecked, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.live.Version, w.live.Update = tt.running, tt.update
			c := w.check("release")
			want(t, c, tt.status, tt.code, tt.fix)
			if tt.message != "" && c.Message != tt.message {
				t.Errorf("message %q, want %q", c.Message, tt.message)
			}
		})
	}
	w := newWorld(t)
	w.live.Update = &api.UpdateInfo{Latest: "0.3.1", URL: url}
	if c := w.check("release"); c.Fix != "run the installer again to upgrade\n"+url {
		t.Errorf("fix %q", c.Fix)
	}
	w.container("docker", true)
	contains(t, w.check("release").Fix, "docker compose pull && docker compose up -d")
}

func TestCheckNofile(t *testing.T) {
	w := newWorld(t)
	c := w.check("nofile")
	want(t, c, api.DoctorStatusInfo, codeNofileOK, "")
	if c.Message != "open-file limit 65536" {
		t.Errorf("message %q", c.Message)
	}
	w.env.NoFile = func() (uint64, error) { return 8192, nil }
	want(t, w.check("nofile"), api.DoctorStatusInfo, codeNofileOK, "")
	w.env.NoFile = func() (uint64, error) { return 1024, nil }
	c = w.check("nofile")
	want(t, c, api.DoctorStatusWarn, codeNofileLow, fixNofileLimit)
	contains(t, c.Message, "open-file limit 1024, isshoni wants 8192")
	contains(t, c.Fix, "LimitNOFILE")
	w.container("docker", true)
	contains(t, w.check("nofile").Fix, "ulimits")
	w.env.NoFile = func() (uint64, error) { return 0, errors.ErrUnsupported }
	want(t, w.check("nofile"), api.DoctorStatusSkip, codeSkipUnsupported, "")
	w.env.NoFile = func() (uint64, error) { return 1<<63 + 5, nil } // "unlimited"
	want(t, w.check("nofile"), api.DoctorStatusInfo, codeNofileOK, "")
}

// The real probes answer on the platforms the server runs on.
func TestRealProbes(t *testing.T) {
	if !unixPerms {
		t.Skip("no probes on this platform")
	}
	if n, err := noFile(); err != nil || n == 0 {
		t.Errorf("noFile = %d, %v", n, err)
	}
	dir := t.TempDir()
	if free, err := diskFree(dir); err != nil || free == 0 {
		t.Errorf("diskFree = %d, %v", free, err)
	}
	if err := writable(dir); err != nil {
		t.Errorf("writable(%s): %v", dir, err)
	}
	if kernelRelease() == "" {
		t.Error("kernelRelease is empty")
	}
	if got := userName(1 << 30); got != "uid 1073741824" {
		t.Errorf("userName of a uid nobody has: %q", got)
	}
}

// bandwidth: the estimate, and a warning when it comes near the speed of the network interface.
func TestCheckBandwidth(t *testing.T) {
	w := newWorld(t)
	c := w.check("bandwidth")
	want(t, c, api.DoctorStatusInfo, codeBandwidthEstimate, "")
	if want := "5 people, 2 sharing: ~41.5 Mbps egress (~44 on the wire), ~39 GB per 2-hour session"; c.Message != want {
		t.Errorf("message %q, want %q", c.Message, want)
	}
	if c.Params["nicMbps"] != int64(1000) || c.Params["iface"] != "eth0" {
		t.Errorf("params %v", c.Params)
	}

	// 43.62 Mbps on the wire against a 50 Mbps interface is more than 80 %.
	w.files["sys/class/net/eth0/speed"].Data = []byte("50\n")
	c = w.check("bandwidth")
	want(t, c, api.DoctorStatusWarn, codeBandwidthNearNIC, fixBandwidthLink)
	contains(t, c.Message, "that is more than 80 % of eth0's 50 Mbps")
	w.files["sys/class/net/eth0/speed"].Data = []byte("100\n")
	want(t, w.check("bandwidth"), api.DoctorStatusInfo, codeBandwidthEstimate, "")

	// A virtual interface reports no speed (-1, or an error): nothing to compare with.
	w.files["sys/class/net/eth0/speed"].Data = []byte("-1\n")
	c = w.check("bandwidth")
	want(t, c, api.DoctorStatusInfo, codeBandwidthEstimate, "")
	if _, ok := c.Params["nicMbps"]; ok {
		t.Errorf("params %v", c.Params)
	}
	delete(w.files, "sys/class/net/eth0/speed")
	want(t, w.check("bandwidth"), api.DoctorStatusInfo, codeBandwidthEstimate, "")

	// Offline the interface is the default route's, and no STUN server is asked for an estimate.
	w = newWorld(t, "--network.stun-servers", "stun.example.net:3478")
	w.live = nil
	stun := &fakeSTUN{}
	w.env.STUN = stun
	w.files["sys/class/net/eth0/speed"].Data = []byte("50\n")
	want(t, w.check("bandwidth"), api.DoctorStatusWarn, codeBandwidthNearNIC, fixBandwidthLink)
	if stun.calls != 0 {
		t.Error("the bandwidth check asked a STUN server")
	}
}
