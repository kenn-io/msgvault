package store_test

import (
	"testing"

	"github.com/wesm/msgvault/internal/store"
)

func TestLiveMessagesWhere_NoAlias(t *testing.T) {
	got := store.LiveMessagesWhere("", true)
	want := "deleted_at IS NULL AND deleted_from_source_at IS NULL"
	if got != want {
		t.Errorf("LiveMessagesWhere(%q) = %q, want %q", "", got, want)
	}
}

func TestLiveMessagesWhere_WithAlias(t *testing.T) {
	got := store.LiveMessagesWhere("m", true)
	want := "m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL"
	if got != want {
		t.Errorf("LiveMessagesWhere(%q) = %q, want %q", "m", got, want)
	}
}

func TestLiveMessagesWhere_TableDriven(t *testing.T) {
	cases := []struct {
		alias                 string
		hideDeletedFromSource bool
		want                  string
	}{
		{"", true, "deleted_at IS NULL AND deleted_from_source_at IS NULL"},
		{"", false, "deleted_at IS NULL"},
		{"m", true, "m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL"},
		{"m", false, "m.deleted_at IS NULL"},
		{"msg", true, "msg.deleted_at IS NULL AND msg.deleted_from_source_at IS NULL"},
		{"msg", false, "msg.deleted_at IS NULL"},
	}
	for _, tc := range cases {
		got := store.LiveMessagesWhere(tc.alias, tc.hideDeletedFromSource)
		if got != tc.want {
			t.Errorf("LiveMessagesWhere(%q, %v) = %q, want %q",
				tc.alias, tc.hideDeletedFromSource, got, tc.want)
		}
	}
}
