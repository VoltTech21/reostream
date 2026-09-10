package control

import "testing"

func TestPlayableStreamPrefersSubThenExtern(t *testing.T) {
	cases := []struct {
		name    string
		streams map[string]string
		want    string
		ok      bool
	}{
		{
			name:    "h264 sub is the cheapest thing a browser can play",
			streams: map[string]string{"main": "H265", "sub": "H264", "extern": "H264"},
			want:    "/gate_sub.ts",
			ok:      true,
		},
		{
			// Some models encode the substream in HEVC too, and no browser
			// plays HEVC here. extern is confirmed H.264 at 896x512.
			name:    "hevc sub falls through to extern",
			streams: map[string]string{"main": "H265", "sub": "H265", "extern": "H264"},
			want:    "/gate_extern.ts",
			ok:      true,
		},
		{
			name:    "nothing playable",
			streams: map[string]string{"main": "H265", "sub": "H265"},
			ok:      false,
		},
		{
			// A stream that has never delivered a keyframe reports an empty
			// codec. Guessing it is H.264 would show a broken player rather
			// than an honest "not playable yet".
			name:    "unknown codec is not assumed playable",
			streams: map[string]string{"sub": ""},
			ok:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := playableStream("gate", tc.streams)
			if ok != tc.ok {
				t.Fatalf("ok=%v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
