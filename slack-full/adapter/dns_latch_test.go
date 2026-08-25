package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack/socketmode"
)

// Tests for the loopback-nameserver latch (gp-keg / ci-c3zl9): detector,
// resolv.conf self-parse, resolver pin, and the fast exit.

func loopbackDNSErr(server string) *net.DNSError {
	return &net.DNSError{
		Err:    "read udp " + server + "->" + server + ": read: connection refused",
		Name:   "slack.com",
		Server: server,
	}
}

func writeResolvConf(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func latchTestRunner(t *testing.T, resolvConf string, exitAfter int) (*socketModeRunner, *atomic.Int32) {
	t.Helper()
	cfg := socketTestConfig(t, "http://127.0.0.1:0")
	cfg.dnsLoopbackExitAfter = exitAfter
	r := newSocketModeRunner(cfg, nil, nil, nil)
	r.resolvConfPath = resolvConf
	var exits atomic.Int32
	r.exit = func(code int) {
		if code != 1 {
			t.Errorf("exit code %d, want 1", code)
		}
		exits.Add(1)
	}
	return r, &exits
}

// The detector trips only on Go's own fallback nameservers (net.defaultNS:
// 127.0.0.1:53 and [::1]:53) — the signature of a resolver that lost
// /etc/resolv.conf — never on a LAN resolver, a systemd-resolved stub
// (127.0.0.53), or a cgo-style error that names no server. Both the typed
// chain and slack-go's flattened text form must classify.
func TestIsLoopbackDNSServerError(t *testing.T) {
	wrapped := &url.Error{
		Op:  "Post",
		URL: "https://slack.com/api/apps.connections.open",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: loopbackDNSErr("[::1]:53")},
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed ::1", loopbackDNSErr("[::1]:53"), true},
		{"typed 127.0.0.1", loopbackDNSErr("127.0.0.1:53"), true},
		{"typed wrapped in url.Error/OpError", wrapped, true},
		{"typed systemd-resolved stub", loopbackDNSErr("127.0.0.53:53"), false},
		{"typed loopback on a non-DNS port", loopbackDNSErr("127.0.0.1:5353"), false},
		{"typed MagicDNS v6 (the 20:58 shape)", &net.DNSError{Err: "server misbehaving", Name: "slack.com", Server: "[fd7a:115c:a1e0::53]:53"}, false},
		{"typed LAN resolver", &net.DNSError{Err: "i/o timeout", Name: "slack.com", Server: "192.168.86.1:53", IsTimeout: true}, false},
		{"typed no server (cgo not-found)", &net.DNSError{Err: "no such host", Name: "slack.com", IsNotFound: true}, false},
		{"flattened ::1", errors.New(`apps.connections.open: Post "https://slack.com/api/apps.connections.open": dial tcp: lookup slack.com on [::1]:53: read udp [::1]:60000->[::1]:53: read: connection refused`), true},
		{"flattened 127.0.0.1", errors.New("lookup slack.com on 127.0.0.1:53: read udp 127.0.0.1:60000->127.0.0.1:53: read: connection refused"), true},
		{"flattened LAN resolver", errors.New("lookup slack.com on 192.168.86.1:53: read udp 192.168.86.2:60000->192.168.86.1:53: i/o timeout"), false},
		{"flattened no server", errors.New("lookup slack.com: no such host"), false},
		{"non-DNS", errors.New("dial tcp 3.1.2.3:443: i/o timeout"), false},
	}
	for _, tc := range cases {
		if got := isLoopbackDNSServerError(tc.err); got != tc.want {
			t.Errorf("%s: isLoopbackDNSServerError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The self-parse mirrors net's own: nameserver lines only, literal IPs
// only, capped at three, comments and other directives ignored; and the
// usable filter drops Go's fallback addresses in any spelling while
// keeping every other candidate.
func TestParseResolvConfNameservers(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"macOS configd", "#\n# macOS Notice\n#\nsearch lan\nnameserver 192.168.86.1\n", []string{"192.168.86.1:53"}},
		{"MagicDNS v4+v6", "nameserver 100.100.100.100\nnameserver fd7a:115c:a1e0::53\nsearch tail.ts.net\n", []string{"100.100.100.100:53", "[fd7a:115c:a1e0::53]:53"}},
		{"v6 zone", "nameserver fe80::1%en0\n", []string{"[fe80::1%en0]:53"}},
		{"comments, options, hostname skipped", "; comment\noptions ndots:2\nnameserver dns.example\nnameserver\t10.0.0.1  # trailing\n", []string{"10.0.0.1:53"}},
		{"capped at three", "nameserver 10.0.0.1\nnameserver 10.0.0.2\nnameserver 10.0.0.3\nnameserver 10.0.0.4\n", []string{"10.0.0.1:53", "10.0.0.2:53", "10.0.0.3:53"}},
		{"bare directive", "nameserver\n", nil},
		{"empty", "", nil},
		{"loopback only", "nameserver 127.0.0.1\n", []string{"127.0.0.1:53"}},
	}
	for _, tc := range cases {
		got, err := parseResolvConfNameservers(strings.NewReader(tc.body))
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: parse = %v, %v, want %v", tc.name, got, err, tc.want)
		}
	}
	// A scanner error (a line too long, a failing reader) is reported,
	// never passed off as "no nameserver".
	if got, err := parseResolvConfNameservers(strings.NewReader("nameserver 10.0.0.1\n# " + strings.Repeat("x", 128*1024) + "\n")); err == nil {
		t.Errorf("oversized line: got %v with no error", got)
	}

	got := usableNameservers([]string{"127.0.0.1:53", "[::1]:53", "[0:0:0:0:0:0:0:1]:53", "127.0.0.53:53", "192.168.86.1:53"})
	if want := []string{"127.0.0.53:53", "192.168.86.1:53"}; !reflect.DeepEqual(got, want) {
		t.Errorf("usableNameservers = %v, want %v", got, want)
	}
	if got := usableNameservers(nil); got != nil {
		t.Errorf("usableNameservers(nil) = %v, want nil", got)
	}

	path := writeResolvConf(t, "nameserver 192.0.2.1\n")
	if servers, err := readResolvConfNameservers(path); err != nil || !reflect.DeepEqual(servers, []string{"192.0.2.1:53"}) {
		t.Errorf("readResolvConfNameservers = %v, %v", servers, err)
	}
	if _, err := readResolvConfNameservers(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v, want ErrNotExist", err)
	}
}

// A latch event re-parses resolv.conf and pins the pure-Go resolver to a
// usable nameserver from it — flipping to the hooked resolver and
// rebuilding the client the first time, rotating across candidates on
// repeated events without a rebuild — and the Dial hook really sends DNS
// traffic to the pin. Connected clears the pin (fresh-resolve stays
// sticky); a non-loopback failure resets the streak but keeps the pin.
func TestLoopbackLatchPinsFreshNameserver(t *testing.T) {
	r, exits := latchTestRunner(t, writeResolvConf(t, "nameserver 127.0.0.1\nnameserver 192.0.2.1\nnameserver 192.0.2.2\n"), 5)
	if res := r.newNetDialer().Resolver; res != nil {
		t.Fatalf("fresh runner dials with resolver %v, want nil (process default)", res)
	}

	cycleCtx, cancel := context.WithCancel(context.Background())
	r.setCycleCancel(cancel)
	r.noteConnectionError(loopbackDNSErr("[::1]:53"), 0)
	if !r.freshResolve.Load() {
		t.Fatal("latch event did not flip to the pure-Go resolver (the Dial hook lives there)")
	}
	if cycleCtx.Err() == nil {
		t.Fatal("client cycle not rebuilt on the first latch event (it was dialing with the process-default resolver)")
	}
	res := r.newNetDialer().Resolver
	if res == nil || !res.PreferGo || res.Dial == nil {
		t.Fatalf("dialer resolver after latch = %+v, want PreferGo with the Dial hook", res)
	}
	if pin := r.pinnedNameserver(); pin != "192.0.2.1:53" {
		t.Fatalf("pin = %q, want the first usable nameserver 192.0.2.1:53 (127.0.0.1 skipped)", pin)
	}
	if h := r.healthzDetail(); !strings.Contains(h, "socket_dns_pin=192.0.2.1:53") || !strings.Contains(h, "socket_dns_loopback_streak=1") {
		t.Fatalf("healthz missing pin/streak: %q", h)
	}

	cycleCtx2, cancel2 := context.WithCancel(context.Background())
	r.setCycleCancel(cancel2)
	r.noteConnectionError(loopbackDNSErr("127.0.0.1:53"), 0)
	if pin := r.pinnedNameserver(); pin != "192.0.2.2:53" {
		t.Fatalf("pin after second event = %q, want rotation to 192.0.2.2:53", pin)
	}
	if cycleCtx2.Err() != nil {
		t.Fatal("client cycle rebuilt on a repeat event (the hook reads the pin at dial time)")
	}
	r.noteConnectionError(loopbackDNSErr("[::1]:53"), 0)
	if pin := r.pinnedNameserver(); pin != "192.0.2.1:53" {
		t.Fatalf("pin after third event = %q, want rotation back to 192.0.2.1:53", pin)
	}
	if exits.Load() != 0 {
		t.Fatalf("exit requested after 3 events with a pin in place, want only at 5")
	}

	// The Dial hook redirects to the pin — a local UDP listener stands in
	// for the nameserver — and passes the address through without one.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	addr := pc.LocalAddr().String()
	r.dnsPin.Store(&addr)
	ctx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()
	conn, err := res.Dial(ctx, "udp", "[::1]:53")
	if err != nil {
		t.Fatalf("hooked dial: %v", err)
	}
	if got := conn.RemoteAddr().String(); got != addr {
		t.Fatalf("hooked dial went to %s, want the pin %s", got, addr)
	}
	conn.Close()
	r.dnsPin.Store(nil)
	conn, err = res.Dial(ctx, "udp", addr)
	if err != nil {
		t.Fatalf("pass-through dial: %v", err)
	}
	if got := conn.RemoteAddr().String(); got != addr {
		t.Fatalf("pass-through dial went to %s, want %s", got, addr)
	}
	conn.Close()

	// Streak vs pin lifecycle.
	r.dnsPin.Store(&addr)
	r.noteConnectionError(errors.New("dial tcp 3.1.2.3:443: i/o timeout"), 0)
	if r.loopbackDNSStreak.Load() != 0 || r.pinnedNameserver() != addr {
		t.Fatalf("after a non-loopback failure: streak=%d pin=%q, want 0 and the pin kept", r.loopbackDNSStreak.Load(), r.pinnedNameserver())
	}
	r.noteConnectionError(loopbackDNSErr("[::1]:53"), 0)
	r.handleClientEvent(context.Background(), &recordingAcker{}, socketmode.Event{Type: socketmode.EventTypeConnected})
	if r.loopbackDNSStreak.Load() != 0 || r.pinnedNameserver() != "" {
		t.Fatalf("after Connected: streak=%d pin=%q, want 0 and no pin", r.loopbackDNSStreak.Load(), r.pinnedNameserver())
	}
	if !r.freshResolve.Load() {
		t.Fatal("fresh-resolve did not stay sticky across Connected")
	}
	if strings.Contains(r.healthzDetail(), "socket_dns_pin=") {
		t.Fatalf("healthz still reports a pin after Connected: %q", r.healthzDetail())
	}
}

// The exit fires only for a signature that persists N attempts WITH a pin
// in place — once per process, sharing the gp-bsk latch, never gated on
// everConnected, disabled by a zero knob (the pin still applies). Nothing
// to pin (unreadable file, no nameserver line, only Go's own fallback
// addresses) never exits: every attempt re-parses instead, and the pin
// applies the moment the file becomes usable.
func TestLoopbackLatchExit(t *testing.T) {
	loop := loopbackDNSErr("[::1]:53")
	missing := filepath.Join(t.TempDir(), "no-resolv.conf")

	r, exits := latchTestRunner(t, missing, 5)
	for i := 0; i < 10; i++ {
		r.noteConnectionError(loop, 0)
	}
	if exits.Load() != 0 || r.pinnedNameserver() != "" || r.freshResolve.Load() {
		t.Fatalf("unreadable resolv.conf: exits=%d pin=%q fresh=%v, want no exit and no pin (a fresh process would latch on the same file)", exits.Load(), r.pinnedNameserver(), r.freshResolve.Load())
	}
	if r.loopbackDNSStreak.Load() != 10 {
		t.Fatalf("streak = %d, want 10 (still counting)", r.loopbackDNSStreak.Load())
	}
	// The file appearing later is picked up by the next attempt's re-parse.
	if err := os.WriteFile(missing, []byte("nameserver 192.0.2.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.noteConnectionError(loop, 0)
	if r.pinnedNameserver() != "192.0.2.1:53" || !r.freshResolve.Load() {
		t.Fatalf("after the file appeared: pin=%q fresh=%v, want the pin applied", r.pinnedNameserver(), r.freshResolve.Load())
	}
	if exits.Load() != 0 {
		t.Fatalf("exit requested on the first pinned attempt (streak %d) — only lookups made with the pin in place count toward it", r.loopbackDNSStreak.Load())
	}
	// The count toward the exit starts at the pin: 5 more failures with
	// it in place, and the 5th exits.
	for i := 0; i < 4; i++ {
		r.noteConnectionError(loop, 0)
	}
	if exits.Load() != 0 {
		t.Fatalf("exit after 4 pinned failures (total streak %d), want only at 5", r.loopbackDNSStreak.Load())
	}
	r.noteConnectionError(loop, 0)
	if exits.Load() != 1 {
		t.Fatalf("exits=%d after 5 pinned failures, want 1", exits.Load())
	}
	// A later unreadable re-parse keeps the earlier pin.
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	r.noteConnectionError(loop, 0)
	if r.pinnedNameserver() != "192.0.2.1:53" {
		t.Fatalf("pin after an unreadable re-parse = %q, want the earlier pin kept", r.pinnedNameserver())
	}

	r, exits = latchTestRunner(t, writeResolvConf(t, "search lan\n# nothing else\n"), 5)
	for i := 0; i < 10; i++ {
		r.noteConnectionError(loop, 0)
	}
	if exits.Load() != 0 || r.pinnedNameserver() != "" {
		t.Fatalf("resolv.conf without a nameserver: exits=%d pin=%q, want no exit and no pin", exits.Load(), r.pinnedNameserver())
	}

	r, exits = latchTestRunner(t, writeResolvConf(t, "nameserver 127.0.0.1\n"), 5)
	for i := 0; i < 10; i++ {
		r.noteConnectionError(loop, 0)
	}
	if exits.Load() != 0 || r.pinnedNameserver() != "" || r.freshResolve.Load() {
		t.Fatalf("loopback-only resolv.conf: exits=%d pin=%q fresh=%v, want no exit and no pin (configured local resolver, not the latch)", exits.Load(), r.pinnedNameserver(), r.freshResolve.Load())
	}

	r, exits = latchTestRunner(t, writeResolvConf(t, "nameserver 192.0.2.1\n"), 5)
	for i := 0; i < 5; i++ { // 1 triggering failure + 4 with the pin in place
		r.noteConnectionError(loop, 0)
	}
	if exits.Load() != 0 {
		t.Fatalf("pinned but persisting, after 4 pinned failures: exits=%d, want 0", exits.Load())
	}
	r.noteConnectionError(loop, 0)
	if exits.Load() != 1 {
		t.Fatalf("pinned but persisting, after 5 pinned failures: exits=%d, want 1 (never-connected runner: not gated on everConnected)", exits.Load())
	}
	r.noteConnectionError(loop, 0)
	r.everConnected.Store(true)
	r.cfg.socketSelfRestartAfter = time.Minute
	r.failStreak.Store(socketSelfRestartMinFailures)
	ts := time.Now().Add(-2 * time.Minute)
	r.failStreakStart.Store(&ts)
	r.maybeSelfRestart(time.Now())
	if exits.Load() != 1 {
		t.Fatalf("exits=%d, want the single latch exit (fire-once, shared with the dark-window self-restart)", exits.Load())
	}

	r, exits = latchTestRunner(t, writeResolvConf(t, "nameserver 192.0.2.1\n"), 0)
	for i := 0; i < 10; i++ {
		r.noteConnectionError(loop, 0)
	}
	if exits.Load() != 0 || r.pinnedNameserver() != "192.0.2.1:53" {
		t.Fatalf("knob 0: exits=%d pin=%q, want no exit and the pin still applied", exits.Load(), r.pinnedNameserver())
	}
}

// fakeDNS is a minimal DNS server on one 127.0.0.1 port over both UDP and
// TCP (the pin is a single host:port the resolver uses for either): every
// A query is answered with 192.0.2.7, AAAA with an empty NOERROR, the
// question echoed verbatim. With truncate set, UDP answers carry TC so
// the resolver must retry over TCP (RFC 5966), exercising the hook's
// network argument.
type fakeDNS struct {
	addr       string
	udp        net.PacketConn
	tcp        net.Listener
	truncate   bool
	udpQueries atomic.Int32
	tcpQueries atomic.Int32
}

func newFakeDNS(t *testing.T, truncate bool) *fakeDNS {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		tcp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		udp, err := net.ListenPacket("udp", tcp.Addr().String())
		if err != nil {
			tcp.Close() // UDP side of that port in use; pick another
			continue
		}
		s := &fakeDNS{addr: tcp.Addr().String(), udp: udp, tcp: tcp, truncate: truncate}
		t.Cleanup(func() { udp.Close(); tcp.Close() })
		go s.serveUDP()
		go s.serveTCP()
		return s
	}
	t.Fatal("could not bind one port for both UDP and TCP")
	return nil
}

func (s *fakeDNS) serveUDP() {
	buf := make([]byte, 4096)
	for {
		n, from, err := s.udp.ReadFrom(buf)
		if err != nil {
			return
		}
		s.udpQueries.Add(1)
		if resp := fakeDNSAnswer(buf[:n], s.truncate); resp != nil {
			s.udp.WriteTo(resp, from)
		}
	}
}

func (s *fakeDNS) serveTCP() {
	for {
		c, err := s.tcp.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var hdr [2]byte
			if _, err := io.ReadFull(c, hdr[:]); err != nil {
				return
			}
			q := make([]byte, binary.BigEndian.Uint16(hdr[:]))
			if _, err := io.ReadFull(c, q); err != nil {
				return
			}
			s.tcpQueries.Add(1)
			resp := fakeDNSAnswer(q, false)
			if resp == nil {
				return
			}
			out := binary.BigEndian.AppendUint16(nil, uint16(len(resp)))
			c.Write(append(out, resp...))
		}()
	}
}

// fakeDNSAnswer builds the response for one query: header with the query
// id, the question echoed, and for an A query (unless truncating) a
// single A record via a name-compression pointer to the question.
func fakeDNSAnswer(q []byte, truncate bool) []byte {
	if len(q) < 12 {
		return nil
	}
	i := 12
	for i < len(q) && q[i] != 0 {
		i += int(q[i]) + 1
	}
	i++ // root label
	if i+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[i:])
	qend := i + 4
	flags := uint16(0x8180) // QR, RD, RA, NOERROR
	if truncate {
		flags |= 0x0200 // TC
	}
	var ancount uint16
	if qtype == 1 && !truncate {
		ancount = 1
	}
	resp := append([]byte(nil), q[0], q[1])
	resp = binary.BigEndian.AppendUint16(resp, flags)
	resp = binary.BigEndian.AppendUint16(resp, 1)
	resp = binary.BigEndian.AppendUint16(resp, ancount)
	resp = binary.BigEndian.AppendUint16(resp, 0)
	resp = binary.BigEndian.AppendUint16(resp, 0)
	resp = append(resp, q[12:qend]...)
	if ancount == 1 {
		resp = append(resp, 0xC0, 0x0C)
		resp = binary.BigEndian.AppendUint16(resp, 1) // A
		resp = binary.BigEndian.AppendUint16(resp, 1) // IN
		resp = binary.BigEndian.AppendUint32(resp, 60)
		resp = binary.BigEndian.AppendUint16(resp, 4)
		resp = append(resp, 192, 0, 2, 7)
	}
	return resp
}

