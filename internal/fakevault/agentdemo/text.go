package agentdemo

import (
	"math/rand/v2"
	"strings"
)

// This vocabulary deliberately excludes requests so every seed has the same loops.
var fillerWords = []string{"archive", "garden", "morning", "project", "notes", "blue", "river", "steady", "summer", "report", "sunlight", "paper", "cloud", "quiet", "window", "finished"}

func filler(seed uint64, index int) string {
	r := rand.New(rand.NewPCG(seed, uint64(index))) //nolint:gosec // reproducible synthetic prose
	words := make([]string, 18)
	for i := range words {
		words[i] = fillerWords[r.IntN(len(fillerWords))]
	}
	return strings.Join(words, " ") + "."
}
