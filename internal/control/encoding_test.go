package control

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// panoOptions is the pano's own answer, reduced to what this page uses:
// two whole combinations, one per main resolution, with bitrates that
// differ between them.
func panoOptions() []cgi.EncOptions {
	return []cgi.EncOptions{
		{
			Main: cgi.EncStreamOptions{
				Size: "7680*2160", Width: 7680, Height: 2160,
				BitRates:   []int{3072, 4096, 5120, 6144, 7168, 8192, 9216, 10240, 12288},
				FrameRates: []int{20, 18, 16, 15, 12, 10, 8, 6, 4},
				GOPMin:     1, GOPMax: 2,
				Profiles: []string{"Base", "Main", "High"}, VTypes: []string{"h265"},
			},
			Sub: cgi.EncStreamOptions{
				Size: "1536*432", Width: 1536, Height: 432,
				BitRates:   []int{256, 512, 1024, 1536, 2048},
				FrameRates: []int{20, 15, 10, 7, 4},
				GOPMin:     1, GOPMax: 4,
				Profiles: []string{"Base", "Main", "High"}, VTypes: []string{"h264"},
			},
		},
		{
			Main: cgi.EncStreamOptions{
				Size: "4096*1152", Width: 4096, Height: 1152,
				BitRates:   []int{3072, 4096, 5120, 6144, 7168, 8192, 9216, 10240},
				FrameRates: []int{20, 18, 16, 15, 12, 10, 8, 6, 4, 2},
				GOPMin:     1, GOPMax: 2,
				Profiles: []string{"Base", "Main", "High"}, VTypes: []string{"h265"},
			},
			Sub: cgi.EncStreamOptions{
				Size: "1536*432", Width: 1536, Height: 432,
				BitRates:   []int{256, 512, 1024, 1536, 2048},
				FrameRates: []int{20, 15, 10, 7, 4},
				GOPMin:     1, GOPMax: 4,
				Profiles: []string{"Base", "Main", "High"}, VTypes: []string{"h264"},
			},
		},
	}
}

// The whole reason the options are whole combinations: a bitrate legal at
// one resolution is refused at another, so checking a value against a
// merged list would pass something the camera rejects.
func TestEncodingRefusesABitrateFromTheOtherResolution(t *testing.T) {
	p := encodingPage{Options: panoOptions(), Have: true}

	// 12288 is offered at 7680x2160 and nowhere else.
	ok := cgi.EncStream{Size: "7680*2160", BitRate: 12288, FrameRate: 20, GOP: 2, Profile: "High"}
	if err := checkEncChoice(ok, p.MainOptionsFor(ok.Size), "main"); err != nil {
		t.Errorf("12288 at 7680*2160 should be allowed: %v", err)
	}

	bad := cgi.EncStream{Size: "4096*1152", BitRate: 12288, FrameRate: 20, GOP: 2, Profile: "High"}
	err := checkEncChoice(bad, p.MainOptionsFor(bad.Size), "main")
	if err == nil {
		t.Fatal("12288 at 4096*1152 was accepted; the camera does not offer it there")
	}
	// The refusal has to say what IS on offer, or it sends somebody back to
	// guess at a list they cannot see.
	if !strings.Contains(err.Error(), "10240") {
		t.Errorf("the refusal does not name the stops: %v", err)
	}
}

// Frame rate differs the other way round: 2 fps exists only at the SMALLER
// size, so an asymmetric mistake is caught too.
func TestEncodingRefusesAFrameRateFromTheOtherResolution(t *testing.T) {
	p := encodingPage{Options: panoOptions(), Have: true}

	ok := cgi.EncStream{Size: "4096*1152", BitRate: 8192, FrameRate: 2, GOP: 1, Profile: "High"}
	if err := checkEncChoice(ok, p.MainOptionsFor(ok.Size), "main"); err != nil {
		t.Errorf("2 fps at 4096*1152 should be allowed: %v", err)
	}

	bad := cgi.EncStream{Size: "7680*2160", BitRate: 8192, FrameRate: 2, GOP: 1, Profile: "High"}
	if err := checkEncChoice(bad, p.MainOptionsFor(bad.Size), "main"); err == nil {
		t.Error("2 fps at 7680*2160 was accepted; the camera does not offer it there")
	}
}

