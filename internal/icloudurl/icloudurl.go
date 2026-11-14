// Package icloudurl recognizes Apple's CardDAV bootstrap and partition URLs.
package icloudurl

import (
	"net/url"
	"regexp"
	"strings"
)

var partitionHost = regexp.MustCompile(`^p[0-9]{2,53}-contacts\.icloud\.com$`)

// IsDiscoveryURL recognizes the published bootstrap root.
func IsDiscoveryURL(raw string) bool {
	target, ok := parse(raw)
	return ok && strings.EqualFold(target.Hostname(), "contacts.icloud.com") &&
		(target.Path == "" || target.Path == "/") && target.RawPath == ""
}

// PartitionOrigin returns the canonical origin of a partition URL.
func PartitionOrigin(raw string) (string, bool) {
	target, ok := parse(raw)
	if !ok || !partitionHost.MatchString(strings.ToLower(target.Hostname())) {
		return "", false
	}
	return "https://" + strings.ToLower(target.Hostname()), true
}

// PartitionRootOrigin recognizes a partition origin with an optional slash.
func PartitionRootOrigin(raw string) (string, bool) {
	origin, ok := PartitionOrigin(raw)
	if !ok {
		return "", false
	}
	target, err := url.Parse(raw)
	if err != nil || (target.Path != "" && target.Path != "/") || target.RawPath != "" {
		return "", false
	}
	return origin, true
}

func parse(raw string) (*url.URL, bool) {
	if strings.TrimSpace(raw) != raw || strings.Contains(raw, "\\") {
		return nil, false
	}
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != "https" || target.Opaque != "" || target.User != nil ||
		target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || strings.Contains(raw, "#") {
		return nil, false
	}
	host := target.Hostname()
	for i := range len(target.Host) {
		if target.Host[i] >= 0x80 {
			return nil, false
		}
	}
	if host == "" || (!strings.EqualFold(target.Host, host) && !strings.EqualFold(target.Host, host+":443")) {
		return nil, false
	}
	return target, true
}
