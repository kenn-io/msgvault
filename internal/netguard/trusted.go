package netguard

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
)

var explicitPrivatePrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// ValidateTrustedDestination validates an operator-approved HTTPS origin and
// private address pins, returning normalized copies. An empty policy is valid.
func ValidateTrustedDestination(origin *url.URL, addresses []netip.Addr) (*url.URL, []netip.Addr, error) {
	if origin == nil && len(addresses) == 0 {
		return nil, nil, nil
	}
	switch {
	case origin == nil || origin.Hostname() == "":
		return nil, nil, errors.New("trusted_origin must include a hostname")
	case origin.Scheme != "https":
		return nil, nil, errors.New("trusted_origin must use HTTPS")
	case origin.User != nil:
		return nil, nil, errors.New("trusted_origin must not include credentials")
	case (origin.Path != "" && origin.Path != "/") || origin.RawPath != "":
		return nil, nil, errors.New("trusted_origin must not include a path other than /")
	case origin.RawQuery != "" || origin.ForceQuery:
		return nil, nil, errors.New("trusted_origin must not include a query")
	case origin.Fragment != "":
		return nil, nil, errors.New("trusted_origin must not include a fragment")
	case ProhibitedHostname(origin.Hostname()):
		return nil, nil, errors.New("trusted_origin must use a fully qualified, non-reserved hostname")
	case len(addresses) == 0:
		return nil, nil, errors.New("trusted_addresses must contain at least one address")
	}
	if rawPort := origin.Port(); rawPort != "" {
		port, err := strconv.ParseUint(rawPort, 10, 16)
		if err != nil || port == 0 {
			return nil, nil, errors.New("trusted_origin port must be between 1 and 65535")
		}
	}
	validated := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if address.Zone() != "" {
			return nil, nil, fmt.Errorf("trusted_addresses: address %s must not include a zone", address)
		}
		allowed := slices.ContainsFunc(explicitPrivatePrefixes, func(prefix netip.Prefix) bool {
			return prefix.Contains(address)
		})
		if !allowed {
			return nil, nil, fmt.Errorf("trusted_addresses: address %s is not in an allowed private range", address)
		}
		if slices.Contains(validated, address) {
			return nil, nil, fmt.Errorf("trusted_addresses: duplicate address %s", address)
		}
		validated = append(validated, address)
	}
	copyOrigin := *origin
	copyOrigin.Path = ""
	return &copyOrigin, validated, nil
}
