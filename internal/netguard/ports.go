package netguard

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// EffectivePort returns an explicit port or the HTTP scheme's default.
func EffectivePort(target *url.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	if strings.EqualFold(target.Scheme, "https") {
		return "443"
	}
	return "80"
}

// TargetPort validates a numeric TCP port after applying the HTTP default.
func TargetPort(target *url.URL) (uint16, error) {
	port, err := strconv.ParseUint(EffectivePort(target), 10, 16)
	if err != nil || port == 0 {
		return 0, errors.New("invalid port")
	}
	return uint16(port), nil
}