// A real lookup through the pinned pure-Go resolver: UDP framing as a
// PacketConn, the TC → TCP retry honoring the hook's network argument,
// and prompt failure on context cancellation against a black hole. The
// host's own resolver config supplies only timeouts/attempts; every
// exchange goes to the pin.
func TestLoopbackLatchDialHookServesRealLookups(t *testing.T) {
	for _, truncate := range []bool{false, true} {
		srv := newFakeDNS(t, truncate)
		r, _ := latchTestRunner(t, writeResolvConf(t, "nameserver 192.0.2.1\n"), 5)
		r.freshResolve.Store(true)
		addr := srv.addr
		r.dnsPin.Store(&addr)
		res := r.newNetDialer().Resolver
		if res == nil || !res.PreferGo || res.Dial == nil {
			t.Fatalf("resolver = %+v, want PreferGo with the Dial hook", res)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		addrs, err := res.LookupIPAddr(ctx, "slack.test.")
		cancel()
		if err != nil {
			t.Fatalf("truncate=%v: lookup through the pin: %v", truncate, err)
		}
		if len(addrs) != 1 || !addrs[0].IP.Equal(net.IPv4(192, 0, 2, 7)) {
			t.Fatalf("truncate=%v: lookup = %v, want [192.0.2.7]", truncate, addrs)
		}
		if srv.udpQueries.Load() == 0 {
			t.Fatalf("truncate=%v: no UDP query reached the pin", truncate)
		}
		if truncate && srv.tcpQueries.Load() == 0 {
			t.Fatal("truncated UDP answer did not lead to a TCP retry through the pin")
		}
		if !truncate && srv.tcpQueries.Load() != 0 {
			t.Fatal("TCP used without truncation")
		}
	}

	blackhole, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	r, _ := latchTestRunner(t, writeResolvConf(t, "nameserver 192.0.2.1\n"), 5)
	r.freshResolve.Store(true)
	addr := blackhole.LocalAddr().String()
	r.dnsPin.Store(&addr)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := r.newNetDialer().Resolver.LookupIPAddr(ctx, "slack.test."); err == nil {
		t.Fatal("lookup against a black hole succeeded")
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("cancelled lookup took %s, want prompt failure", took)
	}
}

func TestLoadConfigDNSLoopbackExitAfter(t *testing.T) {
	base := baseSlackEnv()
	cfg, err := loadConfigFromEnv(stubEnv(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.dnsLoopbackExitAfter != 5 {
		t.Errorf("default dnsLoopbackExitAfter = %d, want 5", cfg.dnsLoopbackExitAfter)
	}
	for in, want := range map[string]int{"0": 0, "7": 7, " 3 ": 3} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env["SLACK_DNS_LOOPBACK_EXIT_AFTER"] = in
		cfg, err := loadConfigFromEnv(stubEnv(env))
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if cfg.dnsLoopbackExitAfter != want {
			t.Errorf("%q: dnsLoopbackExitAfter = %d, want %d", in, cfg.dnsLoopbackExitAfter, want)
		}
	}
	for _, in := range []string{"-1", "five", "2.5"} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env["SLACK_DNS_LOOPBACK_EXIT_AFTER"] = in
		if _, err := loadConfigFromEnv(stubEnv(env)); err == nil {
			t.Errorf("%q: expected config error", in)
		}
	}
}
