package viz

import (
	"math"
	"testing"
)

func TestProject2DSeparatesClusters(t *testing.T) {
	// Two clusters in 4 dimensions: along axis 0 and along axis 1.
	vecs := [][]float32{
		{1, 0.05, 0, 0}, {1, -0.05, 0.02, 0}, {1, 0, -0.02, 0.01},
		{0.05, 1, 0, 0}, {-0.05, 1, 0.02, 0}, {0, 1, -0.02, 0.01},
	}
	p := Project2D(vecs)
	// Clusters must be separated on the first axis, with opposite signs.
	a := (p[0][0] + p[1][0] + p[2][0]) / 3
	b := (p[3][0] + p[4][0] + p[5][0]) / 3
	if a*b >= 0 || math.Abs(a-b) < 0.5 {
		t.Fatalf("clusters not separated on axis 1: %v vs %v (%v)", a, b, p)
	}
	for _, c := range p {
		if math.Abs(c[0]) > 1.0001 || math.Abs(c[1]) > 1.0001 {
			t.Fatalf("coordinates not within [-1, 1]: %v", c)
		}
	}
}

func TestProject2DDegenerate(t *testing.T) {
	if got := Project2D(nil); len(got) != 0 {
		t.Fatal("empty input")
	}
	got := Project2D([][]float32{{1, 2}, nil, {1}})
	if len(got) != 3 {
		t.Fatal("length must match input")
	}
	for _, c := range got {
		if math.IsNaN(c[0]) || math.IsNaN(c[1]) {
			t.Fatalf("NaN in %v", got)
		}
	}
}
