package peoplesweep

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func appreciationQuoteWindow(t *testing.T) (BriefWindow, string) {
	t.Helper()
	peer := briefWindowItem(10, SourceConversationText, 10, "I carried your boxes upstairs.")
	archive := &briefFakeArchive{items: []EvidenceItem{peer}, appreciationPeers: []BriefAppreciationPeer{{
		Ref: peer.Ref, ConversationID: 42, ContributionKey: strings.Repeat("a", 64),
		RawSHA256: strings.Repeat("b", 64), ExactExcerpt: peer.Excerpt, EventTime: peer.EventTime,
	}}, appreciationOwners: []BriefAppreciationOwner{{
		EvidenceID: "owner:test", Ref: EvidenceRef{SourceLane: SourceConversationText, SourceID: peer.Ref.SourceID, MessageID: 99},
		ConversationID: 42, RawSHA256: strings.Repeat("c", 64), ExactExcerpt: "Thanks for carrying my boxes!", EventTime: peer.EventTime.Add(time.Hour),
	}}}
	window, err := BuildBriefWindow(t.Context(), archive, briefWindowRequest(t))
	require.NoError(t, err)
	return window, briefEvidenceID(t, window, 10)
}

func appreciationQuoteCandidate(t *testing.T, anchor, quote, ownerID, thanks string) []byte {
	t.Helper()
	item, err := json.Marshal(map[string]any{"text": "carrying boxes", "anchor_id": anchor, "quote": quote,
		"thanked": map[string]string{"evidence_id": ownerID, "quote": thanks}})
	require.NoError(t, err)
	return briefOutputJSON{ThanksSuggestions: []string{string(item)}}.raw()
}

// Every nonblank substring is a valid exact quote; a changed byte must not be
// accepted merely because it is similar to the original message.
func TestAppreciationExactQuoteProperty(t *testing.T) {
	window, anchor := appreciationQuoteWindow(t)
	const excerpt = "I carried your boxes upstairs."
	hegel.Test(t, func(ht *hegel.T) {
		start := hegel.Draw(ht, hegel.Integers(0, len(excerpt)-1))
		end := hegel.Draw(ht, hegel.Integers(start+1, len(excerpt)))
		quote := excerpt[start:end]
		if strings.TrimSpace(quote) == "" {
			return
		}
		for _, changed := range []bool{false, true} {
			candidate := quote
			if changed {
				candidate += "!"
			}
			item, err := json.Marshal(map[string]any{"text": "carrying boxes", "anchor_id": anchor, "quote": candidate, "thanked": nil})
			require.NoError(ht, err)
			parsed, err := ParseBrief(briefOutputJSON{ThanksSuggestions: []string{string(item)}}.raw(), window, window.Request.Profile)
			require.NoError(ht, err)
			if changed {
				assert.Empty(ht, parsed.Output.ThanksSuggestions)
			} else {
				assert.Len(ht, parsed.Output.ThanksSuggestions, 1)
			}
		}
	}, hegel.WithTestCases(100), hegel.WithDatabase(""))
}

func FuzzAppreciationQuoteAndThanks(f *testing.F) {
	f.Add("carried your boxes", "Thanks for carrying my boxes!", true)
	f.Add("invented quote", "Thanks", false)
	f.Fuzz(func(t *testing.T, quote, thanks string, ownerNamespace bool) {
		if !utf8.ValidString(quote) || !utf8.ValidString(thanks) || len(quote) == 0 || len(thanks) == 0 || len(quote) > 200 || len(thanks) > 200 {
			t.Skip()
		}
		window, anchor := appreciationQuoteWindow(t)
		ownerID := "owner:test"
		if !ownerNamespace {
			ownerID = anchor
		}
		parsed, err := ParseBrief(appreciationQuoteCandidate(t, anchor, quote, ownerID, thanks), window, window.Request.Profile)
		require.NoError(t, err, fmt.Sprintf("quote %q, thanks %q", quote, thanks))
		peerOK := strings.TrimSpace(quote) != "" && strings.Contains("I carried your boxes upstairs.", quote)
		if !peerOK {
			assert.Empty(t, parsed.Output.ThanksSuggestions)
			return
		}
		require.Len(t, parsed.Output.ThanksSuggestions, 1)
		ownerOK := ownerNamespace && strings.TrimSpace(thanks) != "" && strings.Contains("Thanks for carrying my boxes!", thanks)
		if ownerOK {
			assert.Equal(t, "already_thanked", parsed.Output.ThanksSuggestions[0].State)
		} else {
			assert.Equal(t, "suggested", parsed.Output.ThanksSuggestions[0].State)
			assert.Nil(t, parsed.Output.ThanksSuggestions[0].Thanked)
		}
	})
}

