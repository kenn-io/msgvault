package vcard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizePhone(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"+447700900000", "+447700900000"},
		{"+12025551234", "+12025551234"},
		{"+33624921221", "+33624921221"},
		{"+44 7700 900000", "+447700900000"},
		{"+1-202-555-1234", "+12025551234"},
		{"+44 (0)7700 900000", "+447700900000"},
		{"+44(0)20 7123 4567", "+442071234567"},
		{"003-362-4921221", "+33624921221"},
		{"0033624921221", "+33624921221"},
		{"004-479-35975580", "+447935975580"},
		// Local/ambiguous — skipped.
		{"011-585-73843", ""},
		{"07738006043", ""},
		{"077-380-06043", ""},
		{"447700900000", ""},
		{"2025551234", ""},
		{"", ""},
		{"   ", ""},
		{"abc", ""},
		{"12", ""},
	}

	for _, tt := range tests {
		if got := normalizePhone(tt.raw); got != tt.want {
			t.Errorf("normalizePhone(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestParseFile(t *testing.T) {
	vcf := `BEGIN:VCARD
VERSION:2.1
N:McGregor;Alastair;;;
FN:Alastair McGregor
TEL;CELL:+447984959428
END:VCARD
BEGIN:VCARD
VERSION:2.1
N:France;Geoff;;;
FN:Geoff France
TEL;X-Mobile:+33562645735
END:VCARD
BEGIN:VCARD
VERSION:2.1
N:Studios;Claire Mohacek -;Amazon;;
FN:Claire Mohacek - Amazon Studios
TEL;CELL:077-380-06043
END:VCARD
BEGIN:VCARD
VERSION:2.1
TEL;CELL:
END:VCARD
BEGIN:VCARD
VERSION:3.0
FN:Multi Phone Person
TEL;TYPE=CELL:+447700900001
TEL;TYPE=WORK:+442071234567
END:VCARD
`
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vcf")
	if err := os.WriteFile(path, []byte(vcf), 0644); err != nil {
		t.Fatal(err)
	}

	contacts, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile() error: %v", err)
	}
	if len(contacts) != 4 {
		t.Fatalf("got %d contacts, want 4", len(contacts))
	}

	if contacts[0].FullName != "Alastair McGregor" {
		t.Errorf("contact 0 name = %q", contacts[0].FullName)
	}
	if len(contacts[0].Phones) != 1 || contacts[0].Phones[0] != "+447984959428" {
		t.Errorf("contact 0 phones = %v", contacts[0].Phones)
	}

	if contacts[2].FullName != "Claire Mohacek - Amazon Studios" {
		t.Errorf("contact 2 name = %q", contacts[2].FullName)
	}
	if len(contacts[2].Phones) != 0 {
		t.Errorf("contact 2 phones = %v, want [] (local numbers skipped)", contacts[2].Phones)
	}

	if contacts[3].FullName != "Multi Phone Person" {
		t.Errorf("contact 3 name = %q", contacts[3].FullName)
	}
	if len(contacts[3].Phones) != 2 {
		t.Errorf("contact 3 phone count = %d, want 2", len(contacts[3].Phones))
	}
}

func TestParseFile_FoldedAndEncoded(t *testing.T) {
	vcf := "BEGIN:VCARD\r\n" +
		"VERSION:2.1\r\n" +
		"FN:José\r\n" +
		" García\r\n" +
		"TEL;CELL:+34\r\n" +
		" 612345678\r\n" +
		"END:VCARD\r\n" +
		"BEGIN:VCARD\r\n" +
		"VERSION:2.1\r\n" +
		"FN;ENCODING=QUOTED-PRINTABLE:Ren=C3=A9 Dupont\r\n" +
		"TEL;CELL:+33612345678\r\n" +
		"END:VCARD\r\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "folded.vcf")
	if err := os.WriteFile(path, []byte(vcf), 0644); err != nil {
		t.Fatal(err)
	}

	contacts, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile() error: %v", err)
	}
	if len(contacts) != 2 {
		t.Fatalf("got %d contacts, want 2", len(contacts))
	}

	if contacts[0].FullName != "JoséGarcía" {
		t.Errorf("folded name = %q", contacts[0].FullName)
	}
	if len(contacts[0].Phones) != 1 || contacts[0].Phones[0] != "+34612345678" {
		t.Errorf("folded phone = %v", contacts[0].Phones)
	}

	if contacts[1].FullName != "René Dupont" {
		t.Errorf("QP name = %q", contacts[1].FullName)
	}
}

