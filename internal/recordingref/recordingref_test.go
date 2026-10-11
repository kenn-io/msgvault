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
		want       []string
	}{
		{"loom", "See https://WWW.LOOM.COM:443/share/abc?token=secret#fragment.", nil, []string{"https://WWW.LOOM.COM:443/share/abc?token=secret#fragment"}},
		{"cap", "https://cap.so/dev/abc", nil, []string{"https://cap.so/dev/abc"}},
		{"padded cloud port", "https://loom.com:00443/share/abc", nil, []string{"https://loom.com:00443/share/abc"}},
		{"cap HTTP", "http://cap.so/s/abc http://www.cap.so/embed/abc", nil, nil},
		{"IPv4", "https://192.0.2.1:8443/s/abc", []string{"https://192.0.2.1:8443"}, []string{"https://192.0.2.1:8443/s/abc"}},
		{"IPv6 wrong port", "https://[2001:db8::1]:8443/s/abc", []string{"https://[2001:db8::1]"}, nil},
		{"unlisted", "https://cap.example.test/s/abc", nil, nil},
		{"lookalike", "https://loom.com.example.test/share/abc", nil, nil},
		{"user info", "https://alice@loom.com/share/abc", nil, nil},
		{"port", "https://loom.com:444/share/abc", nil, nil},
		{"long", "https://loom.com/share/" + strings.Repeat("a", 8192), nil, nil},
		{"encoded path", "https://loom.com/share/%61bc", nil, nil},
		{"long ID", "https://loom.com/share/" + strings.Repeat("a", 256), nil, []string{"https://loom.com/share/" + strings.Repeat("a", 256)}},
		{"punctuated ID", "https://cap.so/s/abc+def.ghi", nil, []string{"https://cap.so/s/abc+def.ghi"}},
		{"empty ID", "https://loom.com/share/", nil, nil},
		{"multiple segments", "https://cap.so/s/abc/def", nil, nil},
		{"deduplicate", "https://loom.com/share/abc https://www.loom.com/share/abc", nil, []string{"https://loom.com/share/abc"}},
		{"route identity", "https://loom.com/share/abc https://www.loom.com/embed/abc", nil, []string{"https://loom.com/share/abc", "https://www.loom.com/embed/abc"}},
		{"bold", "**https://loom.com/share/abc**", nil, []string{"https://loom.com/share/abc"}},
		{"bold sentence", "**https://loom.com/share/abc.**", nil, []string{"https://loom.com/share/abc"}},
		{"closing quote", "“https://loom.com/share/abc”", nil, []string{"https://loom.com/share/abc"}},
		{"ellipsis", "https://loom.com/share/abc…", nil, []string{"https://loom.com/share/abc"}},
		{"bold ellipsis", "**https://loom.com/share/abc…**", nil, []string{"https://loom.com/share/abc"}},
		{"trailing slash", "https://loom.com/share/abc/", nil, nil},
		{"underline query", "__https://loom.com/share/abc?token=a_b__", nil, []string{"https://loom.com/share/abc?token=a_b"}},
		{"strikethrough", "~~https://loom.com/share/abc~~", nil, []string{"https://loom.com/share/abc"}},
		{"nested", "**__https://loom.com/share/abc__**", nil, []string{"https://loom.com/share/abc"}},
		{"plain label punctuation", "[Watch](https://cap.so/s/abc!)", nil, []string{"https://cap.so/s/abc!"}},
		{"different URL label", "[https://loom.com/share/label](https://cap.so/s/target?token=a_b!)", nil, []string{"https://cap.so/s/target?token=a_b!"}},
		{"URL label with title", `[https://loom.com/share/label](https://cap.so/s/target "title") https://loom.com/share/other`, nil, []string{"https://cap.so/s/target", "https://loom.com/share/other"}},
		{"IPv6 URL label", "[https://[2001:db8::1]:8443/s/label](https://[2001:db8::1]:8443/s/abc?token=(one))", []string{"https://[2001:db8::1]:8443"}, []string{"https://[2001:db8::1]:8443/s/abc?token=(one)"}},
		{"literal query", "https://cap.so/s/abc?token=keep_*_~", nil, []string{"https://cap.so/s/abc?token=keep_*_~"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := scanProse(tc.text, originSet(tc.origins), nil)
			var got []string
			for _, ref := range refs {
				got = append(got, ref.Reference)
				assert.Len(t, ref.RouteKey, 64)
			}
			assert.Equal(t, tc.want, got)
		})
	}
	assert, require := assert.New(t), require.New(t)
	r := scanProse("https://cap.example.test:8443/s/abc?token=secret", originSet([]string{"https://cap.example.test:8443"}), nil)
	require.Len(r, 1)
	assert.Empty(r[0].Canonical)
	padded := scanProse("https://cap.example.test:08443/s/abc?token=secret", originSet([]string{"https://cap.example.test:8443"}), nil)
	require.Len(padded, 1)
	assert.Equal(r[0].RouteKey, padded[0].RouteKey)
}

func TestTeamsPointer(t *testing.T) {
	assert := assert.New(t)
	r, ok := TeamsPointer("teams:recording:abc", "https://teams.example.test/play?id=secret#fragment")
	assert.True(ok)
	assert.Equal("https://teams.example.test/play?id=secret", r.Canonical)
	_, ok = TeamsPointer("teams:inline:abc", "https://teams.example.test/play")
	assert.False(ok)
	_, ok = TeamsPointer("teams:recording:abc", "https://alice@teams.example.test/play")
	assert.False(ok)
}

func TestCanonicalOrigin(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://example.test/s/abc", ""},
		{"https://example.test#token", ""},
		{"HTTPS://EXAMPLE.TEST:443", "https://example.test"},
		{"https://example.test:00443", "https://example.test"},
		{"https://example.test:08443", "https://example.test:8443"},
		{"http://192.0.2.1:80", "http://192.0.2.1"},
		{"https://[2001:DB8::1]", "https://[2001:db8::1]"},
		{"https://[2001:db8::1]:443", "https://[2001:db8::1]"},
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
		{"https://cap.so/s/abc?token=literal**__", ""},
		{"https://www.loom.com/share/abc?token=example!)", " "},
		{"https://www.loom.com/share/abc?token=example!)", "\t\r\n\f"},
	} {
		t.Run(tc.target+tc.padding, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			refs, _ := scanHTML(`<a href="`+tc.padding+tc.target+tc.padding+`">Watch recording</a>`, originSet(nil))
			require.Len(refs, 1)
			assert.Equal(tc.target, refs[0].Reference)
		})
	}
	refs, _ := scanHTML(`<a href="prefix https://cap.so/s/abc">Watch</a>`, originSet(nil))
	assert.Empty(t, refs)
}
