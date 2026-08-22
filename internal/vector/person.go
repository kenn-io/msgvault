package vector

// PersonEmbedding is one complete person document publication. Revision is
// the exact digest of the curated text. An empty Vector records a terminal,
// provider-rejected revision without making the person searchable.
type PersonEmbedding struct {
	PersonID int64
	Revision string
	Vector   []float32
}

// PersonHit is one person-owned ANN result. Revision lets callers revalidate
// the hit against the current store projection before returning it.
type PersonHit struct {
	PersonID int64
	Revision string
	Score    float64
	Rank     int
}
