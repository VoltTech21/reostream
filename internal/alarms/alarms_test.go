package alarms

import (
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Shaped like a real push from an RLC-810A (front_door, 2026-10-24): motion
// with no AI type, then a person.
func TestStateAndChanges(t *testing.T) {
	md, _ := baichuan.ParseAlarmEvents([]byte(`<?xml version="1.0" encoding="UTF-8" ?><body><AlarmEventList version="1.1"><AlarmEvent version="1.1"><channelId>0</channelId><status>MD</status><AItype>none</AItype><recording>0</recording><timeStamp>0</timeStamp></AlarmEvent></AlarmEventList></body>`))
	person, _ := baichuan.ParseAlarmEvents([]byte(`<?xml version="1.0" encoding="UTF-8" ?><body><AlarmEventList version="1.1"><AlarmEvent version="1.1"><channelId>0</channelId><status>MD</status><AItype>people</AItype><recording>1</recording><timeStamp>0</timeStamp></AlarmEvent></AlarmEventList></body>`))
	quiet, _ := baichuan.ParseAlarmEvents([]byte(`<?xml version="1.0" encoding="UTF-8" ?><body><AlarmEventList version="1.1"><AlarmEvent version="1.1"><channelId>0</channelId><status>none</status><AItype>none</AItype><recording>0</recording><timeStamp>0</timeStamp></AlarmEvent></AlarmEventList></body>`))
	now := time.Now()
	a, b, c := stateOf(md), stateOf(person), stateOf(quiet)
	if !a.Motion || len(a.Objects) != 0 || !b.Motion || len(b.Objects) != 1 || b.Objects[0] != "person" || c.Motion {
		t.Fatalf("states %+v %+v %+v", a, b, c)
	}
	ch := diff("front_door", now, State{}, a)
	if len(ch) != 1 || ch[0].Kind != "motion" || !ch[0].On {
		t.Fatalf("motion start: %+v", ch)
	}
	ch = diff("front_door", now, a, b)
	if len(ch) != 1 || ch[0].Kind != "person" || !ch[0].On {
		t.Fatalf("person start: %+v", ch)
	}
	ch = diff("front_door", now, b, c)
	if len(ch) != 2 {
		t.Fatalf("both should end: %+v", ch)
	}
}

func TestDoorbellPressIsVisitorNotMotion(t *testing.T) {
	doc := []byte(`<?xml version="1.0" encoding="UTF-8" ?><body><AlarmEventList version="1.1"><AlarmEvent version="1.1"><channelId>0</channelId><status>visitor</status><AItype>none</AItype><recording>0</recording><timeStamp>0</timeStamp></AlarmEvent></AlarmEventList></body>`)
	ev, err := baichuan.ParseAlarmEvents(doc)
	if err != nil {
		t.Fatal(err)
	}
	s := stateOf(ev)
	if s.Motion || len(s.Objects) != 1 || s.Objects[0] != "visitor" {
		t.Fatalf("got %+v", s)
	}
	ev[0].Status = "MD,visitor"
	if s := stateOf(ev); !s.Motion || len(s.Objects) != 1 {
		t.Fatalf("MD,visitor: got %+v", s)
	}
}
