package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var ErrUnsafeTarget = errors.New("unsafe destination")

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type DialContext func(context.Context, string, string) (net.Conn, error)

func PinnedAddresses(ctx context.Context, resolver Resolver, target, trustedOrigin *url.URL, trusted []netip.Addr, allowPrivate bool) ([]netip.AddrPort, error) {
	port, err := TargetPort(target)
	if err != nil {
		return nil, fmt.Errorf("port: %w", ErrUnsafeTarget)
	}
	if trustedOrigin != nil && strings.EqualFold(trustedOrigin.Scheme, target.Scheme) && strings.EqualFold(trustedOrigin.Hostname(), target.Hostname()) && EffectivePort(trustedOrigin) == EffectivePort(target) {
		result := make([]netip.AddrPort, 0, len(trusted))
		for _, addr := range trusted {
			result = append(result, netip.AddrPortFrom(addr, port))
		}
		return result, nil
	}
	host := target.Hostname()
	if literal, e := netip.ParseAddr(host); e == nil {
		if ProhibitedIP(literal) && !allowPrivate {
			return nil, fmt.Errorf("literal destination: %w", ErrUnsafeTarget)
		}
		return []netip.AddrPort{netip.AddrPortFrom(literal.Unmap(), port)}, nil
	}
	if ProhibitedHostname(host) {
		return nil, fmt.Errorf("hostname: %w", ErrUnsafeTarget)
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, fmt.Errorf("resolving host %q: %w", host, err)
	}
	result := make([]netip.AddrPort, 0, len(addresses))
	seen := make(map[netip.AddrPort]bool, len(addresses))
	for _, addr := range addresses {
		if ProhibitedIP(addr) && !allowPrivate {
			return nil, fmt.Errorf("resolved destination: %w", ErrUnsafeTarget)
		}
		p := netip.AddrPortFrom(addr.Unmap(), port)
		if !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	return result, nil
}
func PinnedDialer(pinned []netip.AddrPort, dial DialContext) DialContext {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var failures []error
		for i, address := range pinned {
			attempt := ctx
			cancel := func() {}
			if deadline, ok := ctx.Deadline(); ok && i < len(pinned)-1 {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return nil, ctx.Err()
				}
				attempt, cancel = context.WithTimeout(ctx, remaining/time.Duration(len(pinned)-i))
			}
			conn, err := dial(attempt, network, address.String())
			cancel()
			if err == nil {
				return conn, nil
			}
			failures = append(failures, err)
		}
		return nil, errors.Join(failures...)
	}
}
