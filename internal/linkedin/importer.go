package linkedin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
)

// SourceType separates local member exports from bridged or live sources.
const SourceType = "linkedin_export"
const rawFormat = "linkedin-export-row-json"

// Options identifies an archive and its owner independently of filenames.
type Options struct {
	Identifier    string
	Me            string
	DisplayName   string
	IncludeDrafts bool
}

// Summary reports rows merged during one import, including repeat rows.
type Summary struct {
	SourceID             int64 `json:"source_id"`
	MessagesAdded        int64 `json:"messages_added"`
	MessagesUpdated      int64 `json:"messages_updated"`
	DraftsSkipped        int64 `json:"drafts_skipped"`
	ConnectionsProcessed int64 `json:"connections_processed"`
}

type actor struct{ URL, Name string }
type resolvedActor struct {
	ID    int64
	Named bool
}
type importer struct {
	st        *store.Store
	opts      Options
	sum       *Summary
	serviceID int64
	actors    map[string]resolvedActor
	roots     map[string]int64
	labels    map[string]int64
}

// ProfileURL canonicalizes the profile URL used as an identity anchor.
func ProfileURL(value string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || (u.Host != "www.linkedin.com" && u.Host != "linkedin.com") || !strings.HasPrefix(u.Path, "/in/") || strings.Trim(strings.TrimPrefix(u.Path, "/in/"), "/") == "" {
		return "", errors.New("expected a LinkedIn member profile URL (https://www.linkedin.com/in/…)")
	}
	u.Scheme = "https"
	u.Host = "www.linkedin.com"
	u.RawQuery = ""
	u.Fragment = ""
	u.RawPath = ""
	u.Path = strings.TrimRight(strings.ToLower(u.Path), "/")
	if strings.Contains(strings.TrimPrefix(u.Path, "/in/"), "/") {
		return "", errors.New("expected a LinkedIn member profile URL")
	}
	return u.String(), nil
}

// Import merges recognized CSV files. Completed rows survive interruption;
// repeating the import repairs them and imports the remaining rows.
func Import(ctx context.Context, st *store.Store, root string, opts Options) (summary *Summary, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts.Identifier = strings.TrimSpace(opts.Identifier)
	if opts.Identifier == "" {
		return nil, errors.New("archive identifier is required")
	}
	me, err := ProfileURL(opts.Me)
	if err != nil {
		return nil, fmt.Errorf("--me: %w", err)
	}
	opts.Me = me
	files, closeArchive, err := discover(root)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, closeArchive()) }()
	source, err := st.GetOrCreateSourceContext(ctx, SourceType, opts.Identifier)
	if err != nil {
		return nil, err
	}
	syncID, err := st.StartSyncContext(ctx, source.ID, "import-linkedin")
	if err != nil {
		return nil, err
	}
	st = st.ScopedToSync(source.ID, syncID)
	defer func() {
		if summary != nil {
			retErr = errors.Join(retErr, st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
				MessagesProcessed: summary.MessagesAdded + summary.MessagesUpdated,
				MessagesAdded:     summary.MessagesAdded, MessagesUpdated: summary.MessagesUpdated,
			}))
		}
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSync(syncID, "LinkedIn export import failed; repeat the import to continue"))
		} else {
			retErr = st.CompleteSyncAndUpdateSourceCursor(syncID, source.ID, source.SyncCursor.String)
		}
	}()
	// Bind explicit owner attribution before any row is written. A nickname
	// must never silently turn an existing source into another member's export.
	var binding struct {
		Version int    `json:"version"`
		Owner   string `json:"owner_profile_url"`
	}
	if source.SyncCursor.Valid && source.SyncCursor.String != "" {
		if json.Unmarshal([]byte(source.SyncCursor.String), &binding) != nil || binding.Version != 1 || binding.Owner != opts.Me {
			return nil, errors.New("archive identifier belongs to a different owner or has incompatible state; use its original --me URL or a new identifier")
		}
	} else {
		binding.Version, binding.Owner = 1, opts.Me
		state, err := json.Marshal(binding)
		if err != nil {
			return nil, err
		}
		if err := st.UpdateSourceSyncState(ctx, source.ID, string(state)); err != nil {
			return nil, err
		}
		source.SyncCursor = sql.NullString{String: string(state), Valid: true}
	}
	service, err := st.ResolveCommunicationServiceContext(ctx, "linkedin")
	if err != nil {
		return nil, err
	}
	imp := &importer{st: st, opts: opts, sum: &Summary{SourceID: source.ID}, serviceID: service.ID, actors: make(map[string]resolvedActor), roots: make(map[string]int64), labels: make(map[string]int64)}
	if _, err := imp.participant(ctx, actor{URL: opts.Me, Name: opts.DisplayName}); err != nil {
		return imp.sum, err
	}
	if err := st.AddAccountIdentityContext(ctx, source.ID, opts.Me, "explicit-me"); err != nil {
		return imp.sum, fmt.Errorf("confirm owner identity: %w", err)
	}
	for _, file := range files {
		err := readRows(ctx, file, func(r row, _ int) error {
			if file.kind == "connections.csv" {
				return imp.connection(ctx, r)
			}
			return imp.message(ctx, file.kind, r)
		})
		if err != nil {
			return imp.sum, fmt.Errorf("%s: %w", file.kind, err)
		}
	}
	return imp.sum, nil
}