func TestParseFile_QPSoftBreaks(t *testing.T) {
	vcf := "BEGIN:VCARD\r\n" +
		"VERSION:2.1\r\n" +
		"FN;ENCODING=QUOTED-PRINTABLE:Jo=C3=A3o da =\r\n" +
		"Silva\r\n" +
		"TEL;CELL:+5511999887766\r\n" +
		"END:VCARD\r\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "qp-soft.vcf")
	if err := os.WriteFile(path, []byte(vcf), 0644); err != nil {
		t.Fatal(err)
	}

	contacts, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile() error: %v", err)
	}
	if len(contacts) != 1 {
		t.Fatalf("got %d contacts, want 1", len(contacts))
	}
	if contacts[0].FullName != "João da Silva" {
		t.Errorf("QP soft break name = %q", contacts[0].FullName)
	}
}

func TestParseFile_QPSoftBreakWithFoldedContinuation(t *testing.T) {
	vcf := "BEGIN:VCARD\r\n" +
		"VERSION:2.1\r\n" +
		"FN;ENCODING=QUOTED-PRINTABLE:Jo=C3=A3o da =\r\n" +
		" Silva\r\n" +
		"TEL;CELL:+5511999887766\r\n" +
		"END:VCARD\r\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "qp-folded-soft.vcf")
	if err := os.WriteFile(path, []byte(vcf), 0644); err != nil {
		t.Fatal(err)
	}

	contacts, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile() error: %v", err)
	}
	if len(contacts) != 1 {
		t.Fatalf("got %d contacts, want 1", len(contacts))
	}
	if contacts[0].FullName != "João da Silva" {
		t.Errorf("QP folded soft break name = %q", contacts[0].FullName)
	}
}

func TestParseFile_Base64PhotoEqualsPaddingDoesNotEatNextLine(t *testing.T) {
	// Apple's modern vCard PHOTO blobs are base64 with '=' padding. The QP
	// soft-break logic must NOT splice such lines into the following
	// END:VCARD, or the contact gets silently dropped.
	vcf := "BEGIN:VCARD\r\n" +
		"VERSION:3.0\r\n" +
		"N:Baum;Bryan;;;\r\n" +
		"FN:Bryan Baum\r\n" +
		"TEL;type=pref:+13052068533\r\n" +
		"PHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQSkZJRgABAQAA=\r\n" +
		"END:VCARD\r\n" +
		"BEGIN:VCARD\r\n" +
		"VERSION:3.0\r\n" +
		"FN:Next Person\r\n" +
		"TEL:+15550000001\r\n" +
		"END:VCARD\r\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "photo.vcf")
	if err := os.WriteFile(path, []byte(vcf), 0644); err != nil {
		t.Fatal(err)
	}

	contacts, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile() error: %v", err)
	}
	if len(contacts) != 2 {
		t.Fatalf("got %d contacts, want 2 — base64 '=' padding swallowed END:VCARD", len(contacts))
	}
	if contacts[0].FullName != "Bryan Baum" {
		t.Errorf("contact 0 name = %q, want %q", contacts[0].FullName, "Bryan Baum")
	}
	if len(contacts[0].Phones) != 1 || contacts[0].Phones[0] != "+13052068533" {
		t.Errorf("contact 0 phones = %v, want [+13052068533]", contacts[0].Phones)
	}
	if contacts[1].FullName != "Next Person" {
		t.Errorf("contact 1 name = %q, want %q (next contact got merged into the photo)", contacts[1].FullName, "Next Person")
	}
}

