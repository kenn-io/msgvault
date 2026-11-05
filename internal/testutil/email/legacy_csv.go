package email

// LegacyCSVOptions configures the legacy CSV generator fixture.
// Payload contains quoted-printable wire text.
type LegacyCSVOptions struct {
	Body      string
	InlinePNG bool
	Payload   string
	Header    string
	ExtraPNG  bool
	TwoCSVs   bool
	StrayLine string
}

// LegacyCSV builds a message with an optional stray delimiter before CSV headers.
func LegacyCSV(withStrayLine bool, shape LegacyCSVOptions) []byte {
	stray := ""
	if withStrayLine {
		stray = shape.StrayLine
		if stray == "" {
			stray = "--------------040908010102080206010104"
		}
		stray += "\r\n"
	}
	if shape.Body == "" {
		shape.Body = "<p>Example body</p>"
	}
	if shape.Payload == "" {
		shape.Payload = "first,last\r\nSample,Person"
	}
	csv := "--related\r\n" + stray +
		"Content-Type: text/csv; name=example.csv\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"Content-Disposition: attachment; filename=example.csv\r\n" + shape.Header +
		"\r\n" + shape.Payload + "\r\n"
	if shape.TwoCSVs {
		csv += csv
	}
	extra := ""
	if shape.ExtraPNG || shape.InlinePNG {
		disposition := "attachment"
		contentID := ""
		if shape.InlinePNG {
			disposition = "inline"
			contentID = "Content-ID: <logo@example.org>\r\n"
		}
		extra = "--related\r\nContent-Type: image/png\r\n" +
			"Content-Transfer-Encoding: base64\r\n" +
			"Content-Disposition: " + disposition + "; filename=example.png\r\n" + contentID + "\r\niVBORw0KGgo=\r\n"
	}
	return []byte("From: sender@example.org\r\nTo: recipient@example.org\r\n" +
		"Subject: recovery fixture\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/related; boundary=related\r\n\r\n" +
		"--related\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + shape.Body + "\r\n" +
		csv + extra + "--related--\r\n")
}
