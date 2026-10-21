package loops

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAsks(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{"question", "Can you send it?", true},
		{"quoted question", "All set.\n> Can you send it?", false},
		{"authored request before an attributed quote", "On Monday, can you send the contract?\nOn Sunday, Alex wrote:\n> Earlier message", true},
		{"attribution", "All set.\nOn Monday, Alex wrote:\nCan you send it?", false},
		{"wrapped attribution", "All set.\nOn Monday, October 12, 2026 at 9:00 AM,\nAlex <alex@example.test> wrote:\nCan you send it?", false},
		{"request word in a link", "Notes at https://example.test/lmk", false},
		{"quoted reply", "All set.\n\nOn Monday, Alex wrote:\n> Can you send it?\n> Thanks", false},
		{"signature", "All set.\r\n-- \r\nQuestions?", false},
		{"question mark in a link", "See https://example.test/?q=one and www.example.test/?two", false},
		{"question after a link", "See https://example.test/proposal?", true},
		{"query string only", "See https://example.test/proposal?version=2", false},
		{"bare domain query", "See example.test/help?topic=1", false},
		{"link then question", "See https://example.test/?q=one\nCan you check?", true},
		{"empty", "", false},
		{"question above signature", "Can you send it?\n-- \nAlex Example", true},
		{"can you", "Can you send the contract", true},
		{"could you", "Could you look at this when free.", true},
		{"will you", "Will you be there", true},
		{"let me know", "Let me know when it ships.", true},
		{"lmk", "lmk", true},
		{"let us know", "Please let us know.", true},
		{"please send", "Please send the deck.", true},
		{"please kindly confirm", "Please kindly confirm receipt.", true},
		{"please alone", "Please enjoy the weekend.", false},
		{"statement", "See you Saturday!", false},
		{"original message", "Done.\n\n-----Original Message-----\nFrom: Alex\nCan you send it?", false},
		{"outlook header pair", "Done.\n\nFrom: Alex Example\nSent: Monday, October 12\nTo: Me\nCan you send it?", false},
		{"forwarded", "FYI\n---------- Forwarded message ---------\nFrom: Sam\nCan you send it?", false},
		{"forwarded body without an authored request", "FYI\n\nBegin forwarded message:\nFrom: Sam <sam@example.test>\nDate: Monday, October 19, 2026\n\nCan you send it?", false},
		{"from line alone", "From: the office\nCan you send it?", true},
	} {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, asks(ownText(tt.body))) })
	}

	const quote = "On Monday, October 12, 2026 at 9:00 AM,\nMe <test@example.com> wrote:\n> Can you approve the contract?\n"
	for _, tt := range []struct {
		name, body, want string
	}{
		{"three-line attribution only", "On Monday, October 12, 2026,\nat 9:00 AM,\nMe <test@example.com> wrote:\n> Can you approve the contract?\n", ""},
		{"approval above wrapped quote", "Approved, go ahead.\n\n" + quote, "Approved, go ahead."},
		{"approval below wrapped quote", quote + "\nApproved, go ahead.", "Approved, go ahead."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, strings.TrimSpace(ownText(tt.body)))
		})
	}
}

func TestAcknowledgementOnly(t *testing.T) {
	for _, tt := range []struct {
		body string
		want bool
	}{
		{"THANK YOU!", true},
		{"Thank you. Got it!", true},
		{"  OK\n  thanks! ", true},
		{"Thanks!\n> Can you send it?", true},
		{"Thanks!\n\nOn Monday,\nAlex wrote:\n> Can you send it?", true},
		{"Thanks!\n-- \nAlex Example", true},
		{"Thanks! https://example.test/report.pdf", false},
		{"Thanks, sent!", false},
		{"Will do", false},
		{"Thanks, I will send it tomorrow", false},
		{"Thanks?", false},
		{"", false},
		{strings.Repeat("thanks ", 10), false},
	} {
		t.Run(tt.body, func(t *testing.T) { assert.Equal(t, tt.want, isAcknowledgementOnly(ownText(tt.body))) })
	}
}

func FuzzAsksIgnoresQuotedSuffix(f *testing.F) {
	f.Add("Can you send it?", "Any news?")
	f.Add("All set", "Check https://example.test/?q=one")
	f.Fuzz(func(t *testing.T, body, quote string) {
		if len(body)+len(quote) > 4096 {
			t.Skip()
		}
		quoted := "> " + strings.ReplaceAll(strings.ReplaceAll(quote, "\r", "\n"), "\n", "\n> ")
		assert.Equal(t, asks(ownText(body)), asks(ownText(body+"\n"+quoted)))
	})
}
