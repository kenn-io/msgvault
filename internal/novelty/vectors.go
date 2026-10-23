package novelty

import "math"

const MaxMessageChunks = 4

// MessageVector normalizes each of the first maxChunks chunks, then normalizes
// their mean. Zero chunks are skipped; malformed chunks reject the message.
func MessageVector(chunks [][]float32, dim, maxChunks int) ([]float32, bool) {
	if dim < 1 || maxChunks < 1 {
		return nil, false
	}
	sum := make([]float64, dim)
	for _, chunk := range chunks[:min(len(chunks), maxChunks)] {
		if !finiteVector(chunk, dim) {
			return nil, false
		}
		norm := norm32(chunk)
		if norm == 0 {
			continue
		}
		for j, x := range chunk {
			sum[j] += float64(float64(x) / norm)
		}
	}
	norm := norm64(sum)
	if norm == 0 {
		return nil, false
	}
	out := make([]float32, dim)
	for j, x := range sum {
		out[j] = float32(x / norm)
	}
	return out, true
}

func finiteVector(v []float32, dim int) bool {
	if dim < 1 || len(v) != dim {
		return false
	}
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
	}
	return true
}

func dot(a, b []float32) float64 {
	var s float64
	for i, x := range a {
		s += float64(float64(x) * float64(b[i]))
	}
	return s
}
func norm32(v []float32) float64 { return math.Sqrt(dot(v, v)) }
func norm64(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += float64(float64(x) * float64(x))
	}
	return math.Sqrt(s)
}
func micro(x float64) int64    { return int64(math.Round(x * 1e6)) }
func clampMicro(x int64) int64 { return max(0, min(1000000, x)) }
