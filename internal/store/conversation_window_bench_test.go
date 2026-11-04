package store

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkConversationWindow(b *testing.B) {
	for _, size := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			s, err := OpenForTest(filepath.Join(b.TempDir(), "window.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			require.NoError(b, s.InitSchema())
			seedLiveMessages(b, s, size)
			for _, anchor := range []struct {
				name string
				id   int64
				want int
			}{{"first", 1, 26}, {"middle", int64(size / 2), 51}, {"last", int64(size), 26}} {
				b.Run(anchor.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						window, err := s.GetConversationWindowContext(context.Background(), 1, anchor.id, 25, 25, nil, nil)
						require.NoError(b, err)
						require.Len(b, window.Messages, anchor.want)
					}
				})
			}
		})
	}
}
