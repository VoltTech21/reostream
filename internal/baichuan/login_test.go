package baichuan

import (
	"strings"
	"testing"
)

// The captured client sent 31-character hashes, not 32.
func TestHashCredMatchesCapture(t *testing.T) {
	if got := hashCred("admin", fixtureNonce); got != "DD95A35D0F0C9B94E01B9A1AB6856E0" {
		t.Errorf("user hash = %q", got)
	}
	if got := hashCred("", fixtureNonce); got != "129427154AD8D1532407058FAB85DEB" {
		t.Errorf("password hash = %q", got)
	}
	if n := len(hashCred("admin", fixtureNonce)); n != 31 {
		t.Errorf("hash length = %d, want 31", n)
	}
}

// Our login XML must match the shape the captured client sent.
func TestLoginXMLMatchesCapture(t *testing.T) {
	got, err := loginXML("admin", "", fixtureNonce)
	if err != nil {
		t.Fatalf("loginXML: %v", err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?><body><LoginUser version="1.1">` +
		`<userName>DD95A35D0F0C9B94E01B9A1AB6856E0</userName>` +
		`<password>129427154AD8D1532407058FAB85DEB</password><userVer>1</userVer>` +
		`</LoginUser><LoginNet version="1.1"><type>LAN</type><udpPort>0</udpPort></LoginNet></body>`
	if string(got) != want {
		t.Errorf("login XML differs from the capture:\n got: %s\nwant: %s", got, want)
	}
}

func TestParseNonceFromCapture(t *testing.T) {
	var xmlBody []byte
	walkCapture(t, "login_s2c.bin", func(i int, h Header, body []byte) bool {
		xmlBody = BCCrypt(h.EncOffset, body)
		return false
	})
	got, err := parseNonce(xmlBody)
	if err != nil {
		t.Fatalf("parseNonce: %v", err)
	}
	if got != fixtureNonce {
		t.Errorf("nonce = %q, want %q", got, fixtureNonce)
	}
}

func TestPreviewXML(t *testing.T) {
	got, err := previewXML(0, 1, StreamSub)
	if err != nil {
		t.Fatalf("previewXML: %v", err)
	}
	for _, want := range []string{"<Preview", "<channelId>0</channelId>", "<handle>1</handle>",
		"<streamType>subStream</streamType>"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("preview XML missing %q:\n%s", want, got)
		}
	}
}

func TestStreamID(t *testing.T) {
	for _, tc := range []struct {
		stream string
		want   byte
	}{{StreamMain, 0}, {StreamSub, 1}, {StreamExtern, 4}} {
		if got := StreamID(tc.stream); got != tc.want {
			t.Errorf("StreamID(%s) = %d, want %d", tc.stream, got, tc.want)
		}
	}
}
