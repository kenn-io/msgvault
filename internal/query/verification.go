package query

import (
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/verification"
)

// VerificationInput must be built from a complete canonical detail, never a
// summary or body page. It does not mutate the engine-owned detail.
func VerificationInput(msg *MessageDetail) verification.Input {
	in := verification.Input{
		MessageType: msg.MessageType, Subject: msg.Subject, Snippet: msg.Snippet,
		BodyText: msg.BodyText, BodyHTML: msg.BodyHTML, SentAt: msg.SentAt, IsFromMe: msg.IsFromMe,
	}
	if msg.ReceivedAt != nil {
		in.ReceivedAt = *msg.ReceivedAt
	}
	if len(msg.From) > 0 {
		in.SenderEmail = msg.From[0].Email
	}
	return in
}

// PrepareVerification returns a copied detail. The policy decision remains at
// the authorized API/MCP surface. Every body variant is masked before paging.
func PrepareVerification(msg *MessageDetail, reveal bool, now time.Time) *MessageDetail {
	if msg == nil {
		return nil
	}
	out := *msg
	if msg.Verification != nil && msg.Verification.Masked {
		return &out // a second boundary cannot recover an already-masked value
	}
	in := VerificationInput(msg)
	f := verification.Detect(in)
	if f == nil {
		return &out
	}
	if reveal {
		out.Verification = verification.Describe(in, f, false, nil, now)
		return &out
	}
	m := verification.NewMasker(f)
	var fields []string
	text := func(name string, value string) string {
		masked := m.Text(value)
		if masked != value && !slices.Contains(fields, name) {
			fields = append(fields, name)
		}
		return masked
	}
	out.Subject = text("subject", msg.Subject)
	// Stored previews may end inside a credential. Their offsets cannot be
	// mapped to canonical content, so withhold them on a finding.
	if msg.Snippet != "" {
		out.Snippet = verification.ContentPlaceholder
		fields = append(fields, "snippet")
	}
	out.BodyText = text("body_text", msg.BodyText)
	out.BodyHTML = m.HTML(msg.BodyHTML)
	if out.BodyHTML != msg.BodyHTML {
		fields = append(fields, "body_html")
	}
	out.ConversationTitle = text("conversation_title", msg.ConversationTitle)
	out.WebURL = text("web_url", msg.WebURL)
	addresses := func(name string, values []Address) []Address {
		out := slices.Clone(values)
		for i := range out {
			out[i].Email = text(name, out[i].Email)
			out[i].Name = text(name, out[i].Name)
		}
		return out
	}
	out.From = addresses("from", msg.From)
	out.To = addresses("to", msg.To)
	out.Cc = addresses("cc", msg.Cc)
	out.Bcc = addresses("bcc", msg.Bcc)
	out.Labels = slices.Clone(msg.Labels)
	for i := range out.Labels {
		out.Labels[i] = text("labels", out.Labels[i])
	}
	out.Attachments = slices.Clone(msg.Attachments)
	for i := range out.Attachments {
		att := &out.Attachments[i]
		att.Filename = text("attachments", att.Filename)
		att.URL = text("attachments", att.URL)
		att.StoragePath = ""
		att.Docbank = nil // optional nested extraction is not a canonical body
	}
	if msg.Calendar != nil {
		out.Calendar = nil // typed calendar payload has independent text fields
		fields = append(fields, "calendar")
	}
	out.Verification = verification.Describe(in, f, true, fields, now)
	return &out
}

