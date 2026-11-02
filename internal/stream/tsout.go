package stream

import (
	"time"

	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/ts"
)

// tsOutput is a stream's MPEG-TS muxer across camera connections, kept on
// its hub (hub.Keep), which outlives them. A new muxer per connection
// started PTS, PCR and the continuity counters over at zero on every
// reconnect while HTTP readers stayed attached, and go2rtc reading the .ts
// for Frigate then made recordings unusable for as long as the previous
// connection had lasted (a link-drop test, 10-30). The RTSP output already
// carried its clock over (Clock.Resume); this does the same here. Only one
// connection feeds a hub at a time, so it has no lock.
type tsOutput struct {
	mux       *ts.Muxer
	lastVideo time.Time // wall time of the latest video frame
	resume    bool      // a new connection: the next muxer call resumes
}

func tsOutputFor(h *hub.Hub) *tsOutput {
	return h.Keep(func() any { return &tsOutput{} }).(*tsOutput)
}

// connected marks a new camera connection.
func (o *tsOutput) connected() {
	o.resume = o.mux != nil
}

// muxer is the muxer for a connection's first video frame, of codec, at
// now. Over a reconnect it is the same muxer, resumed; when the codec or
// the audio track changed it is a new one that continues the timeline
// (ts.Muxer.Recodec). changed says the hub needs the new muxer's header.
func (o *tsOutput) muxer(codec string, audio bool, now time.Time) (m *ts.Muxer, changed bool, err error) {
	gap := now.Sub(o.lastVideo)
	switch {
	case o.mux == nil:
		if audio {
			m, err = ts.NewMuxerWithAudio(codec, "aac")
		} else {
			m, err = ts.NewMuxer(codec)
		}
		changed = true
	case !o.mux.Carries(codec, audio):
		m, err = o.mux.Recodec(codec, audio, gap)
		changed = true
	default:
		m = o.mux
		if o.resume {
			m.Resume(gap)
		}
	}
	if err != nil {
		return nil, false, err
	}
	o.mux, o.resume, o.lastVideo = m, false, now
	return m, changed, nil
}