// Retained updates release capacity before fresh items, while dismissal and
// source identity survive a provider's choice of a different exact quote.
func TestAppreciationReplayProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		dismissed := hegel.Draw(ht, hegel.Booleans())
		alternate := hegel.Draw(ht, hegel.Booleans())
		request := briefWindowRequest(t)
		request.MaxItems = 6
		archive := &briefFakeArchive{dismissedAppreciations: map[string]bool{}}
		for i := 0; i < 4; i++ {
			item := briefWindowItem(int64(60+i), SourceConversationText, 10+i, "I reviewed and sent the draft today.")
			key := fmt.Sprintf("%064x", i+1)
			archive.items = append(archive.items, item)
			archive.appreciationPeers = append(archive.appreciationPeers, BriefAppreciationPeer{Ref: item.Ref,
				ConversationID: 42, ContributionKey: key, RawSHA256: strings.Repeat("b", 64), ExactExcerpt: item.Excerpt, EventTime: item.EventTime})
			if i < 3 {
				if request.Previous == nil {
					request.Previous = &BriefBoundary{}
				}
				request.Previous.AppreciationMemory = append(request.Previous.AppreciationMemory, BriefAppreciationMemory{
					Suggestion: BriefThanksSuggestion{Text: "sending the draft", Quote: "sent the draft", Key: key, State: "suggested", PriorThanks: "unknown"},
					PeerRef:    item.Ref, PeerRawSHA256: strings.Repeat("b", 64), ConversationID: 42})
			}
		}
		if dismissed {
			archive.dismissedAppreciations[fmt.Sprintf("%064x", 2)] = true
		}
		peer := archive.items[0]
		archive.appreciationOwners = []BriefAppreciationOwner{{EvidenceID: "owner:test", Ref: EvidenceRef{
			SourceLane: SourceConversationText, SourceID: peer.Ref.SourceID, MessageID: 99}, ConversationID: 42,
			RawSHA256: strings.Repeat("c", 64), ExactExcerpt: "Thanks for sending the draft!", EventTime: peer.EventTime.Add(time.Hour)}}
		window, err := BuildBriefWindow(t.Context(), archive, request)
		require.NoError(ht, err)
		retained, fresh := briefEvidenceID(t, window, 60), briefEvidenceID(t, window, 63)
		quote := "sent the draft"
		if alternate {
			quote = "reviewed and sent"
		}
		candidate := briefOutputJSON{ThanksSuggestions: []string{
			fmt.Sprintf(`{"text":"new help","anchor_id":%q,"quote":"sent the draft","thanked":null}`, fresh),
			fmt.Sprintf(`{"text":"changed","anchor_id":%q,"quote":%q,"thanked":{"evidence_id":"owner:test","quote":"Thanks for sending the draft!"}}`, retained, quote),
		}}
		parsed, err := ParseBrief(candidate.raw(), window, request.Profile)
		require.NoError(ht, err)
		active, resolved := 0, 0
		seen := map[string]BriefThanksSuggestion{}
		for _, suggestion := range parsed.Output.ThanksSuggestions {
			seen[suggestion.Key] = suggestion
			if suggestion.State == "suggested" {
				active++
			} else if suggestion.State == "already_thanked" {
				resolved++
			}
		}
		assert.LessOrEqual(ht, active, 3)
		assert.LessOrEqual(ht, resolved, 3)
		assert.Contains(ht, seen, fmt.Sprintf("%064x", 4), "fresh item uses the freed slot")
		assert.Equal(ht, "already_thanked", seen[fmt.Sprintf("%064x", 1)].State)
		assert.Equal(ht, "sent the draft", seen[fmt.Sprintf("%064x", 1)].Quote)
		if dismissed {
			assert.NotContains(ht, seen, fmt.Sprintf("%064x", 2))
		}
	}, hegel.WithTestCases(40), hegel.WithDatabase(""))
}

func TestAppreciationUnknownThanksWording(t *testing.T) {
	rendered, err := RenderBrief(ParsedBrief{Output: BriefOutput{ThanksSuggestions: []BriefThanksSuggestion{{
		Text: "carrying boxes", State: "suggested",
	}}}}, BriefWindow{})
	require.NoError(t, err)
	assert.Equal(t, "If you haven't already, you could thank them for carrying boxes.", rendered.Text)
}