// PrepareSummaryVerification masks an existing result with values detected in
// its full canonical message. It never emits canonical bodies to a search-only
// reader. Derived excerpts with unmappable offsets are withheld on findings.
func PrepareSummaryVerification(row MessageSummary, canonical *MessageDetail, reveal bool, now time.Time) MessageSummary {
	if canonical == nil {
		return row
	}
	row = refreshSummaryFromCanonical(row, canonical)
	row.Verification = nil
	in := VerificationInput(canonical)
	f := verification.Detect(in)
	if f == nil {
		return row
	}
	if reveal {
		row.Verification = verification.Describe(in, f, false, nil, now)
		return row
	}
	m := verification.NewMasker(f)
	var fields []string
	text := func(name, value string) string {
		masked := m.Text(value)
		if masked != value && !slices.Contains(fields, name) {
			fields = append(fields, name)
		}
		return masked
	}
	row.Subject = text("subject", row.Subject)
	if row.Snippet != "" {
		row.Snippet = verification.ContentPlaceholder
		fields = append(fields, "snippet")
	}
	row.BodyText = text("body_text", row.BodyText)
	row.FromEmail = text("from_email", row.FromEmail)
	row.FromName = text("from_name", row.FromName)
	row.FromPhone = text("from_phone", row.FromPhone)
	row.ConversationTitle = text("conversation_title", row.ConversationTitle)
	row.WebURL = text("web_url", row.WebURL)
	for _, target := range []struct {
		name   string
		values *[]string
	}{
		{"labels", &row.Labels}, {"attachment_names", &row.AttachmentNames},
	} {
		*target.values = slices.Clone(*target.values)
		for i := range *target.values {
			(*target.values)[i] = text(target.name, (*target.values)[i])
		}
	}
	for _, target := range []*[]Address{&row.To, &row.Cc, &row.Bcc} {
		*target = slices.Clone(*target)
		for i := range *target {
			(*target)[i].Email = text("recipients", (*target)[i].Email)
			(*target)[i].Name = text("recipients", (*target)[i].Name)
		}
	}
	if row.MatchSnippet != "" || len(row.BodyContextSnippets) > 0 {
		fields = append(fields, "matches")
	}
	row.MatchSnippet = ""
	row.BodyContextSnippets = nil
	row.BodyContextSnippetsTruncated = false
	row.Verification = verification.Describe(in, f, true, fields, now)
	return row
}

func refreshSummaryFromCanonical(row MessageSummary, canonical *MessageDetail) MessageSummary {
	fromEmail, fromName := "", ""
	if len(canonical.From) > 0 {
		fromEmail, fromName = canonical.From[0].Email, canonical.From[0].Name
	}
	if row.FromPhone != "" && !strings.EqualFold(strings.TrimSpace(row.FromPhone), strings.TrimSpace(fromEmail)) {
		row.FromPhone = ""
	}
	attachmentNames := make([]string, 0, len(canonical.Attachments))
	for _, attachment := range canonical.Attachments {
		if attachment.Filename != "" {
			attachmentNames = append(attachmentNames, attachment.Filename)
		}
	}
	row.Subject, row.Snippet = canonical.Subject, canonical.Snippet
	row.FromEmail, row.FromName = fromEmail, fromName
	row.ConversationTitle, row.WebURL = canonical.ConversationTitle, canonical.WebURL
	row.To, row.Cc, row.Bcc = slices.Clone(canonical.To), slices.Clone(canonical.Cc), slices.Clone(canonical.Bcc)
	row.Labels = slices.Clone(canonical.Labels)
	row.AttachmentNames = attachmentNames
	if row.BodyText != canonical.BodyText {
		row.BodyText = ""
	}
	if row.MatchSnippet != "" && !summaryExcerptMatchesCanonical(row.MatchSnippet, canonical) {
		row.MatchSnippet = ""
		row.BodyContextSnippetsTruncated = true
	}
	for _, excerpt := range row.BodyContextSnippets {
		if !strings.Contains(canonical.BodyText, excerpt) {
			row.BodyContextSnippets = nil
			row.BodyContextSnippetsTruncated = true
			break
		}
	}
	return row
}

func summaryExcerptMatchesCanonical(excerpt string, canonical *MessageDetail) bool {
	if excerpt == "" {
		return true
	}
	for _, content := range []string{canonical.Subject, canonical.Snippet, canonical.BodyText} {
		if containsExcerptInOrder(content, excerpt) {
			return true
		}
	}
	return false
}

func containsExcerptInOrder(content, excerpt string) bool {
	parts := strings.Split(excerpt, "…")
	offset := 0
	matched := false
	for _, part := range parts {
		if part == "" {
			continue
		}
		index := strings.Index(content[offset:], part)
		if index < 0 {
			return false
		}
		offset += index + len(part)
		matched = true
	}
	return matched
}

func PrepareThreadVerification(row ThreadMessage, canonical *MessageDetail, reveal bool, now time.Time) ThreadMessage {
	prepared := PrepareVerification(canonical, reveal, now)
	row.Subject = prepared.Subject
	row.From, row.To, row.Cc = prepared.From, prepared.To, prepared.Cc
	row.Verification = prepared.Verification
	return row
}
