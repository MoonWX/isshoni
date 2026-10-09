package doctor

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS is a recursive resolver on a loopback UDP (and TCP) socket of the test. It answers CAA queries from a
// table by name; a name that is not in the table gets an empty answer.
type fakeDNS struct {
	t    *testing.T
	udp  net.PacketConn
	tcp  net.Listener
	wg   sync.WaitGroup
	addr string

	mu       sync.Mutex
	records  map[string][]caaRR // by queried name
	rcode    map[string]dnsmessage.RCode
	truncate bool // answer over UDP with TC set and no records
	asked    []string
	overTCP  int
}

// caaRR is one CAA record of the fake zone.
type caaRR struct {
	flags      byte
	tag, value string
}

func startFakeDNS(t *testing.T) *fakeDNS {
	t.Helper()
	var lc net.ListenConfig
	udp, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := lc.Listen(t.Context(), "tcp", udp.LocalAddr().String())
	if err != nil {
		_ = udp.Close()
		t.Skipf("the TCP port next to the UDP one is taken: %v", err)
	}
	f := &fakeDNS{t: t, udp: udp, tcp: tcp, addr: udp.LocalAddr().String(), records: map[string][]caaRR{}, rcode: map[string]dnsmessage.RCode{}}
	f.wg.Go(f.serveUDP)
	f.wg.Go(f.serveTCP)
	t.Cleanup(func() {
		_ = udp.Close()
		_ = tcp.Close()
		f.wg.Wait()
	})
	return f
}

func (f *fakeDNS) serveUDP() {
	buf := make([]byte, 1500)
	for {
		n, from, err := f.udp.ReadFrom(buf)
		if err != nil {
			return
		}
		if answer := f.answer(buf[:n], false); answer != nil {
			_, _ = f.udp.WriteTo(answer, from)
		}
	}
}

func (f *fakeDNS) serveTCP() {
	for {
		conn, err := f.tcp.Accept()
		if err != nil {
			return
		}
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err == nil {
			query := make([]byte, binary.BigEndian.Uint16(length[:]))
			if _, err := io.ReadFull(conn, query); err == nil {
				if answer := f.answer(query, true); answer != nil {
					framed := binary.BigEndian.AppendUint16(nil, uint16(len(answer))) //nolint:gosec // G115: a test answer
					_, _ = conn.Write(append(framed, answer...))
				}
			}
		}
		_ = conn.Close()
	}
}

// answer builds the response to one query.
func (f *fakeDNS) answer(query []byte, tcp bool) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	name := strings.TrimSuffix(q.Name.String(), ".")
	f.mu.Lock()
	defer f.mu.Unlock()
	if tcp {
		f.overTCP++
	} else {
		f.asked = append(f.asked, name)
	}
	if q.Type != typeCAA || !h.RecursionDesired {
		f.t.Errorf("query for %s: type %v, recursion desired %v", name, q.Type, h.RecursionDesired)
	}
	rh := dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true, RCode: f.rcode[name]}
	if f.truncate && !tcp {
		rh.Truncated = true
	}
	b := dnsmessage.NewBuilder(nil, rh)
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	if !rh.Truncated {
		for _, rr := range f.records[name] {
			data := append([]byte{rr.flags, byte(len(rr.tag))}, rr.tag...) //nolint:gosec // G115: a tag of a few letters
			data = append(data, rr.value...)
			_ = b.UnknownResource(dnsmessage.ResourceHeader{Name: q.Name, Type: typeCAA, Class: dnsmessage.ClassINET, TTL: 300},
				dnsmessage.UnknownResource{Type: typeCAA, Data: data})
		}
	}
	out, err := b.Finish()
	if err != nil {
		f.t.Errorf("building the answer for %s: %v", name, err)
		return nil
	}
	return out
}

// zone sets the CAA records of a name; rcodeFor the answer code of one; both under the lock, since the server reads
// them while a test goes on.
func (f *fakeDNS) zone(name string, records ...caaRR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[name] = records
}

func (f *fakeDNS) rcodeFor(name string, code dnsmessage.RCode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rcode[name] = code
}

func (f *fakeDNS) truncateUDP() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.truncate = true
}

