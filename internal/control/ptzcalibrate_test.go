package control

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

// A TrackMix answers 341 only when its minute-long sweep ends, and nothing
// else on that session until then, so a calibrate was reported "refused:
// idle timeout" while the camera calibrated (fake hardware, 10-30, bug 1).
// Silence now means started, confirmed by testStat 1 on a second session;
// a 400 is still a refusal.
func TestCalibrateTakesSilenceAsStartedAndConfirmsOnAFreshSession(t *testing.T) {
	autoTest := func(stat string) string {
		return testXMLHeader + `<body><PtzAutoTest version="1.1"><channelId>0</channelId><testStat>` + stat + `</testStat></PtzAutoTest></body>`
	}
	for _, tc := range []struct {
		name        string
		first       []func(key []byte) []byte
		confirm     string // testStat on the second session, "" for none
		wantOutcome string
		want        string
	}{
		{"silent then running", []func([]byte) []byte{
			func(k []byte) []byte { return statusReply(t, k, baichuan.MsgIDPtzAutoTest, 200, autoTest("2")) },
		}, "1", "accepted", "calibrating, this takes a minute or two"},
		{"341 refused", []func([]byte) []byte{
			func(k []byte) []byte { return statusReply(t, k, baichuan.MsgIDPtzAutoTest, 200, autoTest("2")) },
			func(k []byte) []byte { return statusReply(t, k, baichuan.MsgIDPtzAutoTest2, 400, "") },
		}, "", "refused", "status 400"},
		{"already running", []func([]byte) []byte{
			func(k []byte) []byte { return statusReply(t, k, baichuan.MsgIDPtzAutoTest, 200, autoTest("1")) },
			func(k []byte) []byte { return statusReply(t, k, baichuan.MsgIDPtzAutoTest2, 400, "") },
		}, "1", "refused", "already running its pan/tilt self check"},
		{"340 refused", []func([]byte) []byte{
			func(k []byte) []byte { return statusReply(t, k, baichuan.MsgIDPtzAutoTest, 400, "") },
		}, "", "refused", "status 400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := baichuan.AESKey(testProbeNonce, "")
			first := loginHandshake(testProbeNonce, testProbeDeviceInfo)
			for _, f := range tc.first {
				first = append(first, f(key)...)
			}
			cams := []*fakecam.Camera{fakecam.New(t, first)}
			if tc.confirm != "" {
				second := loginHandshake(testProbeNonce, testProbeDeviceInfo)
				second = append(second, statusReply(t, key, baichuan.MsgIDPtzAutoTest, 200, autoTest(tc.confirm))...)
				cams = append(cams, fakecam.New(t, second))
			}
			var dials atomic.Int32
			s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one"),
				Dial: func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
					n := int(dials.Add(1)) - 1
					return baichuan.Dial(ctx, cams[min(n, len(cams)-1)].Addr(), baichuan.Options{Password: ""})
				}})
			// Known to be a camera on its own, so the recorder check
			// does not take a session of its own.
			s.recorders = map[string]bool{config.NormalizeAddr("192.0.2.10"): false}
			ts := httptest.NewServer(s.Handler())
			t.Cleanup(ts.Close)
			banner := takenFlash(t, s, postForm(t, flashBrowser(t), ts.URL+"/cameras/one/ptz/calibrate", url.Values{}))
			if banner.Outcome != tc.wantOutcome || !strings.Contains(banner.Message, tc.want) {
				t.Errorf("banner %+v, want %s with %q", banner, tc.wantOutcome, tc.want)
			}
			if tc.confirm != "" && dials.Load() != 2 {
				t.Errorf("%d sessions, want the start and a fresh one to confirm", dials.Load())
			}
		})
	}
}
