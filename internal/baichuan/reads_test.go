package baichuan

import "testing"

// This is the exact disagreement a live RLC-810A produced between
// cmd/reocam's probe command (which used to check body length before
// status) and camctl's page (which checked status alone): usercfg answers
// 400 with a non-empty error body, and dns/syscpuload answer 200 with an
// empty one. A caller that trusts body length over status gets both
// backwards. ClassifyRead is now the one place either question gets asked.
func TestClassifyReadIsStatusOnly(t *testing.T) {
	cases := []struct {
		name   string
		status int16
		want   ReadOutcome
	}{
		{"200 with a body", 200, ReadSupported},
		// The usercfg case: understood and refused, but with a body.
		{"400 with a body", StatusBadRequest, ReadWantsParams},
		// The dns/syscpuload case: supported, but nothing to say.
		{"200 with no body", 200, ReadSupported},
		{"405", StatusNotImplemented, ReadAbsent},
	}
	for _, tc := range cases {
		if got := ClassifyRead(tc.status); got != tc.want {
			t.Errorf("%s: ClassifyRead(%d) = %q, want %q", tc.name, tc.status, got, tc.want)
		}
	}
}
