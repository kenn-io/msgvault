package mailkind

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildPacketProjection(t *testing.T) {
	in := Input{MessageType: "email", SenderEmail: "Merchant <orders@shop.example>", Subject: "Receipt for 123456",
		BodyText:            "Paid 123456 for buyer@example.com.\n> old quoted private text\n-- \nSignature private text",
		AttachmentMIMETypes: []string{"application/pdf; name=private-invoice.pdf", "application/pdf", "bad type"}}
	packet, ok := BuildPacket(in)
	require.True(t, ok)
	assert.Equal(t, Packet{SchemaVersion: PacketSchema, SenderDomain: "shop.example",
		Subject: "Receipt for [digits]", Excerpt: "Paid [digits] for [local]@example.com.",
		Attachments: map[string]int{"application/pdf": 2, "application/octet-stream": 1}, Tier0Types: []string{}}, packet)
	encoded, err := json.Marshal(packet)
	require.NoError(t, err)
	for _, private := range []string{"123456", "buyer@", "quoted private", "Signature", "private-invoice", "Merchant"} {
		assert.NotContains(t, string(encoded), private)
	}
}

func TestBuildPacketHTMLAndBounds(t *testing.T) {
	packet, ok := BuildPacket(Input{Subject: "Your receipt", BodyHTML: `<div>Paid <b>12</b> EUR.<script>script private</script><style>style private</style><div class="gmail_quote">quoted private</div></div>`})
	require.True(t, ok)
	assert.Equal(t, "Paid 12 EUR.", packet.Excerpt)
	packet, ok = BuildPacket(Input{BodyHTML: `<head><title>Hidden title</title><style>hidden style</style><p>Visible receipt body</p>`})
	require.True(t, ok)
	assert.Equal(t, "Visible receipt body", packet.Excerpt)
	packet, ok = BuildPacket(Input{BodyHTML: `<head><title>Hidden title</title>Visible receipt body`})
	require.True(t, ok)
	assert.Equal(t, "Visible receipt body", packet.Excerpt)
	for _, in := range []Input{
		{MessageType: "chat", Subject: "receipt"},
		{IsFromMe: true, Subject: "receipt"},
		{Subject: "Your verification code", BodyText: "Use code 432198 to sign in."},
		{Subject: "Order", BodyHTML: `<script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"ORD-A"}</script>`},
		{Subject: "receipt", BodyText: strings.Repeat("x", (1<<20)+1)},
		{Subject: "receipt", BodyHTML: strings.Repeat("<b></b>", 17000)},
	} {
		_, ok := BuildPacket(in)
		assert.False(t, ok)
	}
	packet, ok = BuildPacket(Input{Subject: strings.Repeat("界", 300), BodyText: strings.Repeat("界", 1600)})
	require.True(t, ok)
	assert.Equal(t, 256, utf8.RuneCountInString(packet.Subject))
	assert.Equal(t, 1500, utf8.RuneCountInString(packet.Excerpt))
}

func TestPacketMasksBeforeTruncation(t *testing.T) {
	assert.Equal(t, "[local]@example.com [local]@example.org [digits]", boundedMasked(`"Example Person"@example.com 例子@example.org １２３４５`, 100))
	assert.Equal(t, "[local]@ex", boundedMasked("longprivatepart@example.com", 10))
	assert.Equal(t, "[local]@例子.example.test [local]@[address]", boundedMasked("recipient@例子.example.test admin@[IPv6:2001:db8::1]", 100))

	packet, ok := BuildPacket(Input{
		Subject:  strings.Repeat("x", 245) + "recipient@例子.example.test",
		BodyText: "Contact admin@[IPv6:2001:db8::1] about this notice.",
	})
	require.True(t, ok)
	assert.NotContains(t, packet.Subject, "recipient")
	assert.Contains(t, packet.Subject, "[local]@")
	assert.NotContains(t, packet.Excerpt, "admin")
	assert.Contains(t, packet.Excerpt, "[local]@[address]")
}

func FuzzMailKindPacket(f *testing.F) {
	for _, body := range []string{"hello", "<script>secret</script>", "\xff private@example.com 123456", "> quoted\nnew"} {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body string) {
		if len(body) > 1<<20 {
			t.Skip()
		}
		masked := boundedMasked(body, 1500)
		assert.True(t, utf8.ValidString(masked))
		assert.LessOrEqual(t, utf8.RuneCountInString(masked), 1500)
		packet, ok := BuildPacket(Input{Subject: "Fixture purchase", BodyHTML: body})
		if ok {
			assert.LessOrEqual(t, utf8.RuneCountInString(packet.Excerpt), 1500)
			assert.Equal(t, PacketSchema, packet.SchemaVersion)
		}
	})
}
