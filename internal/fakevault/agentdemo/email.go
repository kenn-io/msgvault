package agentdemo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/gmail"
	syncer "go.kenn.io/msgvault/internal/sync"
)

func email(ctx context.Context, b *builder) error {
	api := gmail.NewMockAPI()
	api.UseRawThreadID = true
	api.HistoryID = 100
	api.Profile = &gmail.Profile{EmailAddress: b.m.Owner.Email, HistoryID: 100}
	for _, id := range []string{"INBOX", "SENT", "UNREAD", "IMPORTANT", "STARRED", "DRAFT"} {
		api.Labels = append(api.Labels, &gmail.Label{ID: id, Name: id, Type: "system"})
	}
	api.Labels = append(api.Labels, &gmail.Label{ID: "clients", Name: "clients", Type: "user"})
	var messages []emailMessage
	add := func(h, thread, from, to, body string, days int, labels ...string) {
		messages = append(messages, emailMessage{handle: h, thread: thread, from: from, to: to, subject: emailSubject(thread), body: body, at: b.m.AsOf.Add(-time.Duration(days) * 24 * time.Hour), labels: labels})
	}
	owner := "Morgan Lee <morgan@example.com>"
	add("jordan_contract_question", "jordan_contract", "Jordan Kim <jordan@client.example>", owner, "Can you confirm the signed contract by Friday?", 6, "INBOX", "UNREAD", "clients")
	messages[len(messages)-1].attachment = true
	add("taylor_proposal_question", "taylor_proposal", owner, "Taylor Brooks <taylor@partner.example>", "Can you confirm the proposal timeline?", 9, "SENT")
	add("priya_budget_question", "priya_budget", "Priya Shah <priya@corp.example>", owner, "Can you confirm the budget?", 8, "INBOX", "STARRED")
	add("priya_budget_answer", "priya_budget", owner, "Priya Shah <priya@corp.example>", "The budget is approved for the summer project.", 7, "SENT")
	messages[len(messages)-1].reply = "priya_budget_question"
	for i := 1; i <= 6; i++ {
		h := fmt.Sprintf("sam_trip_own_%d", i)
		if i == 3 {
			h = "sam_trip_sent_from_draft"
		}
		labels := []string{"SENT"}
		if i == 3 {
			labels = []string{"DRAFT"}
		}
		inbound := fmt.Sprintf("sam_trip_in_%d", i)
		add(inbound, "sam_trip_email", "Sam Rivera <sam.rivera@example.org>", owner, "The route and lodging notes are attached to the plan.", 15-i, "INBOX")
		add(h, "sam_trip_email", owner, "Sam Rivera <sam.rivera@example.org>", "Hi Sam,\n\nThe itinerary is ready. I have recorded the lodging details.\n\nBest, Morgan\n\nOn Mon, 1 Jun 2026 at 10:00, Sam Rivera wrote:\n> The route and lodging notes are ready.\n", 15-i, labels...)
		messages[len(messages)-1].at = messages[len(messages)-1].at.Add(time.Hour)
		messages[len(messages)-1].reply = inbound
	}
	add("sam_trip_unsent_draft", "sam_trip_email", owner, "Sam Rivera <sam.rivera@example.org>", "Hi Sam,\nThe packing notes are ready.\nBest, Morgan", 1, "DRAFT")
	add("riley_intro_question", "riley_intro", "Riley Stone <riley@newco.example>", owner, "Can you describe the archive format?", 5, "INBOX")
	add("pat_quote_outbound", "pat_quote", owner, "Pat Vendor <pat@vendor.example>", "The requirements are recorded in the project notes.", 8, "SENT")
	add("pat_quote_reply", "pat_quote", "Pat Vendor <pat@vendor.example>", owner, "The quote is attached to the project notes.\n\nOn Mon, 1 Jun 2026 at 10:00, Morgan wrote:\n> The requirements are recorded.", 7, "INBOX")
	messages[len(messages)-1].reply = "pat_quote_outbound"
	add("vendor_ticket_question", "vendor_ticket", "Vendor Support <support@vendor.example>", owner, "Can you confirm the ticket details?", 5, "INBOX")
	receipts := []string{}
	bulk := []string{}
	for i := 1; i <= 4; i++ {
		h := fmt.Sprintf("noreply_receipt_%d", i)
		receipts = append(receipts, h)
		add(h, "noreply_receipts", "Service Notifications <no-reply@service.example>", owner, "Your receipt is ready.", 10-i, "INBOX")
		messages[len(messages)-1].extra = "Auto-Submitted: auto-generated\r\n"
	}
	for i := 1; i <= 8; i++ {
		h := fmt.Sprintf("newsletter_%d", i)
		bulk = append(bulk, h)
		add(h, "newsletter", "Weekly Digest <digest@news.example>", owner, filler(b.opts.Seed, i), 20-i, "INBOX")
		messages[len(messages)-1].extra = "List-Id: Weekly Digest <digest.news.example>\r\nList-Unsubscribe: <mailto:unsubscribe@news.example>\r\nPrecedence: bulk\r\n"
	}
	add("followup_jordan", "followup_jordan", owner, "Jordan Kim <jordan@client.example>", "Here are the notes from our kickoff. The contract is recorded in the project folder.", 4, "SENT", "clients")
	add("deleted_at_source", "background_deleted", "Archive Updates <archive-updates@example.net>", owner, "The earlier project record is archived.", 20, "INBOX")
	add("canary", "canary", "Canary Q. Person <canary-addr-7f3a@canary.example>", owner, "The synthetic privacy marker is CANARY-BODY-7f3a.", 12, "INBOX")
	messages[len(messages)-1].subject = "CANARY-SUBJ-7f3a"
	for i := range 60 {
		thread := fmt.Sprintf("background_%02d", i+1)
		for j := range 3 {
			h := fmt.Sprintf("%s_%d", thread, j+1)
			from, to := owner, "Archive Updates <archive-updates@example.net>"
			label := "SENT"
			if j == 0 {
				from, to = to, from
				label = "INBOX"
			}
			add(h, thread, from, to, filler(b.opts.Seed, 100+i*3+j), 40+i, label)
			messages[len(messages)-1].at = messages[len(messages)-1].at.Add(time.Duration(j) * time.Hour)
		}
	}
	for _, m := range messages {
		raw, err := rawMIME(m)
		if err != nil {
			return err
		}
		api.Messages[m.handle] = &gmail.RawMessage{ID: m.handle, ThreadID: m.thread, LabelIDs: m.labels, Raw: raw, InternalDate: m.at.UnixMilli(), HistoryID: 100, SizeEstimate: int64(len(raw))}
		// A single-message page fixes allocation order even when fetch workers run concurrently.
		api.MessagePages = append(api.MessagePages, []string{m.handle})
	}
	api.Profile.MessagesTotal = int64(len(messages))
	s := syncer.New(api, b.st, &syncer.Options{SourceType: "gmail", AttachmentsDir: b.attachmentsDir})
	if _, err := s.Full(ctx, b.m.Owner.Email); err != nil {
		return err
	}
	source, err := b.st.GetOrCreateSourceContext(ctx, "gmail", b.m.Owner.Email)
	if err != nil {
		return err
	}
	api.HistoryID = 101
	api.Profile.HistoryID = 101
	api.HistoryRecords = []gmail.HistoryRecord{{
		ID:            101,
		LabelsAdded:   []gmail.HistoryLabelChange{{Message: gmail.MessageID{ID: "sam_trip_sent_from_draft"}, LabelIDs: []string{"SENT"}}},
		LabelsRemoved: []gmail.HistoryLabelChange{{Message: gmail.MessageID{ID: "sam_trip_sent_from_draft"}, LabelIDs: []string{"DRAFT"}}},
	}}
	if _, err = s.Incremental(ctx, source); err != nil {
		return err
	}
	for _, m := range messages {
		var id, conv int64
		if err = b.st.DB().QueryRowContext(ctx, "SELECT id,conversation_id FROM messages WHERE source_id=? AND source_message_id=?", source.ID, m.handle).Scan(&id, &conv); err != nil {
			return fmt.Errorf("resolve %s: %w", m.handle, err)
		}
		b.m.Messages[m.handle] = id
		b.m.Conversations[m.thread] = Conversation{conv, "gmail", "email_thread"}
	}
	var hash string
	if err = b.st.DB().QueryRowContext(ctx, "SELECT content_hash FROM attachments WHERE message_id=?", b.message("jordan_contract_question")).Scan(&hash); err != nil {
		return err
	}
	b.m.Attachments["contract_v2"] = Attachment{hash, []string{"jordan_contract_question"}}
	b.m.Expect["awaiting_owner_reply"] = []string{"jordan_contract_question", "riley_intro_question", "vendor_ticket_question"}
	b.m.Expect["awaiting_their_reply"] = []string{"taylor_proposal_question"}
	b.m.Expect["settled"] = []string{"priya_budget_question"}
	b.m.Expect["starred_messages"] = []string{"priya_budget_question"}
	b.m.Expect["bulk_messages"] = bulk
	b.automatedMessages = receipts
	b.m.Expect["automated_messages"] = receipts
	b.m.Expect["role_account_participants"] = []string{"support_vendor", "noreply"}
	b.m.Expect["unsaved_participants"] = []string{"riley", "pat", "canary"}
	b.m.Expect["first_contact_messages"] = []string{"riley_intro_question"}
	return nil
}

// Subjects are presentation text; stable thread handles remain source identifiers.
func emailSubject(thread string) string {
	switch thread {
	case "jordan_contract":
		return "Signed contract for the summer project"
	case "taylor_proposal":
		return "Proposal timeline"
	case "priya_budget":
		return "Summer project budget"
	case "sam_trip_email":
		return "Trip itinerary and lodging"
	case "riley_intro":
		return "Introduction to the archive project"
	case "pat_quote":
		return "Quote for the project requirements"
	case "vendor_ticket":
		return "Support ticket details"
	case "noreply_receipts":
		return "Your service receipts"
	case "newsletter":
		return "Weekly archive digest"
	case "followup_jordan":
		return "Notes from our kickoff"
	case "background_deleted":
		return "Earlier project record"
	default:
		return "Project archive notes " + strings.TrimPrefix(thread, "background_")
	}
}
