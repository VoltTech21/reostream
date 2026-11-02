package control

import "testing"

// A TrackMix PoE with a card says diskNum 0 in its support block; message
// 102 is where the card shows (read from one 2026-11-02).
func TestHasCardReadsTheCardList(t *testing.T) {
	trackmix := []byte(`<?xml version="1.0" encoding="UTF-8" ?>
<body>
<HddInfoList version="1.1">
<HddInfo>
<number>0</number>
<capacity>28</capacity>
<capacityM>987</capacityM>
<format>1</format>
<mount>1</mount>
<remainSize>28</remainSize>
<remainSizeM>515</remainSizeM>
<capacityV2>31100698624</capacityV2>
<remainSizeV2>30605592946</remainSizeV2>
</HddInfo>
</HddInfoList>
</body>`)
	none := []byte(`<?xml version="1.0" encoding="UTF-8" ?>
<body>
<HddInfoList version="1.1" />
</body>`)
	if !hasCard(trackmix) {
		t.Error("the TrackMix's card was not seen")
	}
	if hasCard(none) {
		t.Error("an empty list was read as a card")
	}
}
