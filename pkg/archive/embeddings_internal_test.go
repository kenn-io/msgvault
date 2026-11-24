package archive

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

func TestEmbeddingGenerationFingerprint(t *testing.T) {
	d := embedmodel.Descriptor{Model: embedconfig.Model{Name: "test", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}}
	cfg, err := embeddingConfig(&Embeddings{Descriptor: d})
	require.NoError(t, err)
	require.Equal(t, "test:2:p1-111111:c32768:e1:ke5a75bccc9fffe37ce0196b03b2bc41be73a22a6c40b769bb8ee023c3ca31385", cfg.GenerationFingerprint())
}
