package dedup

import (
	"bytes"
	"testing"
)

func TestNormalizeRawMIME(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		wantSame bool   // true if output should equal input
		contains string // substring the output must contain
		excludes string // substring the output must NOT contain
	}{
		{
			name:     "strips Received header (CRLF)",
			input:    []byte("Received: from mx1.google.com\r\nFrom: alice@example.com\r\nSubject: Hi\r\n\r\nBody"),
			contains: "From: alice@example.com",
			excludes: "Received",
		},
		{
			name:     "strips multiple transport headers",
			input:    []byte("Delivered-To: bob@example.com\r\nX-Gmail-Labels: INBOX\r\nAuthentication-Results: spf=pass\r\nFrom: alice@example.com\r\nSubject: Test\r\n\r\nBody"),
			contains: "From: alice@example.com",
			excludes: "Delivered-To",
		},
		{
			name:     "preserves non-transport headers",
			input:    []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Meeting\r\nDate: Mon, 1 Jan 2024 12:00:00 +0000\r\n\r\nBody text"),
			contains: "Subject: Meeting",
		},
		{
			name:     "handles LF-only line endings",
			input:    []byte("Received: from mx1\nFrom: alice@example.com\nSubject: Test\n\nBody with LF"),
			contains: "From: alice@example.com",
			excludes: "Received",
		},
		{
			name:     "no header/body separator returns raw unchanged",
			input:    []byte("This is just a blob of text with no headers"),
			wantSame: true,
		},
		{
			name:     "empty body preserved",
			input:    []byte("From: alice@example.com\r\nSubject: Empty\r\n\r\n"),
			contains: "Subject: Empty",
		},
		{
			name:     "preserves body content exactly",
			input:    []byte("Received: from mx1\r\nFrom: a@b.com\r\n\r\nExact body content here."),
			contains: "Exact body content here.",
		},
		{
			name:     "LF headers with CRLF in body uses earliest boundary",
			input:    []byte("From: a@b.com\nSubject: Test\n\nBody has \r\n\r\n inside"),
			contains: "Body has \r\n\r\n inside",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputCopy := make([]byte, len(tt.input))
			copy(inputCopy, tt.input)

			result := normalizeRawMIME(tt.input)

			if !bytes.Equal(tt.input, inputCopy) {
				t.Error("normalizeRawMIME mutated its input buffer")
			}

			if tt.wantSame {
				if !bytes.Equal(result, tt.input) {
					t.Errorf("expected unchanged output, got:\n%s", result)
				}
				return
			}
			if tt.contains != "" && !bytes.Contains(result, []byte(tt.contains)) {
				t.Errorf("output missing %q:\n%s", tt.contains, result)
			}
			if tt.excludes != "" && bytes.Contains(result, []byte(tt.excludes)) {
				t.Errorf("output should not contain %q:\n%s", tt.excludes, result)
			}
		})
	}
}

func TestNormalizeRawMIME_DeterministicOutput(t *testing.T) {
	raw1 := []byte("Received: from mx1.google.com\r\nFrom: sender@example.com\r\nSubject: Meeting\r\nDate: Mon, 1 Jan 2024 12:00:00 +0000\r\n\r\nLet's meet at 3pm.")
	raw2 := []byte("Received: from mx2.google.com\r\nDelivered-To: other@example.com\r\nFrom: sender@example.com\r\nSubject: Meeting\r\nDate: Mon, 1 Jan 2024 12:00:00 +0000\r\n\r\nLet's meet at 3pm.")

	hash1 := sha256Hex(normalizeRawMIME(raw1))
	hash2 := sha256Hex(normalizeRawMIME(raw2))
	if hash1 != hash2 {
		t.Errorf("same message with different transport headers produced different hashes")
	}
}