func TestEncodingRefusesAnInterframeSpaceOutsideTheRange(t *testing.T) {
	p := encodingPage{Options: panoOptions(), Have: true}
	for _, gop := range []int{0, 3, 50} {
		s := cgi.EncStream{Size: "4096*1152", BitRate: 8192, FrameRate: 20, GOP: gop, Profile: "High"}
		if err := checkEncChoice(s, p.MainOptionsFor(s.Size), "main"); err == nil {
			t.Errorf("interframe space %d was accepted; the range is 1 to 2", gop)
		}
	}
	good := cgi.EncStream{Size: "4096*1152", BitRate: 8192, FrameRate: 20, GOP: 2, Profile: "High"}
	if err := checkEncChoice(good, p.MainOptionsFor(good.Size), "main"); err != nil {
		t.Errorf("interframe space 2 refused: %v", err)
	}
}

func TestEncodingRefusesAResolutionTheCameraDoesNotOffer(t *testing.T) {
	p := encodingPage{Options: panoOptions(), Have: true}
	s := cgi.EncStream{Size: "1920*1080", BitRate: 8192, FrameRate: 20, GOP: 1, Profile: "High"}
	// MainOptionsFor falls back to the first combination for an unknown
	// size, so the size mismatch is what must catch this.
	if err := checkEncChoice(s, p.MainOptionsFor(s.Size), "main"); err == nil {
		t.Error("a resolution the camera never offered was accepted")
	}
}

// The form narrows the stops with JavaScript, and it needs the whole table
// to do it. A page that shipped only the stops for the size in force could
// not repopulate them when the size changed.
func TestOptionsJSONCarriesEveryCombination(t *testing.T) {
	p := encodingPage{Options: panoOptions(), Have: true}

	var got []struct {
		Main struct {
			Size       string `json:"size"`
			BitRates   []int  `json:"bitRates"`
			FrameRates []int  `json:"frameRates"`
			GOPMin     int    `json:"gopMin"`
			GOPMax     int    `json:"gopMax"`
		} `json:"main"`
		Sub struct {
			Size     string `json:"size"`
			BitRates []int  `json:"bitRates"`
		} `json:"sub"`
	}
	if err := json.Unmarshal([]byte(p.OptionsJSON()), &got); err != nil {
		t.Fatalf("OptionsJSON is not valid JSON: %v\n%s", err, p.OptionsJSON())
	}
	if len(got) != 2 {
		t.Fatalf("combinations = %d, want 2", len(got))
	}
	if got[0].Main.Size != "7680*2160" || got[1].Main.Size != "4096*1152" {
		t.Fatalf("sizes = %q, %q", got[0].Main.Size, got[1].Main.Size)
	}
	if len(got[0].Main.BitRates) != 9 || len(got[1].Main.BitRates) != 8 {
		t.Errorf("bitrate counts = %d and %d, want 9 and 8",
			len(got[0].Main.BitRates), len(got[1].Main.BitRates))
	}
	if got[1].Main.GOPMax != 2 {
		t.Errorf("gop max = %d, want 2", got[1].Main.GOPMax)
	}
	if got[0].Sub.Size != "1536*432" {
		t.Errorf("sub size = %q", got[0].Sub.Size)
	}
}

// An empty page must offer nothing rather than a form full of zeros --
// the same rule the aligner learned the hard way.
func TestEncodingOffersNothingWithoutOptions(t *testing.T) {
	p := encodingPage{}
	if p.Have {
		t.Error("an unread encoding claims to have values")
	}
	if got := p.OptionsJSON(); got != "[]" {
		t.Errorf("OptionsJSON = %q, want []", got)
	}
	// And checking anything against no options must refuse, not pass.
	s := cgi.EncStream{Size: "4096*1152", BitRate: 8192, FrameRate: 20, GOP: 1}
	if err := checkEncChoice(s, p.MainOptionsFor(s.Size), "main"); err == nil {
		t.Error("a value was accepted against a camera that offered no options")
	}
}

