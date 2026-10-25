package novelty

import "math"

// microOne is a cosine or distance of 1.0 in the integer micro units used for
// scoring, so selection never compares floats.
const microOne = 1000000

// unitTolerance bounds |‖v‖² − 1| for a vector accepted as unit length. It
// allows float32 rounding of MessageVector output but rejects raw chunks.
const unitTolerance = 1e-4

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

// unitVector reports whether v is a finite unit vector of the given dimension.
func unitVector(v []float32, dim int) bool {
	return finiteVector(v, dim) && math.Abs(dot(v, v)-1) <= unitTolerance
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

func micro(x float64) int64    { return int64(math.Round(x * microOne)) }
func clampMicro(x int64) int64 { return max(0, min(microOne, x)) }
