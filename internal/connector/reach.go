package connector

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Origin says where a spec came from. It is the only thing that decides where
// that spec is allowed to point, and the reason is that the two sources are not
// equally trusted.
//
// A connector in ./connectors/ is configuration: an operator put it on the disk,
// and pointing at localhost is not merely allowed but the normal case — every
// verified connector in this repository does it. A connector that arrived at
// POST /v1/agents came over the network from whoever holds the shared token,
// and `base` is a URL the server will then fetch on their behalf. Without a
// line between the two, that endpoint is a request forgery primitive: it reads
// internal services, and on a cloud instance it reads the metadata endpoint,
// which is where the credentials are.
//
// Restricting by origin rather than by address is what keeps both true. The
// alternative — blocking private addresses for everyone — would break every
// working connector to close a hole that only one path opens.
type Origin string

const (
	// FromFile is the zero value, so anything constructed directly is trusted.
	// That is deliberate: the untrusted path is a single handler, and marking
	// the one place bytes arrive from the network is more reliable than
	// remembering to bless every other construction site.
	FromFile Origin = ""

	// FromAPI marks a spec that arrived over HTTP.
	FromAPI Origin = "api"
)

type originKey struct{}

// withOrigin carries the spec's origin to the dialer, which is the only place
// the destination is known for certain. Checking `base` at registration cannot
// be enough on its own: a name that resolves publicly when it is registered can
// resolve to 169.254.169.254 by the time it is fetched.
func withOrigin(ctx context.Context, o Origin) context.Context {
	if o == FromFile {
		return ctx
	}
	return context.WithValue(ctx, originKey{}, o)
}

func restricted(ctx context.Context) bool {
	o, _ := ctx.Value(originKey{}).(Origin)
	return o == FromAPI
}

var dialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

// guardedDial refuses to connect a restricted spec to an address that is not
// on the public internet.
//
// The check happens after resolution and the connection is then made to the
// address that was checked, rather than to the name. Resolving twice is the
// whole of a DNS rebinding attack: the first answer passes the check and the
// second one is what gets dialled.
func guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	if !restricted(ctx) {
		return dialer.DialContext(ctx, network, addr)
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no address for %s", host)
	}
	// Every answer has to pass, not just the one that gets used. A name
	// resolving to one public and one private address is the same attack with
	// an extra step.
	for _, ip := range ips {
		if why := unreachableReason(ip.IP); why != "" {
			return nil, fmt.Errorf(
				"refusing to reach %s (%s): %s. Agents registered over the API may only "+
					"point at public addresses; put this connector in the connectors "+
					"directory, or start the server with -allow-private-agents", host, ip.IP, why)
		}
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// unreachableReason names why an address is off limits, or returns "".
//
// Phrased as a reason rather than a bool because it ends up in an error a human
// has to act on, and "connection refused" for a policy decision is the kind of
// thing that costs an afternoon.
func unreachableReason(ip net.IP) string {
	switch {
	case ip.IsLoopback():
		return "loopback"
	// Before the link-local cases, which overlap it: 224.0.0.0/24 is link-local
	// multicast, and calling that "where cloud metadata lives" would be a
	// confidently wrong explanation.
	case ip.IsMulticast(), ip.IsInterfaceLocalMulticast():
		return "multicast"
	case ip.IsLinkLocalUnicast():
		// The one that matters most on a cloud instance: 169.254.169.254 serves
		// instance metadata, and on many setups that includes role credentials.
		return "link-local, where cloud instance metadata lives"
	case ip.IsPrivate():
		return "a private network"
	case ip.IsUnspecified():
		return "unspecified"
	case isCGNAT(ip):
		return "carrier-grade NAT, which is a private range in practice"
	}
	return ""
}

// isCGNAT covers 100.64.0.0/10. Not private by RFC 1918 and treated as internal
// by everything that uses it — Tailscale and most mobile carriers.
func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

// checkReachable reports whether a restricted spec may be dialled at all,
// without connecting.
//
// `check` opens a raw connection to report reachability, which for a restricted
// spec would be a port scanner that answers one host at a time. It has to
// consult the same policy as the dialer.
func checkReachable(ctx context.Context, host string) error {
	if !restricted(ctx) {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if why := unreachableReason(ip.IP); why != "" {
			return fmt.Errorf("refusing to reach %s (%s): %s", host, ip.IP, why)
		}
	}
	return nil
}
