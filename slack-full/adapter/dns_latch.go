package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strings"
)

// Loopback-nameserver latch: detector, resolv.conf self-parse, resolver
// pin (gp-keg / ci-c3zl9 / hw-i6yt7, papercut pc_82ff6cc9d209).
//
// INCIDENT (citadel, 2026-08-24 evening, third MagicDNS stall): the
// gp-bsk DNS self-heal had already flipped the Socket Mode runner to Go's
// built-in resolver (net.Resolver{PreferGo}) during the earlier stalls —
// at 20:58 the errors read "lookup slack.com on [fd7a:115c:a1e0::53]:53:
// server misbehaving", the MagicDNS v6 server that /etc/resolv.conf
// listed. At 21:00:03 the launchd MagicDNS watchdog ran `tailscale set
// --accept-dns=false`, which rewrites /etc/resolv.conf. Go's built-in
// resolver parses that file itself, in a package-level singleton shared by
// every net.Resolver in the process; it caught the rewrite and dropped
// onto its compiled-in fallback nameservers, 127.0.0.1:53 and [::1]:53
// (net.defaultNS), and stayed there: from 21:00:27 every
// apps.connections.open failed with
//
//	lookup slack.com on [::1]:53: read udp ...: connection refused
//
// through two full 12-attempt reconnect cycles, until a manual
// `gc service restart slack` at 21:08:57 — while /etc/resolv.conf was
// healthy (192.168.86.1) and every other process resolved fine. Rebuilding
// a net.Resolver cannot escape that state (the config is package-level),
// and the gp-bsk dark-window self-restart would only have fired at ~21:10.
// gp-keg makes THIS signature recover in seconds:
//
//  1. Detect it: a connect failure whose lookup reports exactly one of
//     Go's fallback nameservers (isLoopbackDNSServerError — typed
//     *net.DNSError.Server through url.Error/net.OpError chains, or the
//     flattened "lookup X on S:" text slack-go emits). Exact match, not
//     "any loopback": a systemd-resolved stub on 127.0.0.53 or a local
//     stub on another port is a configured resolver, not the latch.
//  2. Escape it without net's help: on each such failure the runner
//     re-parses /etc/resolv.conf ITSELF, right now
//     (readResolvConfNameservers), and pins the pure-Go resolver's Dial
//     hook to a usable nameserver from it — every DNS exchange then goes
//     there regardless of what net's cached config says. Repeated events
//     rotate across the file's candidates. The pin is evidence-driven and
//     transient: cleared on Connected, re-derived on the next event, so it
//     never goes stale across a later DNS change.
//  3. If the pin does not take, get out fast: a signature that persists
//     for cfg.dnsLoopbackExitAfter consecutive lookups made WITH a pin
//     already in place means something in-process is not honoring it —
//     request the
//     orderly self-exit (same loss-free path as the gp-bsk self-restart:
//     drain → spool → seal, exit 1, shared fire-once latch); the service
//     supervisor restarts the adapter and the fresh process parses the
//     now-healthy file the way net expects. That trigger cannot
//     restart-loop: a fresh process only reproduces the signature if the
//     file yields no nameserver — and then there is no pin, so no exit.
//     SLACK_DNS_LOOPBACK_EXIT_AFTER=0 disables the exit (the pin still
//     applies).
//
//     Deliberately NO exit when there is nothing to pin (file unreadable,
//     no nameserver line, or only Go's own fallback addresses — a local
//     resolver configured that way and currently down is
//     indistinguishable from the latch): a fresh process would parse the
//     same file, latch the same way and exit again, a supervisor restart
//     loop that also takes down outbound /publish, the one path that kept
//     working through every incident. Instead every further attempt
//     re-parses the file, so the pin applies the moment it becomes
//     usable; the gp-bsk 10m self-restart stays the backstop.
//
// The gp-bsk 3-strike "no such host" flip and both backoff ladders are
// untouched. Note for the curious: in go1.26.5 (the deployed toolchain)
// net's resolvConf sets noReload only for `options no-reload`; a read that
// hits ENOENT leaves the mtime zero and re-parses at the next 5 s check,
// so the persistent latch is most likely a parse of the rewritten file
// with no nameserver line (fallback servers + the file's real mtime, no
// re-read until it changes again). The pin bypasses net's cached config
// either way, so the design does not depend on which it was.

// defaultResolvConfPath is the file the loopback-latch re-parse reads.
// Runners carry it as a field so tests can point at a fixture.
const defaultResolvConfPath = "/etc/resolv.conf"

// resolvConfMaxNameservers mirrors the libc/Go limit on nameserver lines
// honored from resolv.conf.
const resolvConfMaxNameservers = 3

// isGoDefaultNameserver reports whether server is one of the servers Go's
// built-in resolver falls back to when /etc/resolv.conf yields no usable
// nameserver (net.defaultNS: "127.0.0.1:53", "[::1]:53"). Accepts any
// textual spelling of those two addresses.
func isGoDefaultNameserver(server string) bool {
	host, port, err := net.SplitHostPort(server)
	if err != nil || port != "53" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback)
}

// lookupServerRe matches the server clause of a Go resolver error in its
// text form: "lookup <name> on <server>: <cause>". The cgo resolver never
// names a server ("lookup <name>: no such host").
var lookupServerRe = regexp.MustCompile(`lookup \S+ on (\S+?): `)

// dnsServerOf returns the nameserver a DNS failure reports, or "" when
// the error is not a DNS failure or names no server. The typed check
// covers errors that keep their chain (url.Error → net.OpError →
// net.DNSError); the text check covers slack-go paths that flatten the
// error into a string.
func dnsServerOf(err error) string {
	if err == nil {
		return ""
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.Server
	}
	if m := lookupServerRe.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return ""
}

// isLoopbackDNSServerError reports whether err is a lookup failure
// against one of Go's fallback nameservers — the signature of a pure-Go
// resolver that lost /etc/resolv.conf.
func isLoopbackDNSServerError(err error) bool {
	return isGoDefaultNameserver(dnsServerOf(err))
}

// parseResolvConfNameservers returns the nameserver entries of a
// resolv.conf body as "host:port" strings (port 53), in file order, at
// most resolvConfMaxNameservers. Mirrors net's own parse: comments ('#',
// ';') and other directives are ignored, and only literal IP addresses
// count (a hostname would need DNS to resolve). A read error (or a line
// too long for the scanner) is reported rather than passed off as an
// empty file.
func parseResolvConfNameservers(body io.Reader) ([]string, error) {
	var servers []string
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" || len(servers) >= resolvConfMaxNameservers {
			continue
		}
		if _, err := netip.ParseAddr(f[1]); err != nil {
			continue
		}
		servers = append(servers, net.JoinHostPort(f[1], "53"))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return servers, nil
}

// readResolvConfNameservers parses the nameservers of the resolv.conf at
// path. A missing or unreadable file is an error (nothing to pin to).
func readResolvConfNameservers(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseResolvConfNameservers(f)
}

// usableNameservers drops Go's own fallback servers from a nameserver
// list: pinning the resolver to 127.0.0.1:53 or [::1]:53 would reproduce
// the latch it is escaping. Everything else — LAN routers, MagicDNS,
// a systemd-resolved stub on 127.0.0.53 — is a candidate.
func usableNameservers(servers []string) []string {
	var out []string
	for _, s := range servers {
		if !isGoDefaultNameserver(s) {
			out = append(out, s)
		}
	}
	return out
}
