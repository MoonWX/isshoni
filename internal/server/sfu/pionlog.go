package sfu

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/pion/logging"
)

// Pion's own lines in the SFU's log (02 §13). They come through logx's bridge (04 §10) under component=sfu, with a
// scope attribute: logx keeps Pion's levels, logs Pion's trace, debug and info lines only while the logger is enabled
// for debug, holds each scope to 20 lines a minute and drops every line that looks like SDP or a candidate. The SFU
// changes two things on top, so that what it logs at info and above is its own lines with ids and states, and Pion's
// warnings and errors, and never an address (02 §13: never ICE candidates or client IPs above debug):
//
//   - Pion's info lines are debug lines. A PeerConnection makes about ten of them as it connects and closes
//     ("ICE connection state changed: checking", "peer connection state changed: connected", the DTLS and SCTP
//     steps). They repeat, without its ids, what the SFU says itself at info: the PC states with their transport.
//   - A warning or an error that holds an IP address is a debug line too. Pion's ICE agent names the remote
//     address when it discards a check ("Discard request with wrong username from (…)", "Failed to find pair for
//     add binding request from … to …"): the address is a client's, and such lines come from stray packets, as
//     netx's muxes find for theirs (04 §7.3). Warnings without an address keep their level, those Pion makes when a
//     PeerConnection closes included ("Failed to accept RTP stream is already closed", "Incoming unhandled RTCP
//     ssrc(…)").

// pionLogs is the SFU's Pion logger factory: logx's bridge with the two changes above. newAPIs builds one and shares
// it between all of the SFU's APIs.
type pionLogs struct{ logging.LoggerFactory }

// NewLogger returns the logger of one Pion scope.
func (f pionLogs) NewLogger(scope string) logging.LeveledLogger {
	return pionLogger{f.LoggerFactory.NewLogger(scope)}
}

// pionLogger is one scope's logger of pionLogs.
type pionLogger struct{ logging.LeveledLogger }

func (l pionLogger) Info(msg string)                  { l.Debug(msg) }
func (l pionLogger) Infof(format string, args ...any) { l.Debugf(format, args...) }

func (l pionLogger) Warn(msg string) {
	if holdsAddress(msg) {
		l.Debug(msg)
		return
	}
	l.LeveledLogger.Warn(msg)
}

func (l pionLogger) Warnf(format string, args ...any) { l.Warn(fmt.Sprintf(format, args...)) }

func (l pionLogger) Error(msg string) {
	if holdsAddress(msg) {
		l.Debug(msg)
		return
	}
	l.LeveledLogger.Error(msg)
}

func (l pionLogger) Errorf(format string, args ...any) { l.Error(fmt.Sprintf(format, args...)) }

// ipv4RE matches what may be an IPv4 address in dotted-quad form, wherever it stands ("remote:192.0.2.1").
var ipv4RE = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}`)

// holdsAddress reports whether s holds an IP address, with or without a port or a zone: a dotted quad that parses as
// an IPv4 address, or a run of the characters an IPv6 address can be written with that parses as one, as it is or
// without the dots and colons around it (the end of a sentence).
func holdsAddress(s string) bool {
	for _, quad := range ipv4RE.FindAllString(s, -1) {
		if _, err := netip.ParseAddr(quad); err == nil {
			return true
		}
	}
	for _, field := range strings.FieldsFunc(s, func(r rune) bool { return !addressRune(r) }) {
		if !strings.Contains(field, ":") {
			continue // no IPv6 address, and the IPv4 ones are found above
		}
		for _, f := range [...]string{
			field, strings.TrimRight(field, ".:"), strings.TrimLeft(field, ".:"), strings.Trim(field, ".:"),
		} {
			if isAddress(f) {
				return true
			}
		}
	}
	return false
}

// isAddress reports whether s is an IP address, or an address and port, as netip reads them. A zone that the field
// cut short (its letters past f aren't address characters) still counts.
func isAddress(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	if _, err := netip.ParseAddrPort(s); err == nil {
		return true
	}
	if i := strings.IndexByte(s, '%'); i > 0 {
		_, err := netip.ParseAddr(s[:i])
		return err == nil
	}
	return false
}

// addressRune reports whether r can be part of a written IP address with a port: hex digits, the separators, the
// brackets around an IPv6 address with a port, and the % of a zone.
func addressRune(r rune) bool {
	switch {
	case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		return true
	}
	return strings.ContainsRune(".:[]%", r)
}
