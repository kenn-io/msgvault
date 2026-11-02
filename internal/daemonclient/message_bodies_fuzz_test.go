package daemonclient

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func FuzzMessageBodyConversion(f *testing.F) {
	f.Add("", "<p>HTML only</p>", true)
	f.Add("<p>Same bytes</p>", "<p>Same bytes</p>", true)
	f.Add("plain", "<p>HTML</p>", false)
	f.Fuzz(func(t *testing.T, text, html string, canonical bool) {
		legacy := text
		if legacy == "" {
			legacy = html
		}
		wire := &generated.MessageDetail{Body: legacy, BodyHTML: new(html)}
		wantText := legacy
		if canonical {
			wire.BodyText = new(text)
			wantText = text
		}
		got := queryDetailFromAPIMessage(generatedDetailToAPIMessage(wire))
		assert.Equal(t, wantText, got.BodyText)
		assert.Equal(t, html, got.BodyHTML)
	})
}
