package recordingref

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScan(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		origins    []string
		count      int
	}{
		{"loom", "See https://WWW.LOOM.COM:443/share/abc?token=secret#fragment.", nil, 1},
		{"cap", "https://cap.so/dev/abc", nil, 1},
		{"cap HTTP", "http://cap.so/s/abc http://www.cap.so/embed/abc", nil, 0},
		{"registered", "https://cap.example.test/s/abc", []string{"https://CAP.EXAMPLE.TEST:443"}, 1},
		{"IPv4", "https://192.0.2.1:8443/s/abc", []string{"https://192.0.2.1:8443"}, 1},
		{"IPv6 default port", "https://[2001:db8::1]:443/s/abc", []string{"https://[2001:db8::1]"}, 1},
		{"IPv6 nondefault port", "https://[2001:db8::1]:8443/s/abc", []string{"https://[2001:db8::1]:8443"}, 1},
		{"IPv6 wrong port", "https://[2001:db8::1]:8443/s/abc", []string{"https://[2001:db8::1]"}, 0},
		{"unlisted", "https://cap.example.test/s/abc", nil, 0},
		{"lookalike", "https://loom.com.example.test/share/abc", nil, 0},
		{"user info", "https://alice@loom.com/share/abc", nil, 0},
		{"port", "https://loom.com:444/share/abc", nil, 0},
		{"long", "https://loom.com/share/" + strings.Repeat("a", 8192), nil, 0},
		{"encoded path", "https://loom.com/share/%61bc", nil, 0},
		{"long ID", "https://loom.com/share/" + strings.Repeat("a", 256), nil, 1},
		{"punctuated ID", "https://cap.so/s/abc+def.ghi", nil, 1},
		{"empty ID", "https://loom.com/share/", nil, 0},
		{"multiple segments", "https://cap.so/s/abc/def", nil, 0},
		{"deduplicate", "https://loom.com/share/abc https://www.loom.com/share/abc", nil, 1},
		{"route identity", "https://loom.com/share/abc https://www.loom.com/embed/abc", nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := scanProse(tc.text, tc.origins, nil)
			require.Len(t, refs, tc.count)
			if tc.count == 1 {
				assert.Len(t, refs[0].RouteKey, 64)
			}
		})
	}
	r := scanProse("https://cap.example.test/s/abc?token=secret", []string{"https://cap.example.test"}, nil)
	require.Len(t, r, 1)
	assert.Empty(t, r[0].Canonical)
}

func TestTeamsPointer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	r, ok := TeamsPointer("teams:recording:abc", "https://teams.example.test/play?id=secret#fragment")
	assert.True(ok)
	assert.Equal("https://teams.example.test/play?id=secret", r.Canonical)
	_, ok = TeamsPointer("teams:inline:abc", "https://teams.example.test/play")
	assert.False(ok)
	_, ok = TeamsPointer("teams:recording:abc", "https://alice@teams.example.test/play")
	assert.False(ok)
	r, ok = TeamsPointer("teams:recording:ipv6", "https://[2001:DB8::1]:8443/play?id=secret#fragment")
	require.True(ok)
	assert.Equal("https://[2001:db8::1]:8443/play?id=secret", r.Canonical)
}

func TestCanonicalOrigin(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://example.test/s/abc", ""},
		{"https://example.test#token", ""},
		{"HTTPS://EXAMPLE.TEST:443", "https://example.test"},
		{"https://192.0.2.1:443", "https://192.0.2.1"},
		{"http://192.0.2.1:80", "http://192.0.2.1"},
		{"https://[2001:DB8::1]", "https://[2001:db8::1]"},
		{"https://[2001:db8::1]:443", "https://[2001:db8::1]"},
		{"http://[2001:db8::1]:80", "http://[2001:db8::1]"},
		{"https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443"},
		{"https://bücher.example", "https://xn--bcher-kva.example"},
		{"https://[2001:db8::1]:0", ""},
		{"https://[2001:db8::1]:65536", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			origin, err := CanonicalOrigin(tc.raw)
			if tc.want == "" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, origin)
		})
	}
}

func TestScanHTML(t *testing.T) {
	for _, tc := range []struct{ target, padding string }{
		{"https://cap.so/s/abc!", ""},
		{"https://cap.so/s/abc?token=secret!", ""},
		{"https://cap.so/s/abc?token=secret)", ""},
		{"https://www.loom.com/share/abc?token=example!)", " "},
		{"https://www.loom.com/share/abc?token=example!)", "\t\r\n\f"},
	} {
		t.Run(tc.target+tc.padding, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			refs := ScanHTML(`<a href="`+tc.padding+tc.target+tc.padding+`">Watch recording</a>`, nil)
			require.Len(refs, 1)
			assert.Equal(tc.target, refs[0].Reference)
			if tc.target == "https://cap.so/s/abc!" {
				plain := scanProse("https://cap.so/s/abc", nil, nil)
				require.Len(plain, 1)
				assert.NotEqual(plain[0].RouteKey, refs[0].RouteKey)
				assert.Equal(tc.target, refs[0].Canonical)
			}
		})
	}
	assert.Empty(t, ScanHTML(`<a href="prefix https://cap.so/s/abc">Watch</a>`, nil))
}
