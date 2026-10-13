package caldav

import (
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// canonicalURL preserves reserved path escapes while normalizing equivalent
// origin spellings and percent-encoded unreserved characters.
func canonicalURL(value *url.URL) *url.URL {
	u := *value
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else {
		u.Host = host
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		}
	}
	path := u.EscapedPath()
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] == '%' && i+2 < len(path) {
			n, err := strconv.ParseUint(path[i+1:i+3], 16, 8)
			if err == nil {
				c := byte(n)
				if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", rune(c)) {
					b.WriteByte(c)
				} else {
					b.WriteByte('%')
					b.WriteString(strings.ToUpper(path[i+1 : i+3]))
				}
				i += 2
				continue
			}
		}
		b.WriteByte(path[i])
	}
	u.RawPath = b.String()
	return &u
}

func sameOriginURL(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	left = canonicalURL(left)
	right = canonicalURL(right)
	return left.Scheme == right.Scheme && left.Host == right.Host
}

func sameCollectionURL(left, right *url.URL) bool {
	if !sameOriginURL(left, right) {
		return false
	}
	left = canonicalURL(left)
	right = canonicalURL(right)
	leftPath, rightPath := path.Clean(left.EscapedPath()), path.Clean(right.EscapedPath())
	if leftPath == "." {
		leftPath = "/"
	}
	if rightPath == "." {
		rightPath = "/"
	}
	return leftPath == rightPath && left.RawQuery == right.RawQuery && left.ForceQuery == right.ForceQuery
}