// What a successful write says has to be what the write did. This one is
// not cosmetic: the message is the only confirmation the operator gets that
// the camera took the setting they asked for, so a message about a stream
// they did not touch is worse than no message. Found against the pano --
// changing the sub stream's interframe space reported the main stream's
// settings and never mentioned the sub stream at all.
func TestASavedEncodingNamesTheStreamThatChanged(t *testing.T) {
	cur := cgi.Enc{
		Main: cgi.EncStream{Size: "4096*1152", Width: 4096, Height: 1152, BitRate: 10240, FrameRate: 20, GOP: 1, Profile: "High", VType: "h265"},
		Sub:  cgi.EncStream{Size: "1536*432", Width: 1536, Height: 432, BitRate: 2048, FrameRate: 20, GOP: 1, Profile: "High", VType: "h264"},
	}

	subOnly := cur
	subOnly.Sub.GOP = 2
	got := encodingChanged(cur, subOnly)
	if !strings.Contains(got, "sub is now") || !strings.Contains(got, "interframe space 2") {
		t.Errorf("a sub-only write says %q, which does not say what changed", got)
	}
	if strings.Contains(got, "main is now") {
		t.Errorf("a sub-only write talks about the main stream: %q", got)
	}

	mainOnly := cur
	mainOnly.Main.FrameRate = 15
	got = encodingChanged(cur, mainOnly)
	if !strings.Contains(got, "main is now") || !strings.Contains(got, "15 fps") {
		t.Errorf("a main-only write says %q", got)
	}
	if strings.Contains(got, "sub is now") {
		t.Errorf("a main-only write talks about the sub stream: %q", got)
	}

	both := cur
	both.Main.FrameRate = 15
	both.Sub.BitRate = 1024
	got = encodingChanged(cur, both)
	if !strings.Contains(got, "main is now") || !strings.Contains(got, "sub is now") {
		t.Errorf("a write touching both streams says %q", got)
	}
}

// cam-19 behind the NVR: the camera runs 6144 kbps at 3840*2160 but the NVR
// offers 4096 at most. Turning the microphone on leaves both streams as
// they are, so it must not be refused over that bitrate; changing main's
// frame rate does touch main, so the bitrate is checked then.
func TestTurningTheMicrophoneOnIgnoresUntouchedStreams(t *testing.T) {
	page := encodingPage{Options: []cgi.EncOptions{{
		Main: cgi.EncStreamOptions{Size: "3840*2160", Width: 3840, Height: 2160,
			BitRates: []int{1024, 1536, 2048, 3072, 4096}, FrameRates: []int{25, 20, 15},
			GOPMin: 1, GOPMax: 4, Profiles: []string{"High"}, VTypes: []string{"h265"}},
		Sub: cgi.EncStreamOptions{Size: "896*512", Width: 896, Height: 512,
			BitRates: []int{256, 512}, FrameRates: []int{15, 10},
			GOPMin: 1, GOPMax: 4, Profiles: []string{"High"}, VTypes: []string{"h264"}},
	}}}
	page.Have = true
	cur := cgi.Enc{
		Main: cgi.EncStream{Size: "3840*2160", Width: 3840, Height: 2160, BitRate: 6144, FrameRate: 25, GOP: 1, Profile: "High", VType: "h265"},
		Sub:  cgi.EncStream{Size: "896*512", Width: 896, Height: 512, BitRate: 512, FrameRate: 15, GOP: 1, Profile: "High", VType: "h264"},
	}

	micOn := cur
	micOn.Audio = 1
	if err := checkEncChanges(cur, micOn, page); err != nil {
		t.Errorf("turning the microphone on was refused: %v", err)
	}

	mainFPS := cur
	mainFPS.Main.FrameRate = 20
	if err := checkEncChanges(cur, mainFPS, page); err == nil {
		t.Error("a main change kept a bitrate the camera does not offer and was not refused")
	}
}
