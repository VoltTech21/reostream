package baichuan

import (
	"encoding/xml"
	"fmt"
)

// snapReply is the camera's first answer to a Snap: the file it made and how
// big it is. The bytes arrive afterwards.
type snapReply struct {
	XMLName xml.Name `xml:"body"`
	Snap    struct {
		ChannelID   int    `xml:"channelId"`
		FileName    string `xml:"fileName"`
		PictureSize int    `xml:"pictureSize"`
	} `xml:"Snap"`
}

// SnapReader assembles a still from the messages a Snap request produces.
//
// The size is not known until the first reply arrives and the image is
// usually split across several messages, so this is a small state machine
// rather than a single read. Feed it every message until Done reports true.
type SnapReader struct {
	name string
	size int
	data []byte
	got  bool
}

// Read takes one message. It reports an error only for a reply it cannot
// parse; a message that has nothing to do with the snap is ignored.
func (s *SnapReader) Read(m Message) error {
	if m.Header.MsgID != MsgIDSnap {
		return nil
	}
	if !s.got && len(m.XML) > 0 {
		var r snapReply
		if err := xml.Unmarshal(m.XML, &r); err != nil {
			return fmt.Errorf("baichuan: parse snap reply: %w", err)
		}
		if r.Snap.PictureSize > 0 {
			s.name, s.size, s.got = r.Snap.FileName, r.Snap.PictureSize, true
		}
	}
	if len(m.Payload) > 0 {
		s.data = append(s.data, m.Payload...)
	}
	return nil
}

// Done reports whether the whole image has arrived.
func (s *SnapReader) Done() bool { return s.got && len(s.data) >= s.size }

// Image returns the still, trimmed to the size the camera declared. The
// trailing bytes exist because the final message is padded, the same filler
// the media path discards.
func (s *SnapReader) Image() []byte {
	if !s.Done() {
		return nil
	}
	return s.data[:s.size]
}

// Name is the filename the camera gave the still, which carries its
// timestamp: "01_20230518140240.jpg".
func (s *SnapReader) Name() string { return s.name }

// Size is the length the camera declared, valid once the first reply has
// been read.
func (s *SnapReader) Size() int { return s.size }