func digest(parts ...string) string {
	data, _ := json.Marshal(parts)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (imp *importer) participant(ctx context.Context, a actor) (int64, error) {
	key := a.URL
	kind := "linkedin"
	if key != "" {
		var err error
		key, err = ProfileURL(key)
		if err != nil {
			return 0, err
		}
	} else {
		if strings.TrimSpace(a.Name) == "" {
			return 0, nil
		}
		kind = "linkedin_export_name"
		key = fmt.Sprintf("%d:%s", imp.sum.SourceID, digest(a.Name))
	}
	// Revisit when a later row supplies a name for a formerly nameless URL.
	if previous := imp.actors[key]; previous.ID != 0 && (previous.Named || a.Name == "") {
		return previous.ID, nil
	}
	id, err := imp.st.EnsureParticipantByIdentifierContext(ctx, kind, key, a.Name)
	if err != nil {
		return 0, err
	}
	if err := imp.st.ClassifyParticipantIdentifierServiceContext(ctx, kind, key, &imp.serviceID, nil, nil); err != nil {
		return 0, err
	}
	if a.URL != "" {
		service := "linkedin"
		_, err = imp.st.RecordContactObservationContext(ctx, id, store.ParticipantContactObservationInput{
			SourceID: &imp.sum.SourceID, AddressKind: store.ContactAddressURL, ServiceSlug: &service, OriginalValue: key,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: &key},
		})
		if err != nil {
			return 0, err
		}
	}
	// A URL anchors one person. Re-promotion preserves existing edited profiles
	// and any explicit participant links; display names alone never link people.
	if a.Name != "" {
		if _, _, err = imp.st.CreatePersonFromParticipantContext(ctx, id); err != nil {
			return 0, err
		}
	}
	imp.actors[key] = resolvedActor{ID: id, Named: a.Name != ""}
	return id, nil
}

func (imp *importer) connection(ctx context.Context, r row) error {
	if strings.TrimSpace(r["url"]) == "" {
		return nil
	}
	a := actor{URL: r["url"], Name: strings.TrimSpace(r["first name"] + " " + r["last name"])}
	id, err := imp.participant(ctx, a)
	if err != nil {
		return err
	}
	if email := strings.TrimSpace(r["email address"]); email != "" {
		_, err = imp.st.RecordContactObservationContext(ctx, id, store.ParticipantContactObservationInput{
			SourceID: &imp.sum.SourceID, AddressKind: store.ContactAddressEmail, OriginalValue: email,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: &a.URL},
		})
	}
	imp.sum.ConnectionsProcessed++
	return err
}

// splitRecipientList keeps empty positions so a missing profile URL cannot
// shift the name-to-URL association of the remaining recipients.
func splitRecipientList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	separator := ","
	if strings.Contains(value, ";") {
		separator = ";"
	} else if strings.Contains(value, "\n") {
		separator = "\n"
	}
	parts := strings.Split(value, separator)
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func recipients(r row) ([]actor, error) {
	urls := splitRecipientList(r["recipient profile urls"])
	names := splitRecipientList(r["to"])
	if len(urls) > 0 && len(names) > 0 && len(urls) != len(names) {
		return nil, errors.New("recipient names and profile URL positions differ; cannot align recipients")
	}
	var out []actor
	for i := range max(len(urls), len(names)) {
		var a actor
		if i < len(urls) {
			a.URL = urls[i]
		}
		if i < len(names) {
			a.Name = names[i]
		}
		if a.URL != "" || a.Name != "" {
			out = append(out, a)
		}
	}
	return out, nil
}

