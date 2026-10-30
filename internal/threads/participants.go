package threads

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

func (w *Writer) ownerEvidence(e snapshotEvidence, origin ItemOrigin) (bool, error) {
	owner := origin.Kind == "own_posts" || origin.Kind == "own_replies" || origin.Kind == "sent_provisional" || e.flag("author_owned") || e.flag("is_reply_owned_by_me")
	if owner {
		if value, present := e["is_reply_owned_by_me"]; present && string(value) == "false" {
			return false, errors.New("conflicting Threads ownership evidence")
		}
		var ref *MediaReference
		if value, present := e["owner"]; present {
			if err := json.Unmarshal(value, &ref); err != nil {
				return false, errors.New("invalid Threads owner evidence")
			}
		}
		_, ownerID, _ := tokenSourceIDs(w.sourceIdentifier)
		if ref != nil && ref.ID != ownerID {
			return false, errors.New("Threads owner does not match connected account")
		}
	}
	return owner, nil
}

// authorOccurrence is an opaque versioned tuple: base64url(canonical source)
// followed by the decimal media ID. It is neither a handle nor a provider user
// ID; separate media always get separate occurrences until manually merged.
func authorOccurrence(source, media string) string {
	return "v1:" + base64.RawURLEncoding.EncodeToString([]byte(source)) + ":" + media
}

func (w *Writer) resolveAuthor(ctx context.Context, mediaID string, e snapshotEvidence, owner, usernameSupplied bool) (int64, string, error) {
	username := e.text("username")
	label := username
	if strings.TrimSpace(label) == "" {
		label = "Unknown Threads author"
	}
	kind, key := "threads_reply_author", authorOccurrence(w.sourceIdentifier, mediaID)
	var provider *string
	if owner {
		kind, key = "threads_user_id", w.sourceIdentifier
		appID, ownerID, _ := tokenSourceIDs(w.sourceIdentifier)
		// Canonical decimal components make this tuple collision-free and keep
		// app scoping out of service scope fields (Threads scope policy: none).
		provider = new("threads-owner:v1:" + appID + ":" + ownerID)
		if err := w.store.AddAccountIdentityContext(ctx, w.sourceID, w.sourceIdentifier, "threads_user_id"); err != nil {
			return 0, "", fmt.Errorf("Threads account identity: %w", err)
		}
	} else {
		e.put("author_occurrence", key)
	}
	// Never cache participant IDs: accepted Store merges repoint identifiers.
	pid, err := w.store.EnsureParticipantByIdentifierContext(ctx, kind, key, label)
	if err != nil {
		return 0, "", fmt.Errorf("Threads author occurrence: %w", err)
	}
	now := time.Now().UTC()
	if w.now != nil {
		now = w.now().UTC()
	}
	slug, ref := "threads", w.sourceIdentifier+":media:"+mediaID
	record := func(kind store.ContactAddressKind, value string) (*store.RecordContactObservationResult, error) {
		return w.store.RecordContactObservationContext(ctx, pid, store.ParticipantContactObservationInput{
			SourceID: &w.sourceID, AddressKind: kind, ServiceSlug: &slug, ProviderUserID: provider,
			OriginalValue: value, ObservedAt: &now,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: &ref},
		})
	}
	if provider != nil {
		if _, err := record(store.ContactAddressProviderIdentity, *provider); err != nil {
			return 0, "", fmt.Errorf("Threads owner observation: %w", err)
		}
	}
	if strings.TrimSpace(username) != "" {
		observation, err := record(store.ContactAddressUsername, username)
		if err != nil {
			return 0, "", fmt.Errorf("Threads username observation: %w", err)
		}
		observed := observation.Observation.ObservedAt
		if observed != nil {
			if e.text("first_username") == "" {
				e.put("first_username", username)
				e.put("username_first_observed_at", observed)
			}
		}
	}
	// Store keeps the first same-value observation. The message snapshot owns
	// when this supplied username was observed now, including A -> B -> A.
	// An omitted field carries no new observation and preserves this time.
	if usernameSupplied {
		e.put("username_observed_at", now)
	}
	e.put("current_username", username)
	return pid, label, nil
}

// Native ownership is immutable evidence, distinct from treating an omitted
// author flag as an unknown third party. A null snapshot cannot erase a prior
// explicit false and later permit that occurrence to become the owner.
func preserveOwnerClaim(e snapshotEvidence, m Media, origin ItemOrigin) error {
	var known bool
	has := false
	for _, field := range []string{"author_owner_claim", "is_reply_owned_by_me"} {
		if raw, present := e[field]; present && (string(raw) == "true" || string(raw) == "false") {
			known = string(raw) == "true"
			has = true
			break
		}
	}
	if e.flag("author_owned") {
		if has && !known {
			return errors.New("conflicting Threads ownership evidence")
		}
		known = true
		has = true
	}
	claim := m.IsReplyOwnedByMe
	if origin.Kind == "own_posts" || origin.Kind == "own_replies" || origin.Kind == "sent_provisional" {
		if claim != nil && !*claim {
			return errors.New("conflicting Threads ownership evidence")
		}
		claim = new(true)
	}
	if claim != nil {
		if has && known != *claim {
			return errors.New("conflicting Threads ownership evidence")
		}
		known = *claim
		has = true
	}
	if has {
		e.put("author_owner_claim", known)
	}
	return nil
}