func (f *fakeDNS) queried() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// The CAA lookup asks the machine's resolver, climbs from the name to its parents until one has records
// (RFC 8659 §3), and reads flags, tag and value.
func TestCAALookup(t *testing.T) {
	dns := startFakeDNS(t)
	dns.zone("example.com",
		caaRR{0, "issue", "letsencrypt.org; validationmethods=http-01"},
		caaRR{0x80, "ISSUEWILD", ";"},
		caaRR{0, "iodef", "mailto:security@example.com"})
	dns.rcodeFor("deep.watch.example.com", dnsmessage.RCodeNameError) // a name that does not exist still has parents
	client := &caaClient{servers: []string{dns.addr}}

	recs, err := client.lookup(t.Context(), "Deep.Watch.Example.COM.")
	if err != nil {
		t.Fatal(err)
	}
	want := []CAARecord{
		{Name: "example.com", Tag: "issue", Value: "letsencrypt.org; validationmethods=http-01"},
		{Name: "example.com", Critical: true, Tag: "issuewild", Value: ";"},
		{Name: "example.com", Tag: "iodef", Value: "mailto:security@example.com"},
	}
	if !slices.Equal(recs, want) {
		t.Errorf("records:\n got %+v\nwant %+v", recs, want)
	}
	if got := dns.queried(); !slices.Equal(got, []string{"deep.watch.example.com", "watch.example.com", "example.com"}) {
		t.Errorf("queried %v", got)
	}
	if !caaAllows(recs, letsEncryptIssuer) {
		t.Error("these records allow Let's Encrypt")
	}

	// No records anywhere up to the top: no restriction.
	recs, err = client.lookup(t.Context(), "watch.example.org")
	if err != nil || len(recs) != 0 {
		t.Errorf("a domain without CAA records: %v, %v", recs, err)
	}

	// The resolver could not find out: an error, not "no records".
	dns.rcodeFor("broken.example.net", dnsmessage.RCodeServerFailure)
	if _, err := client.lookup(t.Context(), "broken.example.net"); err == nil {
		t.Error("SERVFAIL was read as an answer")
	}
}