func actorKey(a actor) (string, error) {
	if a.URL == "" {
		return "name:" + strings.TrimSpace(a.Name), nil
	}
	profile, err := ProfileURL(a.URL)
	return "profile:" + profile, err
}

func recipientKey(people []actor) (string, error) {
	keys := make([]string, 0, len(people))
	for _, person := range people {
		key, err := actorKey(person)
		if err != nil {
			return "", err
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return digest(slices.Compact(keys)...), nil
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "1":
		return true
	}
	return false
}

func linkURL(value string) string {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.String()
}

func postKey(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}

func (imp *importer) message(ctx context.Context, kind string, r row) error {
	if kind == "messages.csv" && isTrue(r["is message draft"]) && !imp.opts.IncludeDrafts {
		imp.sum.DraftsSkipped++
		return nil
	}
	sent, err := date(r["date"])
	if err != nil {
		return err
	}
	sender := actor{URL: imp.opts.Me, Name: imp.opts.DisplayName}
	var to []actor
	body, subject, title, thread, sourceID, post := "", "", "", "", "", ""
	convType := "public_comments"
	switch kind {
	case "messages.csv":
		sender = actor{URL: strings.TrimSpace(r["sender profile url"]), Name: r["from"]}
		to, err = recipients(r)
		if err != nil {
			return err
		}
		body, subject, title = r["content"], r["subject"], r["conversation title"]
		convType = "direct_chat"
		if len(to) > 1 {
			convType = "group_chat"
		}
		senderKey, err := actorKey(sender)
		if err != nil {
			return err
		}
		attachments := splitURLs(r["attachments"])
		slices.Sort(attachments)
		contentKey := body
		if len(attachments) > 0 {
			contentKey = digest(body, digest(slices.Compact(attachments)...))
		} else if body == "" {
			contentKey = digest(body, subject)
		}
		conversation := strings.TrimSpace(r["conversation id"])
		parts := []string{conversation, senderKey, sent.Format("2006-01-02T15:04:05.999999999Z"), contentKey}
		if conversation == "" {
			key, err := recipientKey(to)
			if err != nil {
				return err
			}
			parts = append(parts, key)
		}
		sourceID = "dm:" + digest(parts...)
		thread = "dm:" + conversation
		if conversation == "" {
			thread = sourceID
		}

	case "shares.csv":
		body = r["sharecommentary"]
		post = linkURL(r["sharelink"])
		sourceID = "post:" + postKey(post)
		if post == "" {
			sourceID = "post:" + digest(sent.String(), body, strings.TrimSpace(r["sharedurl"]), strings.TrimSpace(r["mediaurl"]))
		}
		thread = sourceID
		title = snippet(body)
	case "comments.csv":
		body = r["message"]
		post = linkURL(r["link"])
		sourceID = "comment:" + digest(postKey(post), sent.String(), body)
		thread = "post:" + postKey(post)
		if post == "" {
			thread = sourceID
		}
	}
	senderID, err := imp.participant(ctx, sender)
	if err != nil {
		return err
	}
	var members []store.ConversationParticipantRef
	var recipientSets []store.RecipientSet
	if senderID != 0 {
		members = append(members, store.ConversationParticipantRef{ParticipantID: senderID, Role: "member"})
		recipientSets = append(recipientSets, store.RecipientSet{Type: "from", ParticipantIDs: []int64{senderID}, DisplayNames: []string{sender.Name}})
	}
	var toIDs []int64
	var toNames []string
	seen := map[int64]bool{senderID: true}
	for _, a := range to {
		id, err := imp.participant(ctx, a)
		if err != nil {
			return err
		}
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		toIDs = append(toIDs, id)
		toNames = append(toNames, a.Name)
		members = append(members, store.ConversationParticipantRef{ParticipantID: id, Role: "member"})
	}
	recipientSets = append(recipientSets, store.RecipientSet{Type: "to", ParticipantIDs: toIDs, DisplayNames: toNames})
	var reply sql.NullInt64
	if kind == "comments.csv" && post != "" {
		rootID := imp.roots[thread]
		if rootID == 0 {
			prior, err := imp.st.StoredMessagesContext(ctx, imp.sum.SourceID, rawFormat, []string{thread})
			if err != nil {
				return err
			}
			rootID = prior[thread].ID
		}
		reply = sql.NullInt64{Int64: rootID, Valid: rootID != 0}
	}
	var labelIDs []int64
	label := r["folder"]
	switch kind {
	case "comments.csv":
		label = "Comments"
	case "shares.csv":
		label = "Shares"
	}
	if label != "" {
		id := imp.labels[label]
		if id == 0 {
			id, err = imp.st.EnsureLabel(ctx, imp.sum.SourceID, label, label, "user")
			if err != nil {
				return err
			}
			imp.labels[label] = id
		}
		labelIDs = append(labelIDs, id)
	}
	prior, err := imp.st.StoredMessagesContext(ctx, imp.sum.SourceID, rawFormat, []string{sourceID})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	metadata, _ := json.Marshal(map[string]string{"kind": kind, "post_url": post, "visibility": r["visibility"]})
	self := false
	if sender.URL != "" {
		canonical, _ := ProfileURL(sender.URL)
		self = canonical == imp.opts.Me
	}
	message := &store.Message{SourceID: imp.sum.SourceID, SourceMessageID: sourceID, MessageType: "linkedin",
		SentAt: sql.NullTime{Time: sent, Valid: true}, ReceivedAt: sql.NullTime{Time: sent, Valid: true},
		SenderID: sql.NullInt64{Int64: senderID, Valid: senderID != 0}, IsFromMe: self,
		IsSent:  sql.NullBool{Bool: !isTrue(r["is message draft"]), Valid: true},
		Subject: sql.NullString{String: subject, Valid: subject != ""}, Snippet: sql.NullString{String: snippet(body), Valid: body != ""},
		SizeEstimate: int64(len(body)), ReplyToMessageID: reply,
	}
	id, err := imp.st.PersistMessageContext(ctx, &store.MessagePersistData{
		Message: message, Conversation: &store.ConversationPersistData{SourceConversationID: thread, ConversationType: convType, Title: title, Participants: members, PreserveExistingParticipants: true, PreserveExistingType: convType != "group_chat"},
		BodyText: sql.NullString{String: body, Valid: true}, Metadata: &sql.NullString{String: string(metadata), Valid: true},
		RawMIME: raw, RawFormat: rawFormat, Recipients: recipientSets, LabelIDs: labelIDs, PreserveLabels: label == "",
		FTS: &store.FTSDoc{Subject: subject, Body: body, FromAddr: sender.Name, ToAddrs: strings.Join(toNames, " ")},
	})
	if err != nil {
		return err
	}
	var links []store.AttachmentRef
	seenLinks := make(map[string]bool)
	for _, value := range append(splitURLs(r["attachments"]), post, r["sharedurl"], r["mediaurl"]) {
		if link := linkURL(value); link != "" && !seenLinks[link] {
			seenLinks[link] = true
			links = append(links, store.AttachmentRef{Filename: link, StoragePath: link, SourceAttachmentID: "linkedin-link:" + digest(link), MediaType: "link"})
		}
	}
	if err := imp.st.ReplaceMessageLinkAttachments(id, links); err != nil {
		return err
	}
	if err := imp.st.RecomputeMessageAttachmentStatsContext(ctx, id); err != nil {
		return err
	}
	if reply.Valid {
		if err := imp.st.SetMessageReplyContext(ctx, id, reply.Int64); err != nil {
			return err
		}
	} else if kind == "comments.csv" && post == "" {
		if err := imp.st.ClearMessageRepliesContext(ctx, imp.sum.SourceID, []int64{id}); err != nil {
			return err
		}
	}
	if kind == "shares.csv" {
		imp.roots[thread] = id
	}
	if _, exists := prior[sourceID]; exists {
		imp.sum.MessagesUpdated++
	} else {
		imp.sum.MessagesAdded++
	}
	return nil
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 200 {
		return string([]rune(s)[:200])
	}
	return s
}

func splitURLs(value string) []string {
	value = strings.ReplaceAll(value, ",https://", " https://")
	value = strings.ReplaceAll(value, ",http://", " http://")
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r' })
	for i := 0; i+1 < len(parts); i++ {
		if strings.HasPrefix(parts[i+1], "http://") || strings.HasPrefix(parts[i+1], "https://") {
			parts[i] = strings.TrimSuffix(parts[i], ",")
		}
	}
	return parts
}
