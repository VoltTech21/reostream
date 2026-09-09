package baichuan

import "testing"

const abilityFixture = `<?xml version="1.0" encoding="UTF-8" ?>
<body>
<AbilityInfo version="1.1">
<userName>admin</userName>
<system>
<subModule>
<abilityValue>general_rw, version_ro, log_ro</abilityValue>
</subModule>
</system>
<image>
<subModule>
<channelId>0</channelId>
<abilityValue>ispBasic_rw, ledState_rw</abilityValue>
</subModule>
</image>
</AbilityInfo>
</body>`

func TestParseAbilitiesSplitsNameFromAccess(t *testing.T) {
	got, err := ParseAbilities([]byte(abilityFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d abilities, want 5: %+v", len(got), got)
	}

	// Sorted by module then name, so image comes before system.
	want := []Ability{
		{Module: "image", Name: "ispBasic", Writable: true},
		{Module: "image", Name: "ledState", Writable: true},
		{Module: "system", Name: "general", Writable: true},
		{Module: "system", Name: "log", Writable: false},
		{Module: "system", Name: "version", Writable: false},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("ability %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestParseAbilitiesRejectsGarbage(t *testing.T) {
	if _, err := ParseAbilities([]byte("not xml")); err == nil {
		t.Error("want an error for a reply that does not parse")
	}
}
