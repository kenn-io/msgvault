package vcard

import (
	"os"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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
		// 10-digit numbers default to US (+1) so common Contacts.app exports
		// match iMessage handles that the importer normalized the same way.
		{"2025551234", "+12025551234"},
		{"(202) 555-1234", "+12025551234"},
		{"202-555-1234", "+12025551234"},
		// Non-US numbers without explicit country code are E.164-shaped but
		// won't correspond to real handles — iMessage's importer applies the
		// same defaulting, so within one archive the keys remain consistent.
		{"447700900000", "+447700900000"},
		{"07738006043", "+07738006043"},
		{"011-585-73843", "+01158573843"},
		// Empty / too-short / non-numeric — rejected.
		{"", ""},
		{"   ", ""},
		{"abc", ""},
		{"12", ""},
	}

	for _, tt := range tests {
		got := normalizePhone(tt.raw)
		assertpkg.Equal(t, tt.want, got, "normalizePhone(%q)", tt.raw)
	}
}

func TestParseFile(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(os.WriteFile(path, []byte(vcf), 0644))

	contacts, err := ParseFile(path)
	require.NoError(err, "ParseFile")
	require.Len(contacts, 4)

	assert.Equal("Alastair McGregor", contacts[0].FullName, "contact 0 name")
	assert.Equal([]string{"+447984959428"}, contacts[0].Phones, "contact 0 phones")

	assert.Equal("Claire Mohacek - Amazon Studios", contacts[2].FullName, "contact 2 name")
	assert.Equal([]string{"+07738006043"}, contacts[2].Phones, "contact 2 phones")

	assert.Equal("Multi Phone Person", contacts[3].FullName, "contact 3 name")
	assert.Len(contacts[3].Phones, 2, "contact 3 phone count")
}

func TestParseFile_FoldedAndEncoded(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(os.WriteFile(path, []byte(vcf), 0644))

	contacts, err := ParseFile(path)
	require.NoError(err, "ParseFile")
	require.Len(contacts, 2)

	assert.Equal("JoséGarcía", contacts[0].FullName, "folded name")
	assert.Equal([]string{"+34612345678"}, contacts[0].Phones, "folded phone")

	assert.Equal("René Dupont", contacts[1].FullName, "QP name")
}

func TestParseFile_QPSoftBreaks(t *testing.T) {
	require := requirepkg.New(t)
	vcf := "BEGIN:VCARD\r\n" +
		"VERSION:2.1\r\n" +
		"FN;ENCODING=QUOTED-PRINTABLE:Jo=C3=A3o da =\r\n" +
		"Silva\r\n" +
		"TEL;CELL:+5511999887766\r\n" +
		"END:VCARD\r\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "qp-soft.vcf")
	require.NoError(os.WriteFile(path, []byte(vcf), 0644))

	contacts, err := ParseFile(path)
	require.NoError(err, "ParseFile")
	require.Len(contacts, 1)
	assertpkg.Equal(t, "João da Silva", contacts[0].FullName, "QP soft break name")
}

func TestParseFile_QPSoftBreakWithFoldedContinuation(t *testing.T) {
	require := requirepkg.New(t)
	vcf := "BEGIN:VCARD\r\n" +
		"VERSION:2.1\r\n" +
		"FN;ENCODING=QUOTED-PRINTABLE:Jo=C3=A3o da =\r\n" +
		" Silva\r\n" +
		"TEL;CELL:+5511999887766\r\n" +
		"END:VCARD\r\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "qp-folded-soft.vcf")
	require.NoError(os.WriteFile(path, []byte(vcf), 0644))

	contacts, err := ParseFile(path)
	require.NoError(err, "ParseFile")
	require.Len(contacts, 1)
	assertpkg.Equal(t, "João da Silva", contacts[0].FullName, "QP folded soft break name")
}

func TestParseFile_Base64PhotoEqualsPaddingDoesNotEatNextLine(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(os.WriteFile(path, []byte(vcf), 0644))

	contacts, err := ParseFile(path)
	require.NoError(err, "ParseFile")
	require.Len(contacts, 2, "base64 '=' padding swallowed END:VCARD")
	assert.Equal("Bryan Baum", contacts[0].FullName, "contact 0 name")
	assert.Equal([]string{"+13052068533"}, contacts[0].Phones, "contact 0 phones")
	assert.Equal("Next Person", contacts[1].FullName, "contact 1 name (next contact got merged into the photo)")
}

func TestParseFile_Emails(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(os.WriteFile(path, []byte(vcf), 0644))

	contacts, err := ParseFile(path)
	require.NoError(err, "ParseFile")
	require.Len(contacts, 2)

	assert.Equal([]string{"alice@example.com", "alice.work@example.com"}, contacts[0].Emails, "contact 0 emails")

	assert.Equal([]string{"bob@example.com"}, contacts[1].Emails, "contact 1 emails")
	assert.Empty(contacts[1].Phones, "contact 1 phones")
}

func TestParseFile_GroupedProperties(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	vcf := "BEGIN:VCARD\r\n" +
		"VERSION:3.0\r\n" +
		"item1.FN:Grouped Person\r\n" +
		"item2.TEL;TYPE=CELL:+15551234567\r\n" +
		"item3.EMAIL;TYPE=INTERNET:Grouped@Example.com\r\n" +
		"END:VCARD\r\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "grouped.vcf")
	require.NoError(os.WriteFile(path, []byte(vcf), 0644))

	contacts, err := ParseFile(path)
	require.NoError(err, "ParseFile")
	require.Len(contacts, 1)
	assert.Equal("Grouped Person", contacts[0].FullName, "grouped name")
	assert.Equal([]string{"+15551234567"}, contacts[0].Phones, "grouped phones")
	assert.Equal([]string{"grouped@example.com"}, contacts[0].Emails, "grouped emails")
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
		got := decodeQuotedPrintable(tt.input)
		assertpkg.Equal(t, tt.want, got, "decodeQuotedPrintable(%q)", tt.input)
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
		got := extractValue(tt.line)
		assertpkg.Equal(t, tt.want, got, "extractValue(%q)", tt.line)
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
		got := normalizedPropertyKey(tt.line)
		assertpkg.Equal(t, tt.want, got, "normalizedPropertyKey(%q)", tt.line)
	}
}
