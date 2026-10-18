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
		{"All set.\r\n-- \r\nQuestions?", false},
		{"See https://example.test/?q=one and www.example.test/?two", false},
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
