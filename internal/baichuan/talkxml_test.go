package baichuan

import (
	"bytes"
	"testing"
)

// The dissector records a 104 byte extension and a 394 byte TalkConfig on the
// wire. Those counts are the only check available that these documents match
// what a camera expects, byte for byte, so they are asserted rather than
// eyeballed.
func TestTalkXMLMatchesTheWireByteCounts(t *testing.T) {
	ext, err := channelXML(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ext) != 104 {
		t.Errorf("extension = %d bytes, want 104:\n%s", len(ext), ext)
	}

	body, err := talkConfigXML(0, TalkFormat{
		Duplex: "FDX", StreamMode: "followVideoStream", AudioType: "adpcm",
		SampleRate: 16000, SamplePrecision: 16, LengthPerEncoder: 1024, SoundTrack: "mono",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 394 {
		t.Errorf("talk config = %d bytes, want 394:\n%s", len(body), body)
	}
}

// WriteParts must seal the XML and the binary section as two independent
// cipher streams. Sealing them as one continuous stream, or leaving the
// binary in plaintext, both get a TalkConfig refused with status 400 by a
// real camera, and neither is visible without one.
func TestWritePartsSealsEachSectionIndependently(t *testing.T) {
	key := AESKey("nonce", "")
	xmlPart := []byte("<Extension/>")
	binPart := []byte("binary section")

	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetAESKey(key)
	h := Header{MsgID: MsgIDTalkConfig, Class: ClassModern24}
	if err := w.WriteParts(h, xmlPart, binPart); err != nil {
		t.Fatal(err)
	}

	out := buf.Bytes()[HeaderLen(ClassModern24):]
	gotXML, err := AESDecrypt(key, out[:len(xmlPart)])
	if err != nil {
		t.Fatal(err)
	}
	gotBin, err := AESDecrypt(key, out[len(xmlPart):])
	if err != nil {
		t.Fatal(err)
	}

	if string(gotXML) != string(xmlPart) {
		t.Errorf("xml = %q, want %q", gotXML, xmlPart)
	}
	// This is the assertion that matters: decrypting the binary section from
	// the start of the cipher stream must recover it. It would come out as
	// noise if the section continued the XML's stream.
	if string(gotBin) != string(binPart) {
		t.Errorf("binary = %q, want %q; the section must start its own cipher stream", gotBin, binPart)
	}
}

// A media section is plaintext. Sealing it, which is right for a TalkConfig's
// second XML document, produced a talk stream a camera accepted without
// error and did not play: the failure is completely silent on the wire and
// only shows up as no sound in the room.
func TestWriteMediaLeavesTheMediaSectionPlain(t *testing.T) {
	key := AESKey("nonce", "")
	xmlPart := []byte("<Extension/>")
	media := []byte("adpcm packet")

	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetAESKey(key)
	if err := w.WriteMedia(Header{MsgID: MsgIDTalk, Class: ClassModern24}, xmlPart, media); err != nil {
		t.Fatal(err)
	}

	out := buf.Bytes()[HeaderLen(ClassModern24):]
	if got := string(out[len(xmlPart):]); got != string(media) {
		t.Errorf("media section = %q, want it unencrypted as %q", got, media)
	}
}
