package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// cmdLanCheck tests the macOS Local Network permission (15+). It sends one UDP
// datagram to the default gateway's discard port (9): while the app is not
// allowed, macOS fails the send (EPIPE on macOS 27; EHOSTUNREACH is also
// reported for this case).
// (Connecting to this Mac's own LAN address is not gated, so it can't be used.)
// The first run shows the prompt; later builds signed with the same identity
// must pass without one.
func cmdLanCheck(args []string) error {
	gw, err := defaultGateway()
	if err != nil {
		return err
	}
	start := time.Now()
	for attempt := 1; ; attempt++ {
		err := sendUDP(gw + ":9")
		if err == nil {
			fmt.Printf("local network: ALLOWED (UDP to the gateway %s went out, after %.1f s, attempt %d, build %s)\n",
				gw, time.Since(start).Seconds(), attempt, bundleVersion())
			return nil
		}
		if !errors.Is(err, syscall.EHOSTUNREACH) && !errors.Is(err, syscall.EPIPE) {
			return fmt.Errorf("unexpected send error: %w", err)
		}
		if time.Since(start) > 60*time.Second {
			fmt.Printf("local network: BLOCKED (%v)\n", err)
			return exitCode(3)
		}
		if attempt == 1 {
			fmt.Printf("blocked (%v): if macOS asks to find devices on your local network, click Allow\n", err)
		}
		time.Sleep(2 * time.Second)
	}
}

func sendUDP(addr string) error {
	c, err := net.Dial("udp4", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte("isshoni local network check"))
	return err
}

func defaultGateway() (string, error) {
	out, err := exec.Command("/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "gateway:" {
			if ip := net.ParseIP(f[1]); ip != nil && ip.To4() != nil && ip.IsPrivate() {
				return f[1], nil
			}
			return "", fmt.Errorf("default gateway %s is not a private IPv4 address (VPN/proxy?)", f[1])
		}
	}
	return "", errors.New("no default gateway")
}

// bundleVersion reads CFBundleVersion of the running app (for logs).
func bundleVersion() string {
	out, err := exec.Command("/usr/bin/defaults", "read", appInfoPlist(), "CFBundleVersion").Output()
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

func appInfoPlist() string {
	exe, _ := osExecutable()
	if i := strings.Index(exe, ".app/Contents/MacOS/"); i >= 0 {
		return exe[:i+4] + "/Contents/Info"
	}
	return ""
}

var osExecutable = os.Executable
