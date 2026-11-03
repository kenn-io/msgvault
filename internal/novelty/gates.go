package novelty

type Gates struct{ MinIdentityScore, MinExcerptRunes int }

func DefaultGates() Gates { return Gates{MinIdentityScore: 800, MinExcerptRunes: 40} }

func (g Gates) Eligible(identityScore, excerptRunes int, cooled bool) bool {
	return !cooled && identityScore >= g.MinIdentityScore && excerptRunes >= g.MinExcerptRunes
}
