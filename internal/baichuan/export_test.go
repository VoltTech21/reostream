package baichuan

import "time"

// SetStallTimeout shortens stallTimeout for a test of package baichuan_test,
// and returns the function that puts it back.
func SetStallTimeout(d time.Duration) (restore func()) {
	old := stallTimeout
	stallTimeout = d
	return func() { stallTimeout = old }
}
