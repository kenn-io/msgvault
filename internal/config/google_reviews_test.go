package config

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/googlereviews"
	"testing"
)

func TestGoogleReviewsConfig(t *testing.T) {
	target := googlereviews.Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Enabled: false}
	cfg := NewDefaultConfig()
	cfg.GoogleReviews = []googlereviews.Target{target}
	require.NoError(t, cfg.ValidateGoogleReviews())
	cfg.GoogleReviews = append(cfg.GoogleReviews, target)
	require.Error(t, cfg.ValidateGoogleReviews())
	cfg.GoogleReviews = []googlereviews.Target{target}
	cfg.GoogleReviews[0].Enabled = true
	require.Error(t, cfg.ValidateGoogleReviews())
}
