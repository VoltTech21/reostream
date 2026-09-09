package baichuan

import (
	"bytes"
	"testing"
)

func snapMessages(t *testing.T, size int, chunks ...[]byte) []Message {
	t.Helper()
	reply := []byte(`<?xml version="1.0" encoding="UTF-8" ?>
<body>
<Snap version="1.1">
<channelId>0</channelId>
<fileName>01_20260909121822000.jpg</fileName>
<time>0</time>
<pictureSize>` + itoa(size) + `</pictureSize>
</Snap>
</body>`)
	out := []Message{{Header: Header{MsgID: MsgIDSnap}, XML: reply}}
	for _, c := range chunks {
		out = append(out, Message{Header: Header{MsgID: MsgIDSnap}, Payload: c})
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// The image arrives across several messages and the last one is padded, so
// the reader has to both join the pieces and trim to the declared size.
func TestSnapReaderJoinsAndTrims(t *testing.T) {
	img := bytes.Repeat([]byte{0xAB}, 300)
	img[0], img[1] = 0xFF, 0xD8

	var r SnapReader
	// Three chunks, the last carrying 40 bytes of filler past the image.
	chunks := [][]byte{img[:100], img[100:250], append(img[250:], bytes.Repeat([]byte{0}, 40)...)}
	for _, m := range snapMessages(t, len(img), chunks...) {
		if err := r.Read(m); err != nil {
			t.Fatal(err)
		}
	}

	if !r.Done() {
		t.Fatal("reader says the image is incomplete")
	}
	if got := r.Image(); !bytes.Equal(got, img) {
		t.Errorf("image is %d bytes, want %d; filler must be trimmed", len(got), len(img))
	}
	if r.Name() != "01_20260909121822000.jpg" {
		t.Errorf("Name = %q", r.Name())
	}
}

// Done must stay false until every byte has arrived, or a caller writes out a
// truncated file and it still opens in most viewers.
func TestSnapReaderWaitsForTheWholeImage(t *testing.T) {
	var r SnapReader
	for _, m := range snapMessages(t, 500, bytes.Repeat([]byte{1}, 200)) {
		if err := r.Read(m); err != nil {
			t.Fatal(err)
		}
	}
	if r.Done() {
		t.Error("Done reported true with 200 of 500 bytes read")
	}
	if r.Image() != nil {
		t.Error("Image returned data before the whole picture arrived")
	}
}

// Messages for other things share the connection and must be ignored.
func TestSnapReaderIgnoresUnrelatedMessages(t *testing.T) {
	var r SnapReader
	if err := r.Read(Message{Header: Header{MsgID: MsgIDVideo}, Payload: []byte("video")}); err != nil {
		t.Fatal(err)
	}
	if len(r.Image()) != 0 || r.Size() != 0 {
		t.Error("a video message was taken as part of the snap")
	}
}
