package palette

import (
	"fmt"
	"math"
	"strconv"
	"testing"
)

// The R7 palette check: every pair of the 12 colours must differ by CIE76
// deltaE of 20 or more under normal vision and under simulated protanopia,
// deutanopia and tritanopia (Machado, Oliveira and Fernandes 2009, severity 1.0,
// applied in linear sRGB).
const minDeltaE = 20.0

var cvdMatrices = map[string][3][3]float64{
	"normal": {{1, 0, 0}, {0, 1, 0}, {0, 0, 1}},
	"protan": {{0.152286, 1.052583, -0.204868}, {0.114503, 0.786281, 0.099216}, {-0.003882, -0.048116, 1.051998}},
	"deutan": {{0.367322, 0.860646, -0.227968}, {0.280085, 0.672501, 0.047413}, {-0.011820, 0.042940, 0.968881}},
	"tritan": {{1.255528, -0.076749, -0.178779}, {-0.078411, 0.930809, 0.147602}, {0.004733, 0.691367, 0.303900}},
}

func parseHex(t testing.TB, h string) [3]float64 {
	t.Helper()
	if len(h) != 7 || h[0] != '#' {
		t.Fatalf("bad colour %q", h)
	}
	var c [3]float64
	for i := range c {
		v, err := strconv.ParseUint(h[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatalf("bad colour %q: %v", h, err)
		}
		c[i] = float64(v) / 255
	}
	return c
}

func toLinear(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func clamp01(v float64) float64 { return math.Min(1, math.Max(0, v)) }

func lab(rgbLinear [3]float64) [3]float64 {
	r, g, b := clamp01(rgbLinear[0]), clamp01(rgbLinear[1]), clamp01(rgbLinear[2])
	x := (0.4124564*r + 0.3575761*g + 0.1804375*b) / 0.95047
	y := 0.2126729*r + 0.7151522*g + 0.0721750*b
	z := (0.0193339*r + 0.1191920*g + 0.9503041*b) / 1.08883
	f := func(t float64) float64 {
		if t > 216.0/24389 {
			return math.Cbrt(t)
		}
		return (24389.0/27*t + 16) / 116
	}
	fx, fy, fz := f(x), f(y), f(z)
	return [3]float64{116*fy - 16, 500 * (fx - fy), 200 * (fy - fz)}
}

func simulate(t testing.TB, hex, kind string) [3]float64 {
	c := parseHex(t, hex)
	lin := [3]float64{toLinear(c[0]), toLinear(c[1]), toLinear(c[2])}
	m := cvdMatrices[kind]
	var out [3]float64
	for i := range out {
		out[i] = m[i][0]*lin[0] + m[i][1]*lin[1] + m[i][2]*lin[2]
	}
	return lab(out)
}

func deltaE76(a, b [3]float64) float64 {
	return math.Sqrt(math.Pow(a[0]-b[0], 2) + math.Pow(a[1]-b[1], 2) + math.Pow(a[2]-b[2], 2))
}

func TestPaletteSurvivesColourVisionDeficiency(t *testing.T) {
	for kind := range cvdMatrices {
		worst, wa, wb := math.MaxFloat64, "", ""
		for i := range Len() {
			for j := i + 1; j < Len(); j++ {
				d := deltaE76(simulate(t, At(i).Hex, kind), simulate(t, At(j).Hex, kind))
				if d < worst {
					worst, wa, wb = d, At(i).Name, At(j).Name
				}
				if d < minDeltaE {
					t.Errorf("%s: %s and %s differ by deltaE %.1f, want %.0f or more", kind, At(i).Name, At(j).Name, d, minDeltaE)
				}
			}
		}
		t.Log(fmt.Sprintf("%s: closest pair %s/%s at deltaE %.1f", kind, wa, wb, worst))
	}
}
