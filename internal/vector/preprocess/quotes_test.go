package preprocess

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

var replyQuoteCases = []struct{ name, body, want string }{
	{"wrapped attribution with reply below", "Thanks!\n\nOn Monday,\nAlex Example wrote:\n  > Can you send it?\n\nThe report is attached.", "Thanks!\n\n\nThe report is attached."},
	{"indented quotation", "Received.\n  > Can you send it?", "Received."},
	{"unmarked quoted tail", "Thanks!\n\n On Monday, Alex wrote:\nCan you send it?", "Thanks!"},
	{"three-line attribution only", "On Monday, October 12, 2026,\nat 9:00 AM,\nMe <test@example.com> wrote:\n> Can you approve the contract?\n", ""},
	{"approval below wrapped attribution with blank line", "On Monday, October 12, 2026 at 9:00 AM,\nAlex <alex@example.test> wrote:\n\n> Can you approve the contract?\n\nApproved, go ahead.", "Approved, go ahead."},
	{"bottom-posted answer after an indented quote", "On Monday, Me wrote:\n\n  > Can you send the contract?\n\nI sent it.", "I sent it."},
	{"quoted question", "All set.\n> Can you send it?", "All set."},
	{"authored request before an attributed quote", "On Monday, can you send the contract?\nOn Sunday, Alex wrote:\n> Earlier message", "On Monday, can you send the contract?"},
	{"wrapped attribution", "All set.\nOn Monday, October 12, 2026 at 9:00 AM\nAlex <alex@example.test> wrote:\nCan you send it?", "All set."},
	{"quoted reply", "All set.\n\nOn Monday, Alex wrote:\n> Can you send it?\n> Thanks", "All set."},
	{"original message", "Done.\n\n-----Original Message-----\nFrom: Alex\nCan you send it?", "Done."},
	{"outlook header pair", "Done.\n\nFrom: Alex Example\nSent: Monday, October 12\nTo: Me\nCan you send it?", "Done."},
	{"forwarded", "FYI\n---------- Forwarded message ---------\nFrom: Sam\nCan you send it?", "FYI"},
	{"forwarded body without an authored request", "FYI\n\nBegin forwarded message:\nFrom: Sam <sam@example.test>\nDate: Monday, October 19, 2026\n\nCan you send it?", "FYI"},
	{"multibyte CRLF", "Merci\r\n> Déjà reçu\r\nRéponse", "Merci\r\nRéponse"},
	{"from line alone", "From: the office\nCan you send it?", "From: the office\nCan you send it?"},
}

func TestStripReplyQuotes(t *testing.T) {
	for _, tc := range replyQuoteCases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.TrimSpace(StripReplyQuotes(tc.body))
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMaskReplyQuotes(t *testing.T) {
	for _, tc := range replyQuoteCases {
		t.Run(tc.name, func(t *testing.T) {
			masked := MaskReplyQuotes(tc.body)
			assert.Equal(t, len(tc.body), len(masked))
			for i := range tc.body {
				if tc.body[i] == '\r' || tc.body[i] == '\n' {
					assert.Equal(t, tc.body[i], masked[i], "line ending at byte %d", i)
				}
			}
		})
	}
	for _, tc := range []struct{ name, body, want string }{
		{"authored text", "Hello\nWorld", "Hello\nWorld"},
		{"quoted line", "Hi\n> q\nOK", "Hi\n   \nOK"},
		{"multibyte line with CRLF", "Hi\r\n> reçu\r\nOK", "Hi\r\n       \r\nOK"},
		{"answer below quote block", "On Monday, A wrote:\n> q\nOK", "                   \n   \nOK"},
		{"unmarked tail", "On Monday, A wrote:\nq\nOK", "                   \n \n  "},
		{"separated Outlook headers", "hello\nFrom: a\nOn Monday, A wrote:\n> q\nSent: b", "hello\nFrom: a\n                   \n   \nSent: b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, MaskReplyQuotes(tc.body))
		})
	}
}
