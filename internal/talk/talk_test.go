package talk

import (
	"math"
	"testing"
)

func sine(n, rate int, hz float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(10000 * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
	}
	return out
}

func TestResampleDoublesEightKToSixteenK(t *testing.T) {
	var r resampler
	out := r.convert(nil, sine(8000, 8000, 440), 8000, 16000)
	if len(out) < 15998 || len(out) > 16002 {
		t.Fatalf("one second at 8 kHz became %d samples at 16 kHz", len(out))
	}
}

// Packet boundaries must not show in the output: converting in RTP sized
// pieces has to give what converting the whole thing at once gives.
func TestResampleIsContinuousAcrossCalls(t *testing.T) {
	in := sine(4000, 8000, 300)
	var whole resampler
	want := whole.convert(nil, in, 8000, 16000)

	var pieces resampler
	var got []int16
	for off := 0; off < len(in); off += 160 {
		got = pieces.convert(got, in[off:off+160], 8000, 16000)
	}
	if len(got) != len(want) {
		t.Fatalf("pieces gave %d samples, whole gave %d", len(got), len(want))
	}
	for i := range want {
		if d := int(got[i]) - int(want[i]); d > 1 || d < -1 {
			t.Fatalf("sample %d: pieces %d, whole %d", i, got[i], want[i])
		}
	}
}

func TestResamplePassesMatchingRatesThrough(t *testing.T) {
	var r resampler
	in := sine(100, 16000, 440)
	if out := r.convert(nil, in, 16000, 16000); len(out) != len(in) || out[50] != in[50] {
		t.Fatal("16 kHz into 16 kHz was altered")
	}
}

func peak(pcm []int16) int {
	m := 0
	for _, x := range pcm {
		if a := int(math.Abs(float64(x))); a > m {
			m = a
		}
	}
	return m
}

// talk_gain_db scales the speech, and the limiter holds every peak at
// -1 dBFS (29204) however loud it comes in or is turned up: full scale
// overdrove an E1 Zoom's speaker (a tester, 10-29).
func TestGainScalesAndTheLimiterHoldsPeaksAtMinusOneDBFS(t *testing.T) {
	quiet := sine(16000, 16000, 440) // peak 10000
	var l limiter
	if p := peak(l.apply(quiet, math.Pow(10, -12.0/20), 16000)); p < 2480 || p > 2540 {
		t.Errorf("-12 dB: peak %d, want about 2512", p)
	}
	l = limiter{}
	if p := peak(l.apply(quiet, 1, 16000)); p < 9990 || p > 10000 {
		t.Errorf("0 dB below the ceiling: peak %d, want 10000 untouched", p)
	}
	for _, db := range []float64{0, 6, 12} {
		loud := make([]int16, 16000)
		for i := range loud {
			loud[i] = int16(32767 * math.Sin(2*math.Pi*300*float64(i)/16000))
		}
		l = limiter{}
		if p := peak(l.apply(loud, math.Pow(10, db/20), 16000)); p > 29205 {
			t.Errorf("full scale at +%v dB: peak %d, over -1 dBFS", db, p)
		}
	}
}

func TestSetGainTakesDecibels(t *testing.T) {
	var s Session
	s.SetGain(-12)
	if math.Abs(s.gain-0.2512) > 0.001 {
		t.Errorf("-12 dB is %v, want 0.2512", s.gain)
	}
}
