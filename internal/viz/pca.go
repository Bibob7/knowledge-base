// Package viz serves a live, read-only visualisation of the knowledge base.
package viz

import "math"

// Project2D reduces the vectors to two dimensions with PCA (power iteration with
// deflation, no external dependencies). Vectors are L2-normalised first so that the
// layout follows cosine similarity. Missing or inconsistent vectors map to (0, 0).
// The result is scaled into [-1, 1] on both axes.
func Project2D(vecs [][]float32) [][2]float64 {
	n := len(vecs)
	out := make([][2]float64, n)
	dim := 0
	for _, v := range vecs {
		if len(v) > dim {
			dim = len(v)
		}
	}
	if n < 2 || dim == 0 {
		return out
	}
	x := make([][]float64, n)
	valid := make([]bool, n)
	mean := make([]float64, dim)
	cnt := 0
	for i, v := range vecs {
		x[i] = make([]float64, dim)
		if len(v) != dim {
			continue
		}
		var norm float64
		for _, f := range v {
			norm += float64(f) * float64(f)
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			continue
		}
		for j, f := range v {
			x[i][j] = float64(f) / norm
			mean[j] += x[i][j]
		}
		valid[i] = true
		cnt++
	}
	if cnt < 2 {
		return out
	}
	for j := range mean {
		mean[j] /= float64(cnt)
	}
	for i := range x {
		if !valid[i] {
			continue
		}
		for j := range mean {
			x[i][j] -= mean[j]
		}
	}

	var comps [2][]float64
	for c := 0; c < 2; c++ {
		comps[c] = principal(x, valid, dim, c)
		// Deflate: remove this component from the data.
		for i := range x {
			if !valid[i] {
				continue
			}
			p := dot(x[i], comps[c])
			for j := range x[i] {
				x[i][j] -= p * comps[c][j]
			}
		}
	}
	// x was deflated in place, so the coordinates are computed from the original vectors.
	return scores(vecs, valid, mean, comps)
}

func scores(vecs [][]float32, valid []bool, mean []float64, comps [2][]float64) [][2]float64 {
	out := make([][2]float64, len(vecs))
	var maxAbs [2]float64
	for i, v := range vecs {
		if !valid[i] {
			continue
		}
		var norm float64
		for _, f := range v {
			norm += float64(f) * float64(f)
		}
		norm = math.Sqrt(norm)
		for c := 0; c < 2; c++ {
			var s float64
			for j, f := range v {
				s += (float64(f)/norm - mean[j]) * comps[c][j]
			}
			out[i][c] = s
			if a := math.Abs(s); a > maxAbs[c] {
				maxAbs[c] = a
			}
		}
	}
	for i := range out {
		for c := 0; c < 2; c++ {
			if maxAbs[c] > 0 {
				out[i][c] /= maxAbs[c]
			}
		}
	}
	return out
}

// principal returns the dominant unit eigenvector of XᵀX (power iteration).
func principal(x [][]float64, valid []bool, dim, seed int) []float64 {
	v := make([]float64, dim)
	for j := range v {
		// Deterministic, non-degenerate start vector.
		v[j] = math.Sin(float64(j*(seed+2)) + 1)
	}
	normalize(v)
	tmp := make([]float64, len(x))
	for it := 0; it < 200; it++ {
		for i := range x {
			if valid[i] {
				tmp[i] = dot(x[i], v)
			} else {
				tmp[i] = 0
			}
		}
		next := make([]float64, dim)
		for i := range x {
			if !valid[i] || tmp[i] == 0 {
				continue
			}
			for j := range next {
				next[j] += tmp[i] * x[i][j]
			}
		}
		if !normalize(next) {
			return v
		}
		var diff float64
		for j := range v {
			diff += math.Abs(next[j] - v[j])
		}
		v = next
		if diff < 1e-9 {
			break
		}
	}
	return v
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func normalize(v []float64) bool {
	n := math.Sqrt(dot(v, v))
	if n == 0 {
		return false
	}
	for i := range v {
		v[i] /= n
	}
	return true
}
