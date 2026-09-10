package control_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/control"
)

func TestAddCameraReportsWhatTheProbeFound(t *testing.T) {
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		Probe: func(ctx context.Context, addr, user, pass string) control.CameraReport {
			return control.CameraReport{
				Model: "RLC-810A",
				Streams: []control.StreamReport{
					{Name: "main", Codec: "H265", Width: 3840, Height: 2160},
					{Name: "sub", Codec: "H264", Width: 640, Height: 360},
				},
			}
		},
	})
	resp, err := http.PostForm(ts.URL+"/setup/probe", url.Values{
		"address": {"192.0.2.50"}, "username": {"admin"}, "password": {""},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"RLC-810A", "H265", "3840"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("probe result does not mention %q: %s", want, body)
		}
	}
}

func TestAddCameraReportsAFailureInPlainWords(t *testing.T) {
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		Probe: func(ctx context.Context, addr, user, pass string) control.CameraReport {
			// A wrong password on these cameras reads as an empty login
			// reply, which looks exactly like a camera holding a dead
			// session until you print the status. It is a 401.
			return control.CameraReport{Err: "authentication failed (401): check the username and password"}
		},
	})
	resp, err := http.PostForm(ts.URL+"/setup/probe", url.Values{
		"address": {"192.0.2.50"}, "username": {"admin"}, "password": {"wrong"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "authentication failed") {
		t.Fatalf("failure not reported: %s", body)
	}
}
