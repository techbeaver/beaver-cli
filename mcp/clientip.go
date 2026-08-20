package mcp

import (
	"net"
	"net/http"
	"strings"

	"github.com/techbeaver/beaver-cli/client"
)

// defaultTrustedProxyCIDRs is what we trust to state a caller's real address
// when nothing is configured.
//
// These are the private ranges, and the default is deliberate rather than lazy.
// This service is exposed through a ClusterIP Service and an ingress, so the
// peer on the other end of the socket is always the ingress controller and
// always private. A caller on the public internet can therefore never be the
// direct peer, and so can never be trusted to state its own address, which is
// the property that makes the header safe to read at all.
//
// Configure MCP_TRUSTED_PROXIES to override, e.g. when running this service
// somewhere its immediate peer is not on a private network.
var defaultTrustedProxyCIDRs = []string{
	"127.0.0.0/8",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"::1/128",
	"fc00::/7",
}

// parseTrustedProxies turns a configured list into matchers, reporting anything
// it could not read so the caller can say so out loud rather than silently
// running with a narrower trust list than intended.
func parseTrustedProxies(entries []string) (nets []*net.IPNet, invalid []string) {
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if _, network, err := net.ParseCIDR(entry); err == nil {
			nets = append(nets, network)
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		invalid = append(invalid, entry)
	}
	return nets, invalid
}

// trustedPeer reports whether the immediate peer may state a caller's address.
func trustedPeer(remoteAddr string, trusted []*net.IPNet) bool {
	ip := net.ParseIP(peerHost(remoteAddr))
	if ip == nil {
		return false
	}
	for _, network := range trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// peerHost strips the port from a RemoteAddr.
func peerHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		return strings.TrimSpace(remoteAddr)
	}
	return host
}

// callerIP returns the address a request came from.
//
// Only CF-Connecting-IP is read, and only from a trusted peer.
// X-Forwarded-For is deliberately not consulted: with an ingress in front of
// Cloudflare in front of a client the real address is neither the first entry
// nor the last, and a rule that is wrong in some deployments is worse than one
// that plainly falls back.
//
// The fallback is the peer itself. When this service is not behind Cloudflare
// that means every caller resolves to the ingress, and every uncredentialed
// caller therefore shares one bucket. That is why the limiter keys credentialed
// traffic on the token instead: the common case stays correct either way.
func callerIP(r *http.Request, trusted []*net.IPNet) string {
	if trustedPeer(r.RemoteAddr, trusted) {
		if forwarded := strings.TrimSpace(r.Header.Get(client.HeaderCFConnectingIP)); forwarded != "" {
			// This becomes a limiter key and reaches the API, so never arbitrary text.
			if ip := net.ParseIP(forwarded); ip != nil {
				return ip.String()
			}
		}
	}
	if host := peerHost(r.RemoteAddr); host != "" {
		return host
	}
	// Named, so "we could not tell" is visible in a key space; empty is not.
	return "unknown-caller"
}
