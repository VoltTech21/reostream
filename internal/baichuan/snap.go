package baichuan

import (
	"context"
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

// SnapRefusedError is a camera answering a Snap with a status other than
// success: 405 from a model or recorder that does not make stills over this
// protocol, 400 for a stream it does not have.
type SnapRefusedError struct{ Status int16 }

func (e *SnapRefusedError) Error() string {
	return "baichuan: camera refused the snap: " + ExplainStatus(e.Status)
}

// TakeSnap asks conn's camera for a still and waits until all of it has
// arrived or ctx ends. It returns the image, trimmed to the size the camera
// declared, and the filename the camera gave it.
//
// The bytes are whatever the camera made, returned as they came. Every
// camera that has answered one so far made a JPEG, but checking that is the
// caller's business: reocam saves the file regardless, the control page
// refuses to label anything else image/jpeg.
func TakeSnap(ctx context.Context, conn *Conn, stream string) ([]byte, string, error) {
	if err := conn.Snap(stream); err != nil {
		return nil, "", err
	}
	var r SnapReader
	for !r.Done() {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				if err := conn.Err(); err != nil {
					return nil, "", fmt.Errorf("baichuan: connection closed after %d of %d snap bytes: %w", len(r.data), r.Size(), err)
				}
				return nil, "", fmt.Errorf("baichuan: connection closed after %d of %d snap bytes", len(r.data), r.Size())
			}
			// Status 0 is what a continuation carries; only a reply that
			// says something else is a refusal.
			if m.Header.MsgID == MsgIDSnap && m.Header.Status() != 0 && m.Header.Status() != 200 {
				return nil, "", &SnapRefusedError{Status: m.Header.Status()}
			}
			if err := r.Read(m); err != nil {
				return nil, "", err
			}
		case <-ctx.Done():
			return nil, "", fmt.Errorf("baichuan: waiting for the snap (%d of %d bytes): %w", len(r.data), r.Size(), ctx.Err())
		}
	}
	return r.Image(), r.Name(), nil
}
