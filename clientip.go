package maxmind_geoblock //nolint:revive,stylecheck // Traefik derives the package name from the module path.

import (
	"net/http"
	"net/netip"
	"strings"
)

const xForwardedFor = "X-Forwarded-For"

// forwardedIP determines the client IP address of a request behind trusted
// proxies. peer is the already parsed connecting address (req.RemoteAddr).
//
// The peer is used unless it is a trusted forwarder, in which case the
// X-Forwarded-For header is walked from right to left and the first address
// that is not a trusted forwarder is returned (if every entry is trusted, the
// left-most one is used).
//
// The zero netip.Addr is returned when the IP cannot be determined, e.g. a
// trusted forwarder sent a malformed X-Forwarded-For header.
//
// Prefix matching is inlined rather than delegated to containsIP: function
// calls dominate the cost of this path under Yaegi.
func (g *GeoBlock) forwardedIP(req *http.Request, peer netip.Addr) netip.Addr {
	if !peer.IsValid() {
		return peer
	}

	trusted := g.trustedForwarders
	peerTrusted := false

	for i := range trusted {
		if trusted[i].Contains(peer) {
			peerTrusted = true

			break
		}
	}

	if !peerTrusted {
		return peer
	}

	values := req.Header.Values(xForwardedFor)
	if len(values) == 0 {
		return peer
	}

	xff := values[0]
	if len(values) > 1 {
		xff = strings.Join(values, ",")
	}

	var leftmost netip.Addr

	for xff != "" {
		var entry string

		if i := strings.LastIndexByte(xff, ','); i >= 0 {
			entry, xff = xff[i+1:], xff[:i]
		} else {
			entry, xff = xff, ""
		}

		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		ip := parseIP(entry)
		if !ip.IsValid() {
			return netip.Addr{}
		}

		ipTrusted := false

		for i := range trusted {
			if trusted[i].Contains(ip) {
				ipTrusted = true

				break
			}
		}

		if !ipTrusted {
			return ip
		}

		leftmost = ip
	}

	if leftmost.IsValid() {
		return leftmost
	}

	return peer
}

// parseIP parses "ip", "ip:port" or "[ip]:port", normalizing IPv4-mapped IPv6
// addresses to IPv4 and dropping zones. It returns the zero Addr on failure.
func parseIP(s string) netip.Addr {
	var (
		ip  netip.Addr
		err error
	)

	// Pick the parser from the shape of the input instead of trying both:
	// a failed parse allocates an error. "[...]" or a single ':' means a port.
	if first := strings.IndexByte(s, ':'); first >= 0 && (s[0] == '[' || strings.IndexByte(s[first+1:], ':') < 0) {
		var ap netip.AddrPort

		ap, err = netip.ParseAddrPort(s)
		ip = ap.Addr()
	} else {
		ip, err = netip.ParseAddr(s)
	}

	if err != nil {
		return netip.Addr{}
	}

	if ip.Is4In6() {
		ip = ip.Unmap()
	}

	if ip.Zone() != "" {
		ip = ip.WithZone("")
	}

	return ip
}
