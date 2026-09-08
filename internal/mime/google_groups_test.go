package mime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseGoogleGroupsHeaders(t *testing.T) {
	for _, tt := range []struct {
		name, raw, fallback string
		want                GoogleGroupsHeaders
	}{
		{name: "ordinary Gmail remains ordinary", raw: "X-GM-THRID: 123\nX-Gmail-Labels: Inbox\n\nbody"},
		{name: "other mailing list remains ordinary", raw: "X-BeenThere: list@example.com\nX-GM-THRID: 123\n\nbody"},
		{name: "lookalike domain remains ordinary", raw: "X-BeenThere: list@googlegroups.com.example.com\n\nbody"},
		{name: "public group fallback", raw: "X-BeenThere: test-group@GoogleGroups.com\r\nX-GM-THRID: 000123\r\n\r\nbody", want: GoogleGroupsHeaders{Group: "test-group", ThreadID: "123", Labels: []string{"test-group"}}},
		{name: "public archive identifier", raw: "X-GM-THRID: 123\n\nbody", fallback: " TEST-GROUP@GoogleGroups.com ", want: GoogleGroupsHeaders{Group: "test-group", ThreadID: "123", Labels: []string{"test-group"}}},
		{name: "public address in Groups header", raw: "X-Google-Groups: TEST-GROUP@GoogleGroups.com\nX-GM-THRID: 123\n\nbody", want: GoogleGroupsHeaders{Group: "test-group", ThreadID: "123", Labels: []string{"test-group"}}},
		{name: "explicit Workspace export", raw: "X-GM-THRID: 123\n\nbody", fallback: "Team@Example.com", want: GoogleGroupsHeaders{Group: "Team@Example.com", ThreadID: "123", Labels: []string{"Team@Example.com"}}},
		{name: "headers beat archive identifier", raw: "X-Google-Groups: test-group\nX-BeenThere: other@googlegroups.com\nX-GM-THRID: 123\n\nbody", fallback: "archive", want: GoogleGroupsHeaders{Group: "test-group", ThreadID: "123", Labels: []string{"test-group"}}},
		{name: "invalid thread uses email threading", raw: "X-Google-Groups: test-group\nX-GM-THRID: not-a-number\n\nbody", want: GoogleGroupsHeaders{Group: "test-group", Labels: []string{"test-group"}}},
		{name: "zero thread uses email threading", raw: "X-Google-Groups: test-group\nX-GM-THRID: 0\n\nbody", want: GoogleGroupsHeaders{Group: "test-group", Labels: []string{"test-group"}}},
		{name: "folded localized and quoted labels", raw: "X-Google-Groups: test-group\r\nX-Gmail-Labels: Starred,\r\n =?UTF-8?Q?R=C3=A9solu?=, \"Category, one\"\r\n\r\nbody", want: GoogleGroupsHeaders{Group: "test-group", Labels: []string{"test-group", "Starred", "Résolu", "Category, one"}}},
		{name: "malformed label bytes remain valid UTF-8", raw: "X-Google-Groups: test-group\nX-Gmail-Labels: Bad\xff label\n\nbody", want: GoogleGroupsHeaders{Group: "test-group", Labels: []string{"test-group", "Bad� label"}}},
		{name: "body headers ignored", raw: "Subject: Ordinary message\n\nX-Google-Groups: test-group\nX-GM-THRID: 123\n"},
	} {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, ParseGoogleGroupsHeaders([]byte(tt.raw), tt.fallback)) })
	}
}
