package sync

import testemail "go.kenn.io/msgvault/internal/testutil/email"

// testMIME returns a simple plain-text MIME message for testing.
// Returns a fresh byte slice on each call to prevent cross-test mutation.
func testMIME() []byte {
	return testemail.NewMessage().Bytes()
}

// testMIMEWithAttachment returns a MIME message with a binary attachment.
// Returns a fresh byte slice on each call to prevent cross-test mutation.
func testMIMEWithAttachment() []byte {
	return testemail.NewMessage().
		Subject("Test with Attachment").
		Body("This is the message body.").
		WithAttachment("test.bin", "application/octet-stream", []byte("Hello World!")).
		Bytes()
}

// testMIMENoSubject returns a MIME message with no Subject header.
// Returns a fresh byte slice on each call to prevent cross-test mutation.
func testMIMENoSubject() []byte {
	return testemail.NewMessage().
		NoSubject().
		Body("Message with no subject line.").
		Bytes()
}

// testMIMEMultipleRecipients returns a MIME message with To, Cc, and Bcc recipients.
// Returns a fresh byte slice on each call to prevent cross-test mutation.
func testMIMEMultipleRecipients() []byte {
	return testemail.NewMessage().
		To("to1@example.com, to2@example.com").
		Cc("cc1@example.com").
		Bcc("bcc1@example.com").
		Subject("Multiple Recipients").
		Body("Message with multiple recipients.").
		Bytes()
}

// testMIMEDuplicateRecipients returns a MIME message with duplicate addresses across To/Cc/Bcc.
// Returns a fresh byte slice on each call to prevent cross-test mutation.
func testMIMEDuplicateRecipients() []byte {
	return testemail.NewMessage().
		To(`duplicate@example.com, other@example.com, "Duplicate Person" <duplicate@example.com>`).
		Cc(`cc-dup@example.com, "CC Duplicate" <cc-dup@example.com>`).
		Bcc("bcc-dup@example.com, bcc-dup@example.com").
		Subject("Duplicate Recipients").
		Body("Message with duplicate recipients in To, Cc, and Bcc fields.").
		Bytes()
}
