package baichuan

import (
	"encoding/xml"
	"testing"
)

var extensionSamples = []string{
	xmlHeader + `<Extension version="1.1"><binaryData>1</binaryData></Extension>`,
	xmlHeader + "\n" + `<Extension version="1.1">` + "\n<binaryData>1</binaryData>\n</Extension>\n",
	xmlHeader + `<Extension version="1.1"><encryptLen>1024</encryptLen><binaryData>1</binaryData></Extension>`,
	xmlHeader + `<Extension version="1.1"><checkPos>0</checkPos><checkValue>-234853339</checkValue></Extension>`,
	`<?xml version="1.0" encoding="UTF-8" ?>` + "\n" + `<Extension version="1.1">` + "\n<channelId>0</channelId>\n</Extension>\n",
	`<Extension><binaryData> 7 </binaryData></Extension>`,
	`<Extension><binaryData></binaryData></Extension>`,
	`<Extension/>`,
	`<Extension><binaryData>x</binaryData></Extension>`,
	`<Extension><binaryData>1</binaryData><binaryData>2</binaryData></Extension>`,
	`<Extension><a><binaryData>1</binaryData></a></Extension>`,
	`<Extension><binaryData>1&amp;</binaryData></Extension>`,
	`<Extension><binaryData>1</binaryDat></Extension>`,
	`<Other><binaryData>1</binaryData></Other>`,
	`<?xml version="1.1"?><Extension><binaryData>1</binaryData></Extension>`,
	`<?xml version="1.0" encoding="GB2312"?><Extension><binaryData>1</binaryData></Extension>`,
	`<Extension version="a>b"><binaryData>1</binaryData></Extension>`,
	`<Extensions><binaryData>1</binaryData></Extensions>`,
	`<Extension><binaryData>1</binaryData>`,
	`<Extension><binaryData>99999999999999999999</binaryData></Extension>`,
	`<Extension><encryptLen>-5</encryptLen></Extension>`,
	"<Extension>\x00</Extension>",
	`<Extension><binaryData>1</binaryData></Extension><x/>`,
}

func xmlExtension(x []byte) (extensionHeader, bool) {
	var ext extensionHeader
	if err := xml.Unmarshal(x, &ext); err != nil {
		return extensionHeader{}, false
	}
	return ext, true
}

// Whatever scanExtension accepts it must read exactly as encoding/xml does,
// since it stands in for it on the hot path.
func TestScanExtensionAgreesWithXML(t *testing.T) {
	fast := 0
	for _, s := range extensionSamples {
		got, ok := scanExtension([]byte(s))
		if !ok {
			continue
		}
		fast++
		want, wok := xmlExtension([]byte(s))
		if !wok || got.BinaryData != want.BinaryData || got.EncryptLen != want.EncryptLen {
			t.Errorf("%q: scan = %+v; encoding/xml = %+v, %v", s, got, want, wok)
		}
	}
	// The camera forms must take the fast path, or this saves nothing.
	for _, s := range extensionSamples[:5] {
		if _, ok := scanExtension([]byte(s)); !ok {
			t.Errorf("%q: not read by scanExtension", s)
		}
	}
	if fast < 5 {
		t.Fatalf("only %d samples took the fast path", fast)
	}
}

func FuzzScanExtension(f *testing.F) {
	for _, s := range extensionSamples {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		got, ok := scanExtension(b)
		if !ok {
			return
		}
		want, wok := xmlExtension(b)
		if !wok || got.BinaryData != want.BinaryData || got.EncryptLen != want.EncryptLen {
			t.Fatalf("%q: scan = %+v; encoding/xml = %+v, %v", b, got, want, wok)
		}
	})
}

func BenchmarkParseExtension(b *testing.B) {
	x := []byte(extensionSamples[3])
	for b.Loop() {
		parseExtension(x)
	}
}

func BenchmarkParseExtensionXML(b *testing.B) {
	x := []byte(extensionSamples[3])
	for b.Loop() {
		xmlExtension(x)
	}
}