// An answer that does not fit into a UDP datagram is asked again over TCP.
func TestCAALookupTruncated(t *testing.T) {
	dns := startFakeDNS(t)
	dns.zone("example.com", caaRR{0, "issue", "digicert.com"})
	dns.truncateUDP()
	recs, err := (&caaClient{servers: []string{dns.addr}}).lookup(t.Context(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Value != "digicert.com" || caaAllows(recs, letsEncryptIssuer) {
		t.Errorf("records %+v", recs)
	}
	dns.mu.Lock()
	defer dns.mu.Unlock()
	if dns.overTCP != 1 {
		t.Errorf("%d queries over TCP, want 1", dns.overTCP)
	}
}

// The resolvers come from /etc/resolv.conf: the first three nameservers, and the next one is asked when one does
// not answer.
func TestCAAResolvers(t *testing.T) {
	dns := startFakeDNS(t)
	dns.zone("example.com", caaRR{0, "issue", "letsencrypt.org"})
	host, port, _ := net.SplitHostPort(dns.addr)

	conf := "# made by a test\nsearch example.com\nnameserver 192.0.2.1\nnameserver 2001:db8::53\n\nnameserver " + host +
		"\nnameserver 192.0.2.4\noptions edns0\n"
	client := &caaClient{fs: fstest.MapFS{"etc/resolv.conf": {Data: []byte(conf)}}}
	servers, err := client.resolvers()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"192.0.2.1:53", "[2001:db8::53]:53", host + ":53"}; !slices.Equal(servers, want) {
		t.Errorf("resolvers %v, want %v", servers, want)
	}

	// A dead resolver first: the query moves on to the one that answers. The dead one is a closed loopback port,
	// which refuses at once.
	deadConn, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := deadConn.LocalAddr().String()
	_ = deadConn.Close()
	recs, err := queryCAA(t.Context(), []string{dead, net.JoinHostPort(host, port)}, "example.com")
	if err != nil || len(recs) != 1 {
		t.Errorf("with a dead resolver first: %v, %v", recs, err)
	}
	// Nobody answers: an error.
	if _, err := queryCAA(t.Context(), []string{dead}, "example.com"); err == nil {
		t.Error("a query nobody answered returned no error")
	}

	// No resolv.conf, or one without a nameserver: the lookup says that it could not ask.
	for name, files := range map[string]fstest.MapFS{
		"no file":       {},
		"no nameserver": {"etc/resolv.conf": {Data: []byte("search example.com\n")}},
	} {
		if _, err := (&caaClient{fs: files}).lookup(t.Context(), "example.com"); err == nil {
			t.Errorf("%s: lookup returned no error", name)
		}
	}
	// A name that can't be a DNS name is an error too, not a panic.
	if _, err := (&caaClient{servers: []string{dns.addr}}).lookup(t.Context(), strings.Repeat("a", 300)+".example.com"); err == nil {
		t.Error("a name longer than DNS allows was queried")
	}
}

// What comes back from the network is parsed without trust: an answer to another query, a record that is cut
// short, bytes that are no DNS message.
func TestParseCAA(t *testing.T) {
	name := dnsmessage.MustNewName("example.com.")
	build := func(id uint16, response bool, records ...[]byte) []byte {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: response})
		_ = b.StartQuestions()
		_ = b.Question(dnsmessage.Question{Name: name, Type: typeCAA, Class: dnsmessage.ClassINET})
		_ = b.StartAnswers()
		_ = b.CNAMEResource(dnsmessage.ResourceHeader{Name: name, Class: dnsmessage.ClassINET},
			dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("alias.example.net.")})
		for _, data := range records {
			_ = b.UnknownResource(dnsmessage.ResourceHeader{Name: name, Type: typeCAA, Class: dnsmessage.ClassINET},
				dnsmessage.UnknownResource{Type: typeCAA, Data: data})
		}
		out, err := b.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	good := []byte("\x00\x05issueletsencrypt.org")

	recs, err := parseCAA(build(7, true, good, []byte{0}, []byte("\x00\x09issue"), []byte("\x00\x00")), 7, "example.com")
	if err != nil || len(recs) != 1 || recs[0].Tag != "issue" || recs[0].Value != "letsencrypt.org" {
		t.Errorf("a CNAME, one good record and three broken ones: %+v, %v", recs, err)
	}
	if _, err := parseCAA(build(8, true, good), 7, "example.com"); err == nil {
		t.Error("an answer with another id was accepted")
	}
	if _, err := parseCAA(build(7, false, good), 7, "example.com"); err == nil {
		t.Error("a query was accepted as an answer")
	}
	if _, err := parseCAA([]byte("not dns"), 7, "example.com"); err == nil {
		t.Error("garbage was accepted")
	}
	msg := build(7, true, good)
	if _, err := parseCAA(msg[:len(msg)-5], 7, "example.com"); err == nil {
		t.Error("a message cut short was accepted")
	}
}

// FuzzParseCAA: no input makes the parser panic.
func FuzzParseCAA(f *testing.F) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, Response: true})
	name := dnsmessage.MustNewName("example.com.")
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: name, Type: typeCAA, Class: dnsmessage.ClassINET})
	_ = b.StartAnswers()
	_ = b.UnknownResource(dnsmessage.ResourceHeader{Name: name, Type: typeCAA, Class: dnsmessage.ClassINET},
		dnsmessage.UnknownResource{Type: typeCAA, Data: []byte("\x80\x05issueletsencrypt.org")})
	seed, _ := b.Finish()
	f.Add(seed)
	f.Add([]byte{})
	f.Add([]byte("\x00\x07\x80\x00\x00\x01\x00\x01\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, msg []byte) {
		recs, err := parseCAA(msg, 7, "example.com")
		if err != nil && recs != nil {
			t.Fatalf("records %v next to the error %v", recs, err)
		}
		for _, r := range recs {
			if r.Tag == "" || r.Tag != strings.ToLower(r.Tag) || r.Name != "example.com" {
				t.Fatalf("record %+v", r)
			}
		}
		caaAllows(recs, letsEncryptIssuer)
	})
}

// A lookup stops with its context.
func TestCAALookupCancelled(t *testing.T) {
	dns := startFakeDNS(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := (&caaClient{servers: []string{dns.addr}}).lookup(ctx, "example.com")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("lookup with a cancelled context: %v", err)
	}
}
