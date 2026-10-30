package baichuan

import (
	"bytes"
	"strings"
	"testing"
)

const supportFixture = `<?xml version="1.0" encoding="UTF-8" ?>
<body>
<Support version="1.1">
<IOInputPortNum>0</IOInputPortNum>
<IOOutputPortNum>0</IOOutputPortNum>
<diskNum>0</diskNum>
<channelNum>1</channelNum>
<audioNum>1</audioNum>
<ptzMode>none</ptzMode>
<wifi>0</wifi>
<audioTalk>%s</audioTalk>
<audioAlarm>1</audioAlarm>
<noExternStream>%s</noExternStream>
</Support>
</body>`

func supportXML(talk, noExtern string) []byte {
	s := supportFixture
	// crude, but keeps the fixture readable as real camera output
	out := ""
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+1 < len(s) && s[i+1] == 's' {
			if n == 0 {
				out += talk
			} else {
				out += noExtern
			}
			n++
			i++
			continue
		}
		out += string(s[i])
	}
	return []byte(out)
}

// These two flags are the ones that were established the slow way against
// real cameras, so they are the ones worth pinning.
func TestSupportReportsTalkAndExternStream(t *testing.T) {
	tests := []struct {
		name           string
		talk, noExtern string
		wantTalk       bool
		wantExtern     bool
	}{
		{"fisheye: speaks, no balanced stream", "1", "1", true, false},
		{"pano: speaks, has balanced stream", "1", "0", true, true},
		{"the six others: no speaker", "0", "0", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := ParseSupport(supportXML(tt.talk, tt.noExtern))
			if err != nil {
				t.Fatal(err)
			}
			if s.CanTalk() != tt.wantTalk {
				t.Errorf("CanTalk = %v, want %v", s.CanTalk(), tt.wantTalk)
			}
			if s.HasExternStream() != tt.wantExtern {
				t.Errorf("HasExternStream = %v, want %v", s.HasExternStream(), tt.wantExtern)
			}
		})
	}
}

func TestSupportReportsNoPTZOnAFixedCamera(t *testing.T) {
	s, err := ParseSupport(supportXML("1", "0"))
	if err != nil {
		t.Fatal(err)
	}
	if s.HasPTZ() {
		t.Error("HasPTZ true for a camera reporting ptzMode none")
	}
}

// supportReply frames body as a camera's reply to the support read, with
// PayloadOff at off (0 for none), and reads it back through a Reader keyed
// with key.
func supportReply(t *testing.T, key, body []byte, off uint32) Message {
	t.Helper()
	h := Header{MsgID: MsgIDGetSupport, MsgLen: uint32(len(body)), EncByte: 200, Class: ClassModern24, PayloadOff: off}
	r := NewReader(bytes.NewReader(append(h.Encode(), body...)))
	r.SetAESKey(key)
	m, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSupportReplyLayouts(t *testing.T) {
	key := AESKey(fixtureNonce, fixturePassword)
	doc := supportXML("1", "0")
	enc := func(b []byte) []byte {
		out, err := AESEncrypt(key, b)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ext := []byte(`<?xml version="1.0" encoding="UTF-8" ?>` + "\n" + `<Extension version="1.1">` + "\n<channelId>0</channelId>\n</Extension>\n")

	for _, tc := range []struct {
		name string
		body []byte
		off  uint32
	}{
		{"document alone", enc(doc), 0},
		{"offset inside the document", enc(doc), 90},
		{"extension then a separately encrypted document", append(enc(ext), enc(doc)...), uint32(len(ext))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := parseSupportReply(supportReply(t, key, tc.body, tc.off))
			if err != nil {
				t.Fatal(err)
			}
			if !s.CanTalk() || s.Support.ChannelNum != 1 {
				t.Fatalf("parsed %+v", s.Support)
			}
		})
	}
}

func TestSupportReplyErrorDescribesTheReply(t *testing.T) {
	key := AESKey(fixtureNonce, fixturePassword)
	cut, err := AESEncrypt(key, supportXML("1", "0")[:90])
	if err != nil {
		t.Fatal(err)
	}
	_, err = parseSupportReply(supportReply(t, key, cut, 0))
	if err == nil {
		t.Fatal("a truncated document parsed")
	}
	for _, want := range []string{"status 200", "body 90 bytes", "payload offset 0", "<Support"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// The compact E1/E320/E330 builds report <ftp>0</ftp> and refuse every FTP
// request; an FTP-capable camera reports a bitmask; a camera that leaves
// the element out has said nothing, and must not be read as having no FTP.
func TestLacksFTPOnlyOnAPositiveNo(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		want bool
	}{
		{`<body><Support version="1.1"><ftp>0</ftp></Support></body>`, true},
		{`<body><Support version="1.1"><ftp>14311</ftp></Support></body>`, false},
		{`<body><Support version="1.1"><rtsp>1</rtsp></Support></body>`, false},
	} {
		s, err := ParseSupport([]byte(tc.doc))
		if err != nil {
			t.Fatal(err)
		}
		if got := s.LacksFTP(); got != tc.want {
			t.Errorf("%s: LacksFTP = %v, want %v", tc.doc, got, tc.want)
		}
	}
}
