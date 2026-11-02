package verification

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkMaskerManyValues(b *testing.B) {
	for _, count := range []int{1, 16, 64, 256, 1024} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			finding := &Finding{}
			var text strings.Builder
			for i := 0; i < count; i++ {
				value := fmt.Sprint(10000000 + i)
				finding.Values = append(finding.Values, Secret{Kind: "code", Value: value})
				text.WriteString("Verification code: " + value + ". ")
			}
			body := text.String()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				NewMasker(finding).Text(body)
			}
		})
	}
}
