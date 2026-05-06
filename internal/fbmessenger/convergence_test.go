package fbmessenger

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

var convergenceWS = regexp.MustCompile(`\s+`)

func normalizeConvergence(s string) string {
	return strings.TrimSpace(convergenceWS.ReplaceAllString(s, " "))
}

func TestJSONHTMLConvergence_Simple(t *testing.T) {
	jsonRoot := "testdata/json_simple"
	htmlRoot := "testdata/html_simple"
	jsonTh, err := ParseJSONThread(jsonRoot, threadDir(t, jsonRoot, "inbox", "alice_ABC123"))
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	htmlTh, err := ParseHTMLThread(htmlRoot, threadDir(t, htmlRoot, "inbox", "alice_ABC123"))
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	if len(jsonTh.Messages) != len(htmlTh.Messages) {
		t.Fatalf("message count: json=%d html=%d", len(jsonTh.Messages), len(htmlTh.Messages))
	}
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
	if !reflect.DeepEqual(jSlugs, hSlugs) {
		t.Errorf("participant slugs differ: json=%v html=%v", jSlugs, hSlugs)
	}
	// Per-message bodies and timestamps.
	//
	// Reactions are a JSON-only feature (HTML exports do not expose
	// reaction metadata), so we compare bodies on their common ground:
	// the JSON body with its trailing "[reacted: ...]" suffix stripped.
	// Dual-path reaction coverage lives in TestImportDYI_ReactionsDualPath.
	for i := range jsonTh.Messages {
		jb := normalizeConvergence(stripReactionSuffix(jsonTh.Messages[i].Body))
		hb := normalizeConvergence(htmlTh.Messages[i].Body)
		if jb != hb {
			t.Errorf("message[%d] body differs:\n  json=%q\n  html=%q", i, jb, hb)
		}
		jt := jsonTh.Messages[i].SentAt.Truncate(time.Minute)
		ht := htmlTh.Messages[i].SentAt.Truncate(time.Minute)
		if !jt.Equal(ht) {
			t.Errorf("message[%d] timestamp differs: json=%v html=%v", i, jt, ht)
		}
		if Slug(jsonTh.Messages[i].SenderName) != Slug(htmlTh.Messages[i].SenderName) {
			t.Errorf("message[%d] sender differs: json=%q html=%q",
				i, jsonTh.Messages[i].SenderName, htmlTh.Messages[i].SenderName)
		}
	}
}
