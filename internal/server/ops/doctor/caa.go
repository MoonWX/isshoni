package doctor

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// CAA lookups for the dns check (04 §13.2): does a CAA record keep Let's Encrypt from issuing the certificate?
// Go's resolver has no CAA query, so this file sends one itself, to the machine's own recursive resolvers
// (/etc/resolv.conf), the same ones the system resolver asks. It is a best effort: when no resolver answers, the
// check says so in its params and leaves CAA to the CA, which reports a refusal as tls.caa_forbids.

// CAARecord is one CAA resource record (RFC 8659).
type CAARecord struct {
	Name     string // the name that holds the record: the domain itself or a parent
	Critical bool   // the issuer critical flag: a CA that does not know Tag must not issue
	Tag      string // "issue", "issuewild", "iodef", …, in lower case
	Value    string
}

// caaAllows reports whether the CAA records of a name's relevant RRset let the CA with the issuer domain name
// issuer issue a certificate for that name (not a wildcard): RFC 8659 §4. No "issue" property means no
// restriction; with some, one of them must name the issuer. A critical property that a CA would not understand
// forbids issuance.
func caaAllows(recs []CAARecord, issuer string) bool {
	restricted, named := false, false
	for _, rec := range recs {
		switch strings.ToLower(rec.Tag) {
		case "issue":
			restricted = true
			name, _, _ := strings.Cut(rec.Value, ";")
			if strings.EqualFold(strings.TrimSpace(name), issuer) {
				named = true
			}
		case "issuewild", "iodef":
		default:
			if rec.Critical {
				return false
			}
		}
	}
	return named || !restricted
}

const (
	typeCAA         = dnsmessage.Type(257)
	caaQueryTimeout = 2 * time.Second
	caaMaxServers   = 3   // as the system resolver: the first three nameservers
	caaMaxClimb     = 10  // labels to climb from the domain towards the root
	dnsMaxUDP       = 512 // without EDNS a UDP answer is at most this long
	dnsPort         = "53"
)

// caaClient asks recursive resolvers for CAA records.
type caaClient struct {
	fs fs.FS // the machine's filesystem, for etc/resolv.conf
	// servers replaces the resolvers of resolv.conf ("host:port"); tests set it.
	servers []string
}

// lookup returns the CAA records that apply to name: its own, or those of the closest parent that has any
// (RFC 8659 §3 climbs the tree). No records at all is an empty result. The error is non-nil when no resolver gave
// an answer.
func (c *caaClient) lookup(ctx context.Context, name string) ([]CAARecord, error) {
	servers := c.servers
	if servers == nil {
		var err error
		if servers, err = c.resolvers(); err != nil {
			return nil, err
		}
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for range caaMaxClimb {
		if name == "" {
			break
		}
		recs, err := queryCAA(ctx, servers, name)
		if err != nil {
			return nil, err
		}
		if len(recs) > 0 {
			return recs, nil
		}
		_, parent, ok := strings.Cut(name, ".")
		if !ok {
			break
		}
		name = parent
	}
	return nil, nil
}

// resolvers returns the nameservers of /etc/resolv.conf as "host:port".
func (c *caaClient) resolvers() ([]string, error) {
	f, err := c.fs.Open("etc/resolv.conf")
	if err != nil {
		return nil, fmt.Errorf("doctor: no resolver for CAA: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() && len(out) < caaMaxServers {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			out = append(out, net.JoinHostPort(fields[1], dnsPort))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("doctor: no resolver for CAA: /etc/resolv.conf names no nameserver")
	}
	return out, nil
}

// queryCAA asks the servers in turn for the CAA records of name and returns the first usable answer: the records,
// or none when the name has none or does not exist.
func queryCAA(ctx context.Context, servers []string, name string) ([]CAARecord, error) {
	qname, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return nil, fmt.Errorf("doctor: CAA query for %q: %w", name, err)
	}
	var id [2]byte
	_, _ = rand.Read(id[:]) // never fails (crypto/rand)
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(dnsmessage.Question{Name: qname, Type: typeCAA, Class: dnsmessage.ClassINET}); err != nil {
		return nil, err
	}
	query, err := b.Finish()
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, server := range servers {
		answer, err := exchangeDNS(ctx, server, query)
		if err != nil {
			lastErr = err
			continue
		}
		recs, err := parseCAA(answer, binary.BigEndian.Uint16(id[:]), name)
		if err != nil {
			lastErr = err
			continue
		}
		return recs, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no resolver")
	}
	return nil, fmt.Errorf("doctor: CAA query for %s: %w", name, lastErr)
}

// exchangeDNS sends query to server over UDP and returns the answer; an answer cut short (TC) is asked again over
// TCP.
func exchangeDNS(ctx context.Context, server string, query []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, caaQueryTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(deadline)
	_, err = conn.Write(query)
	buf := make([]byte, dnsMaxUDP)
	n := 0
	if err == nil {
		n, err = conn.Read(buf)
	}
	_ = conn.Close()
	if err != nil {
		return nil, err
	}
	var p dnsmessage.Parser
	h, err := p.Start(buf[:n])
	if err != nil {
		return nil, err
	}
	if !h.Truncated {
		return buf[:n], nil
	}

	conn, err = d.DialContext(ctx, "tcp", server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)
	// RFC 1035 §4.2.2: over TCP a message is preceded by its length in two bytes.
	framed := binary.BigEndian.AppendUint16(make([]byte, 0, len(query)+2), uint16(len(query))) //nolint:gosec // G115: a query is a few dozen bytes
	if _, err := conn.Write(append(framed, query...)); err != nil {
		return nil, err
	}
	var length [2]byte
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		return nil, err
	}
	answer := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(conn, answer); err != nil {
		return nil, err
	}
	return answer, nil
}

// parseCAA reads the CAA records out of an answer to the query with this id. A name without records, or one that
// does not exist, is an empty result; a resolver that could not find out (SERVFAIL, REFUSED) is an error.
func parseCAA(msg []byte, id uint16, name string) ([]CAARecord, error) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return nil, err
	}
	switch {
	case !h.Response || h.ID != id:
		return nil, errors.New("not the answer to this query")
	case h.RCode == dnsmessage.RCodeNameError:
		return nil, nil
	case h.RCode != dnsmessage.RCodeSuccess:
		return nil, fmt.Errorf("the resolver answered %s", h.RCode)
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, err
	}
	var out []CAARecord
	for {
		rh, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if rh.Type != typeCAA {
			if err := p.SkipAnswer(); err != nil { // a CNAME on the way to the records
				return nil, err
			}
			continue
		}
		res, err := p.UnknownResource()
		if err != nil {
			return nil, err
		}
		// RFC 8659 §4.1: flags (1 byte), tag length (1 byte), tag, value.
		data := res.Data
		if len(data) < 2 || int(data[1]) > len(data)-2 || data[1] == 0 {
			continue
		}
		tagEnd := 2 + int(data[1])
		out = append(out, CAARecord{
			Name:     name,
			Critical: data[0]&0x80 != 0,
			Tag:      strings.ToLower(string(data[2:tagEnd])),
			Value:    string(data[tagEnd:]),
		})
	}
}
