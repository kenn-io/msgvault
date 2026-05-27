package fbmessenger

import (
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

var convergenceWS = regexp.MustCompile(`\s+`)

func normalizeConvergence(s string) string {
	return strings.TrimSpace(convergenceWS.ReplaceAllString(s, " "))
}

func TestJSONHTMLConvergence_Simple(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	jsonRoot := "testdata/json_simple"
	htmlRoot := "testdata/html_simple"
	jsonTh, err := ParseJSONThread(jsonRoot, threadDir(t, jsonRoot, "alice_ABC123"))
	require.NoError(err, "json")
	htmlTh, err := ParseHTMLThread(htmlRoot, threadDir(t, htmlRoot, "alice_ABC123"))
	require.NoError(err, "html")
	require.Len(jsonTh.Messages, len(htmlTh.Messages), "message count")
	// Participants equal by slug.
	var jSlugs, hSlugs []string
	for _, p := range jsonTh.Participants {
		jSlugs = append(jSlugs, Slug(p.Name))
	}
	for _, p := range htmlTh.Participants {
		hSlugs = append(hSlugs, Slug(p.Name))
	}
	sort.Strings(jSlugs)
	sort.Strings(hSlugs)
	assert.Equal(hSlugs, jSlugs, "participant slugs differ")
	// Per-message bodies and timestamps.
	//
	// Reactions are a JSON-only feature (HTML exports do not expose
	// reaction metadata), so we compare bodies on their common ground:
	// the JSON body with its trailing "[reacted: ...]" suffix stripped.
	// Dual-path reaction coverage lives in TestImportDYI_ReactionsDualPath.
	for i := range jsonTh.Messages {
		jb := normalizeConvergence(stripReactionSuffix(jsonTh.Messages[i].Body))
		hb := normalizeConvergence(htmlTh.Messages[i].Body)
		assert.Equal(hb, jb, "message[%d] body differs", i)
		jt := jsonTh.Messages[i].SentAt.Truncate(time.Minute)
		ht := htmlTh.Messages[i].SentAt.Truncate(time.Minute)
		assert.True(jt.Equal(ht), "message[%d] timestamp differs: json=%v html=%v", i, jt, ht)
		assert.Equal(Slug(htmlTh.Messages[i].SenderName), Slug(jsonTh.Messages[i].SenderName),
			"message[%d] sender differs: json=%q html=%q",
			i, jsonTh.Messages[i].SenderName, htmlTh.Messages[i].SenderName)
	}
}
