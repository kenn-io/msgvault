package config

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/businessreviews"
	"testing"
)

func TestBusinessReviewsConfig(t *testing.T) {
	target := businessreviews.Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Enabled: false}
	cfg := NewDefaultConfig()
	cfg.BusinessReviews = []businessreviews.Target{target}
	require.NoError(t, cfg.ValidateBusinessReviews())
	cfg.BusinessReviews = append(cfg.BusinessReviews, target)
	require.Error(t, cfg.ValidateBusinessReviews())
	cfg.BusinessReviews = []businessreviews.Target{target}
	cfg.BusinessReviews[0].Enabled = true
	require.Error(t, cfg.ValidateBusinessReviews())
}
