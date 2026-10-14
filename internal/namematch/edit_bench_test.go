package namematch

import "testing"

// Both implementations receive identical normalized token pairs.
func BenchmarkDirectoryVerifier(b *testing.B) {
	pairs := [][2]string{
		{"alice", "alice"}, {"alcie", "alice"}, {"example", "exampel"},
		{"éloïse", "éloïsf"}, {"界界界界a", "界界界a界"},
		{"participant", "partcipant"}, {"participanx", "participant"},
		{"participant", "parxicypant"},
	}
	for _, tc := range []struct {
		name  string
		match func(string, string) bool
	}{
		{"old", oldVerifier}, {"linear", AtMostOne},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for _, pair := range pairs {
					_ = tc.match(pair[0], pair[1])
				}
			}
		})
	}
}
