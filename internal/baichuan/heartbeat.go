package baichuan

import (
	"encoding/xml"
	"fmt"
	"time"
)

// HeartBeatReply is what a camera returns for a heartbeat.
//
// Sec and Usec are the camera's own clock, which makes this a round trip
// measurement: the difference against a local reading is clock offset plus
// path delay, and the change in that difference between two heartbeats is
// drift. OverlapCount is the camera's count of requests that arrived while an
// earlier one was still outstanding, so a climbing value means the client is
// heartbeating faster than the camera can answer.
type HeartBeatReply struct {
	Size         int
	Sec          int64
	Usec         int64
	OverlapCount int
	Delay        int
}

// CameraTime is the camera's clock at the moment it answered.
func (h HeartBeatReply) CameraTime() time.Time {
	return time.Unix(h.Sec, h.Usec*int64(time.Microsecond))
}

type heartBeatReplyXML struct {
	XMLName   xml.Name `xml:"body"`
	HeartBeat struct {
		Size         int   `xml:"size"`
		Sec          int64 `xml:"sec"`
		Usec         int64 `xml:"usec"`
		OverlapCount int   `xml:"overlapCount"`
		Delay        int   `xml:"delay"`
	} `xml:"HeartBeat"`
}

// ParseHeartBeat reads a heartbeat reply.
func ParseHeartBeat(x []byte) (HeartBeatReply, error) {
	var r heartBeatReplyXML
	if err := xml.Unmarshal(x, &r); err != nil {
		return HeartBeatReply{}, fmt.Errorf("baichuan: parse heartbeat: %w", err)
	}
	return HeartBeatReply{
		Size:         r.HeartBeat.Size,
		Sec:          r.HeartBeat.Sec,
		Usec:         r.HeartBeat.Usec,
		OverlapCount: r.HeartBeat.OverlapCount,
		Delay:        r.HeartBeat.Delay,
	}, nil
}
