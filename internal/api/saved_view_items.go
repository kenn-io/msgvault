package api

import (
	"context"
	"fmt"
	"net/http"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func (s *Server) savedViewItemsResponse(r *http.Request, view *store.SavedView) (SavedView, error) {
	response := savedViewResponse(view, nil)
	response.Items = &store.SavedViewItemList{Include: []store.SavedViewItem{}, Exclude: []store.SavedViewItem{}}
	if len(view.ItemsJSON) == 0 {
		return response, nil
	}
	items, stale, err := s.savedViewStore.SavedViewItemsForView(r.Context(), view)
	if err != nil {
		return response, err
	}
	response.StaleItemCount = stale
	filter := func(entries []store.SavedViewItem) []store.SavedViewItem {
		out := make([]store.SavedViewItem, 0, len(entries))
		for _, e := range entries {
			if !s.agentObjectSourceAllowed(r, e.SourceID) {
				response.RedactedItemCount++
				continue
			}
			out = append(out, e)
		}
		return out
	}
	items.Include = filter(items.Include)
	items.Exclude = filter(items.Exclude)
	response.Items = &items
	response.IncludeCount = len(items.Include)
	response.ExcludeCount = len(items.Exclude)
	return response, nil
}

func (s *Server) resolveExploreItems(ctx context.Context, include, exclude []string) (query.EntryItems, error) {
	items := query.EntryItems{}
	for mode, keys := range [][]string{include, exclude} {
		if len(keys) > store.MaxSavedViewItemsPerMode {
			return items, newAPIHTTPError(400, "saved_view_items_limit", "at most 500 keys per mode")
		}
		for i, key := range keys {
			if _, err := query.ParseEntryKey(key); err != nil {
				return items, newAPIHTTPError(400, "invalid_entry_key", fmt.Sprintf("%s_keys[%d]: %s", []string{"include", "exclude"}[mode], i, err))
			}
		}
		if len(keys) == 0 {
			continue
		}
		if s.savedViewStore == nil {
			return items, newAPIHTTPError(503, "saved_views_unavailable", "entry key resolution unavailable")
		}
		resolved, _, err := s.savedViewStore.ResolveEntryKeys(ctx, keys)
		if err != nil {
			return items, err
		}
		r := (&http.Request{}).WithContext(ctx)
		for _, key := range keys {
			entry, ok := resolved[key]
			if !ok || !s.agentObjectSourceAllowed(r, entry.Ref.SourceID) {
				continue
			}
			ref := entry.Ref
			if mode == 0 {
				if ref.MessageID > 0 {
					items.IncludeMessageIDs = append(items.IncludeMessageIDs, ref.MessageID)
				} else {
					items.IncludeConversationIDs = append(items.IncludeConversationIDs, ref.ConversationID)
				}
			} else {
				if ref.MessageID > 0 {
					items.ExcludeMessageIDs = append(items.ExcludeMessageIDs, ref.MessageID)
				} else {
					items.ExcludeConversationIDs = append(items.ExcludeConversationIDs, ref.ConversationID)
				}
			}
		}
	}
	return items, nil
}

func (s *Server) patchSavedViewItemChange(w http.ResponseWriter, r *http.Request, request PatchSavedViewRequest) (*store.SavedViewItemChange, bool) {
	groups := [][]string{request.IncludeAdd, request.IncludeRemove, request.ExcludeAdd, request.ExcludeRemove}
	present := false
	keys := []string{}
	for _, group := range groups {
		if group != nil {
			present = true
		}
		if len(group) > 500 {
			writeError(w, 400, "saved_view_items_limit", "at most 500 keys per operation")
			return nil, false
		}
		keys = append(keys, group...)
	}
	if !present {
		return nil, true
	}
	resolved, rejected, err := s.savedViewStore.ResolveEntryKeys(r.Context(), keys)
	if err != nil {
		s.writeSavedViewError(w, err)
		return nil, false
	}
	for _, key := range keys {
		if entry, ok := resolved[key]; ok && !s.agentObjectSourceAllowed(r, entry.Ref.SourceID) {
			rejected = append(rejected, store.EntryKeyRejection{Key: key, Reason: "not_found"})
		}
	}
	if len(rejected) > 0 {
		message := "saved_view_items_rejected: no changes applied"
		for _, rejection := range rejected {
			message += fmt.Sprintf("; %s: %s", rejection.Key, rejection.Reason)
		}
		writeJSON(w, 422, struct {
			Error    string                    `json:"error"`
			Message  string                    `json:"message"`
			Rejected []store.EntryKeyRejection `json:"rejected"`
		}{"saved_view_items_rejected", message, rejected})
		return nil, false
	}
	change := &store.SavedViewItemChange{AddedBy: "owner"}
	if grant := s.requestAuthentication(r).Grant; grant != nil {
		change.AddedBy = "agent:" + grant.Label
	}
	targets := []*[]store.SavedViewItemRef{&change.IncludeAdd, &change.IncludeRemove, &change.ExcludeAdd, &change.ExcludeRemove}
	for i, group := range groups {
		for _, key := range group {
			*targets[i] = append(*targets[i], resolved[key].Ref)
		}
	}
	return change, true
}
