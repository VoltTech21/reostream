package baichuan

import "testing"

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
