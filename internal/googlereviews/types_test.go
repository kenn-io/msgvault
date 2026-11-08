package googlereviews

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestReplyByteLimit(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.SampledFrom([]string{"a", "é", "中", "🙂", "aé中🙂", " ", "\n"}))
		n := hegel.Draw(ht, hegel.Integers(0, 4097))
		body := strings.Repeat(text, n)
		if hegel.Draw(ht, hegel.Booleans()) {
			body = string(hegel.Draw(ht, hegel.Binary(0, 64)))
		}
		wantInvalid := !utf8.ValidString(body) || strings.TrimSpace(body) == "" || len(body) > 4096
		for _, action := range []string{"preview_reply", "assist_reply"} {
			err := (Request{Action: action, Target: "shop", ReviewID: "r", Comment: body}).Validate()
			assert.Equal(ht, wantInvalid, err != nil)
		}
	})
	for _, n := range []int{4095, 4096, 4097} {
		err := (Request{Action: "preview_reply", Target: "shop", ReviewID: "r", Comment: strings.Repeat("a", n)}).Validate()
		assert.Equal(t, n > 4096, err != nil)
	}
}
func TestActionFieldIsolation(t *testing.T) {
	baselines := []Request{
		{Action: "capabilities"}, {Action: "invalidate_target", Target: "shop"},
		{Action: "archive_status", Target: "shop"}, {Action: "backfill", Target: "shop"},
		{Action: "list", Target: "shop", PageSize: 50},
		{Action: "get", Target: "shop", ReviewID: "r"}, {Action: "assist_reply", Target: "shop", ReviewID: "r", Comment: "Thanks"},
		{Action: "preview_reply", Target: "shop", ReviewID: "r", Comment: "Thanks"},
		{Action: "confirm_reply", ConfirmationToken: strings.Repeat("a", 64), Confirmed: true}, {Action: "receipt", ReceiptID: "receipt"},
	}
	hegel.Test(t, func(ht *hegel.T) {
		request := hegel.Draw(ht, hegel.SampledFrom(baselines))
		require.NoError(ht, request.Validate())
		switch request.Action {
		case "capabilities", "invalidate_target", "archive_status", "backfill", "list", "get", "assist_reply", "preview_reply":
			request.ReceiptID = "unexpected"
		case "confirm_reply", "receipt":
			request.Target = "unexpected"
		}
		require.ErrorIs(ht, request.Validate(), ErrInvalid)
		calls := 0
		service := NewService(Options{Targets: func(context.Context) ([]Target, error) { calls++; return nil, nil }})
		defer service.Close()
		_, err := service.Execute(t.Context(), "owner", request)
		require.ErrorIs(ht, err, ErrInvalid)
		assert.Zero(ht, calls)
	})
	require := require.New(t)
	for _, id := range []string{"../r", "a/b", "x?y", "%2f", ".", ""} {
		require.Error((Request{Action: "get", Target: "shop", ReviewID: id}).Validate())
	}
	require.NoError((Request{Action: "list", Target: "shop", PageSize: 50}).Validate())
	require.Error((Request{Action: "list", Target: "shop", PageSize: 51}).Validate())
	require.Error((Request{Action: "list", Target: "shop", PageToken: strings.Repeat("x", 4097)}).Validate())
	require.Error((Request{Action: "backfill", Target: "shop", PageSize: 50}).Validate())
}

func TestArchivePageValidationBindsReviewNamesToLocation(t *testing.T) {
	require := require.New(t)
	page := Page{Reviews: []Review{{Name: "accounts/a/locations/l/reviews/r1", ReviewID: "r1", CreateTime: "2025-01-01T00:00:00Z"}}}
	require.NoError(page.ValidateFor("shop", "accounts/a/locations/l"))
	require.ErrorIs(page.ValidateFor("shop", "accounts/a/locations/other"), ErrInvalid)
	page.Reviews[0].ReviewID = "../r1"
	require.ErrorIs(page.ValidateFor("shop", "accounts/a/locations/l"), ErrInvalid)
	page = Page{Reviews: []Review{{ReviewID: "r1", ReviewReply: &Reply{Comment: strings.Repeat("x", 2<<20)}}}}
	require.ErrorIs(page.ValidateFor("shop", "accounts/a/locations/l"), ErrInvalid)
}

func TestTargetPolicy(t *testing.T) {
	require := require.New(t)
	valid := Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: "accounts/a/locations/l", Enabled: true, OwnProject: true}
	require.NoError(valid.Validate())
	for _, change := range []func(*Target){func(v *Target) { v.OAuthApp = "" }, func(v *Target) { v.OwnProject = false }, func(v *Target) { v.Location = "accounts/a/locations/../" }, func(v *Target) { v.Provider = "facebook" }} {
		v := valid
		change(&v)
		require.Error(v.Validate())
	}
	valid.Enabled = false
	valid.Location = ""
	require.NoError(valid.Validate())
}
