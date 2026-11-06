package baichuan

import "testing"

// A second lens is a camera reporting two channels. A recorder reports a
// channel per camera it holds and is not one; nor is a camera that does
// not say what it is.
func TestSecondLens(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		want bool
	}{
		{`<body><DeviceInfo><type>ipc</type><typeInfo>IPC</typeInfo><channelNum>2</channelNum></DeviceInfo></body>`, true},
		{`<body><DeviceInfo><type>ipc</type><typeInfo>IPC</typeInfo><channelNum>1</channelNum></DeviceInfo></body>`, false},
		{`<body><DeviceInfo><type>wifi_nvr</type><typeInfo>NVR</typeInfo><channelNum>12</channelNum></DeviceInfo></body>`, false},
		{`<body><DeviceInfo><channelNum>2</channelNum></DeviceInfo></body>`, false},
	} {
		d, err := ParseDeviceInfo([]byte(tc.doc))
		if err != nil {
			t.Fatal(err)
		}
		if got := d.SecondLens(); got != tc.want {
			t.Errorf("%s: SecondLens = %v, want %v", tc.doc, got, tc.want)
		}
	}
}
