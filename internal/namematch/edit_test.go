package namematch

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Removing a permitted edit or accepting two edits breaks this test.
func TestAtMostOne(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		want        bool
	}{
		{"", "", true}, {"", "界", true}, {"", "界界", false},
		{"alice", "alice", true}, {"alice", "allice", true}, {"alice", "alce", true},
		{"alice", "alixe", true}, {"alice", "alcie", true}, {"alice", "blice", true},
		{"aaaa", "aaa", true}, {"alice", "axixe", false}, {"界éab", "界aéb", true},
	} {
		assert.Equal(t, tc.want, AtMostOne(tc.left, tc.right), "%q/%q", tc.left, tc.right)
	}
}

// Exhaustive parity catches changed Directory acceptance independently of the new algorithm.
func TestAtMostOneExhaustiveDirectoryParity(t *testing.T) {
	values := []string{}
	var build func(string, int)
	build = func(s string, n int) {
		if n == 0 {
			values = append(values, s)
			return
		}
		for _, r := range "abé界" {
			build(s+string(r), n-1)
		}
	}
	for n := 1; n <= 5; n++ {
		build("", n)
	}
	for _, a := range values {
		for _, b := range values {
			assert.Equal(t, oldVerifier(a, b), AtMostOne(a, b), "%q/%q", a, b)
		}
	}
}

func TestAtMostOneWithoutNumberEdit(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		want        bool
	}{
		{"invoice2", "invocie2", true}, {"invoice2", "invoice3", false},
		{"file٢", "file٣", false}, {"file²", "file³", false}, {"file2", "file", false},
		{"file2", "file20", false}, {"file2a", "filea2", false}, {"abcde", "xbcde", true},
		{"file①", "file②", false}, {"invoice٢", "invoice٢", true},
		{"2file", "file", false}, {"file", "2file", false}, {"abcde", "xbcye", false},
		{"abcde", "abcdef", true}, {"abcdef", "abcde", true},
	} {
		assert.Equal(t, tc.want, AtMostOneWithoutNumberEdit(tc.left, tc.right), "%q/%q", tc.left, tc.right)
	}
}

func oldVerifier(left, right string) bool {
	a, b := []rune(left), []rune(right)
	if len(a)-len(b) > 1 || len(b)-len(a) > 1 {
		return false
	}
	if len(a) == len(b) {
		first, second := -1, -1
		for index := range a {
			if a[index] == b[index] {
				continue
			}
			if first == -1 {
				first = index
			} else if second == -1 {
				second = index
			} else {
				return false
			}
		}
		if second == first+1 && a[first] == b[second] && a[second] == b[first] {
			return true
		}
	}
	previous, current := make([]int, len(b)+1), make([]int, len(b)+1)
	for index := range previous {
		previous[index] = index
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		minimum := current[0]
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
			if current[j] < minimum {
				minimum = current[j]
			}
		}
		if minimum > 1 {
			return false
		}
		previous, current = current, previous
	}
	return previous[len(b)] <= 1
}

// Number protection applies to every edited position and both argument orders.
func TestAtMostOneNumberEditLocations(t *testing.T) {
	assert := assert.New(t)

	const token = "abcde"
	for _, number := range []rune{'2', '٢', '²', '①'} {
		for pos := range len(token) {
			changed := token[:pos] + string(number) + token[pos+1:]
			assert.False(AtMostOneWithoutNumberEdit(token, changed))
			assert.False(AtMostOneWithoutNumberEdit(changed, token))
		}
		for pos := 0; pos <= len(token); pos++ {
			changed := token[:pos] + string(number) + token[pos:]
			assert.False(AtMostOneWithoutNumberEdit(token, changed))
			assert.False(AtMostOneWithoutNumberEdit(changed, token))
		}
	}
}
