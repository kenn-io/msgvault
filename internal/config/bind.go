package config

import (
	"fmt"
	"net"
	"slices"
	"strings"
)

// BindAddressSource reports where the loaded bind address came from.
func (c *Config) BindAddressSource() string {
	if c.bindSource != "" {
		return c.bindSource
	}
	return "default"
}

// ResolveBindAddress resolves an interface selector to one usable address.
// It never falls back to another interface or a wildcard bind.
func ResolveBindAddress(address string) (string, error) {
	if !strings.HasPrefix(address, "iface:") {
		return address, nil
	}
	name := strings.TrimPrefix(address, "iface:")
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return "", fmt.Errorf("resolve bind interface %q: %w", name, err)
	}
	if iface.Flags&net.FlagUp == 0 {
		return "", fmt.Errorf("bind interface %q is down", name)
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("read bind interface %q addresses: %w", name, err)
	}
	var ipv4, ipv6 []string
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
			continue
		}
		if ip.To4() != nil {
			ipv4 = append(ipv4, ip.String())
		} else {
			ipv6 = append(ipv6, ip.String())
		}
	}
	for _, candidates := range [][]string{ipv4, ipv6} {
		if len(candidates) != 0 {
			slices.Sort(candidates)
			return candidates[0], nil
		}
	}
	return "", fmt.Errorf("bind interface %q has no usable IP address", name)
}

// ResolveServerBindAddress resolves the configured bind for runtime use while
// keeping an iface:NAME selector out of subsequent config saves.
func (c *Config) ResolveServerBindAddress() (string, error) {
	resolved, err := ResolveBindAddress(c.Server.BindAddr)
	if err != nil {
		return "", err
	}
	if resolved != c.Server.BindAddr {
		c.runtimeConfigState.bindAddr.capture(c.Server.BindAddr, resolved)
		c.Server.BindAddr = resolved
	}
	return resolved, nil
}
