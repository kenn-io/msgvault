package loops

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAsksQuestion(t *testing.T) {
	for _, tt := range []struct {
		body string
		want bool
	}{
		{"Can you send it?", true},
		{"All set.\n> Can you send it?", false},
		{"All set.\nOn Monday, Alex wrote:\nCan you send it?", false},
		{"On Monday, Alex wrote:\n> Can you send it?\n\nCan you confirm the date?", true},
		{"On Monday, Alex wrote:\nCan you send it?\n\nI will send it tomorrow.", false},
		{"On Monday, Alex wrote:\n> Can you send it?\nI can send it Friday. Can you confirm the address?", true},
		{"All set.\r\n-- \r\nQuestions?", false},
		{"FYI\n---------- Forwarded message ----------\nSubject: Which day works?\n\nCan you meet Friday?", false},
		{"Can you review this?\n---------- Forwarded message ----------\nSubject: Which day works?", true},
		{"See https://example.test/?q=one and www.example.test/?two", false},
		{"Can you review https://example.test/proposal?", true},
		{"See https://example.test/proposal?version=2", false},
		{"See example.test/help?topic=1", false},
		{"Can you review example.test/proposal?", true},
		{"See https://example.test/?q=one\nCan you check?", true},
		{"", false},
		{"Can you send it?\n-- \nAlex Example", true},
	} {
		t.Run(tt.body, func(t *testing.T) { assert.Equal(t, tt.want, asksQuestion(tt.body)) })
	}
}

func TestAcknowledgementOnly(t *testing.T) {
	participants := []LoopParticipant{
		{DisplayName: "Alex Example"},
		{DisplayName: "Avery Example"},
		{DisplayName: "Jordan"},
		{DisplayName: "Émile"},
	}
	for _, tt := range []struct {
		body string
		want bool
	}{
		{"Thanks, Alex!", true},
		{"Thanks, avery!", true},
		{"Thanks, Émile!", true},
		{"Thanks, Avery Example!", true},
		{"THANK YOU, Avery Example!", true},
		{"Thanks, Avery and Jordan!", true},
		{"Thanks, Avery & Jordan!", true},
		{"Thank you. Got it!", true},
		{"Thank you very much!", true},
		{"Thanks a lot!", true},
		{"Thanks so much!", true},
		{"Many many thanks!", true},
		{"  OK\n  thanks! ", true},
		{"Thanks!\n> Can you send it?", true},
		{"Thanks!\n-- \nAlex Example", true},
		{"Thanks! https://example.test/report.pdf", false},
		{"Thanks, sent!", false},
		{"Thanks, no.", false},
		{"Will do", false},
		{"Thanks, I will send it tomorrow", false},
		{"Thanks, I will send", false},
		{"Thanks?", false},
		{"", false},
		{strings.Repeat("thanks ", 10), false},
	} {
		t.Run(tt.body, func(t *testing.T) { assert.Equal(t, tt.want, isAcknowledgementOnly(tt.body, participants)) })
	}
}

func FuzzQuestionQuotedSuffix(f *testing.F) {
	f.Add("Can you send it?", "Any news?")
	f.Add("All set", "Check https://example.test/?q=one")
	f.Fuzz(func(t *testing.T, body, quote string) {
		if len(body)+len(quote) > 4096 {
			t.Skip()
		}
		quoted := "> " + strings.ReplaceAll(strings.ReplaceAll(quote, "\r", "\n"), "\n", "\n> ")
		assert.Equal(t, asksQuestion(body), asksQuestion(body+"\n"+quoted))
	})
}