func TestParseFile_Emails(t *testing.T) {
	vcf := "BEGIN:VCARD\r\n" +
		"VERSION:3.0\r\n" +
		"FN:Alice Example\r\n" +
		"EMAIL;TYPE=INTERNET:Alice@Example.com\r\n" +
		"EMAIL;TYPE=WORK:alice.work@example.com\r\n" +
		"TEL:+15551234567\r\n" +
		"END:VCARD\r\n" +
		"BEGIN:VCARD\r\n" +
		"VERSION:3.0\r\n" +
		"FN:Bob Email-Only\r\n" +
		"EMAIL:bob@example.com\r\n" +
		"END:VCARD\r\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "emails.vcf")
	if err := os.WriteFile(path, []byte(vcf), 0644); err != nil {
		t.Fatal(err)
	}

	contacts, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile() error: %v", err)
	}
	if len(contacts) != 2 {
		t.Fatalf("got %d contacts, want 2", len(contacts))
	}

	if len(contacts[0].Emails) != 2 ||
		contacts[0].Emails[0] != "alice@example.com" ||
		contacts[0].Emails[1] != "alice.work@example.com" {
		t.Errorf("contact 0 emails = %v", contacts[0].Emails)
	}

	if len(contacts[1].Emails) != 1 || contacts[1].Emails[0] != "bob@example.com" {
		t.Errorf("contact 1 emails = %v", contacts[1].Emails)
	}
	if len(contacts[1].Phones) != 0 {
		t.Errorf("contact 1 phones = %v", contacts[1].Phones)
	}
}

func TestParseFile_GroupedProperties(t *testing.T) {
	vcf := "BEGIN:VCARD\r\n" +
		"VERSION:3.0\r\n" +
		"item1.FN:Grouped Person\r\n" +
		"item2.TEL;TYPE=CELL:+15551234567\r\n" +
		"item3.EMAIL;TYPE=INTERNET:Grouped@Example.com\r\n" +
		"END:VCARD\r\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "grouped.vcf")
	if err := os.WriteFile(path, []byte(vcf), 0644); err != nil {
		t.Fatal(err)
	}

	contacts, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile() error: %v", err)
	}
	if len(contacts) != 1 {
		t.Fatalf("got %d contacts, want 1", len(contacts))
	}
	if contacts[0].FullName != "Grouped Person" {
		t.Errorf("grouped name = %q", contacts[0].FullName)
	}
	if len(contacts[0].Phones) != 1 || contacts[0].Phones[0] != "+15551234567" {
		t.Errorf("grouped phones = %v", contacts[0].Phones)
	}
	if len(contacts[0].Emails) != 1 || contacts[0].Emails[0] != "grouped@example.com" {
		t.Errorf("grouped emails = %v", contacts[0].Emails)
	}
}

func TestDecodeQuotedPrintable(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"Ren=C3=A9", "René"},
		{"=C3=A9=C3=A8", "éè"},
		{"no=encoding", "no=encoding"},
		{"end=", "end="},
	}
	for _, tt := range tests {
		if got := decodeQuotedPrintable(tt.input); got != tt.want {
			t.Errorf("decodeQuotedPrintable(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExtractValue(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{"FN:John Doe", "John Doe"},
		{"FN;CHARSET=UTF-8:John Doe", "John Doe"},
		{"TEL;CELL:+447700900000", "+447700900000"},
		{"TEL;TYPE=CELL:+447700900000", "+447700900000"},
		{"TEL:+447700900000", "+447700900000"},
		{"NO_COLON", ""},
	}
	for _, tt := range tests {
		if got := extractValue(tt.line); got != tt.want {
			t.Errorf("extractValue(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestNormalizedPropertyKey(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{"FN:John Doe", "FN"},
		{"FN;CHARSET=UTF-8:John Doe", "FN"},
		{"item1.FN:John Doe", "FN"},
		{"item2.TEL;TYPE=CELL:+447700900000", "TEL"},
		{"item3.EMAIL;TYPE=INTERNET:alice@example.com", "EMAIL"},
		{"NO_COLON", ""},
	}
	for _, tt := range tests {
		if got := normalizedPropertyKey(tt.line); got != tt.want {
			t.Errorf("normalizedPropertyKey(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}
