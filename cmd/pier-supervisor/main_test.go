package main

import "testing"

func TestParsePSIAvg60(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"typical", "some avg10=0.00 avg60=12.34 avg300=5.00 total=123456\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", 12.34},
		{"quiet", "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", 0},
		{"empty", "", 0},
		{"garbage", "not a psi file\n", 0},
	}
	for _, c := range cases {
		if got := parsePSIAvg60(c.in); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
