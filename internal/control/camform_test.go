package control

import (
	"net/url"
	"testing"

	"github.com/VoltTech21/reostream/internal/config"
)

func TestFormKeepsAnExistingPasswordWhenTheFieldIsLeftAlone(t *testing.T) {
	cfg := &config.Config{Cameras: []config.Camera{
		{Name: "a", Address: "1.1.1.1", Username: "admin", Password: "secret", Streams: []string{"main"}},
	}}
	form := url.Values{
		"name":     {"a"},
		"address":  {"1.1.1.1"},
		"username": {"admin"},
		"password": {passwordUnchanged},
		"streams":  {"main"},
	}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if cfg.Cameras[0].Password != "secret" {
		t.Fatalf("password became %q; the form must not be able to blank a password it never showed", cfg.Cameras[0].Password)
	}
}

func TestFormSetsANewPassword(t *testing.T) {
	cfg := &config.Config{Cameras: []config.Camera{
		{Name: "a", Address: "1.1.1.1", Password: "secret", Streams: []string{"main"}},
	}}
	form := url.Values{
		"name":     {"a"},
		"address":  {"1.1.1.1"},
		"username": {""},
		"password": {"$CAM_PW"},
		"streams":  {"main"},
	}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if cfg.Cameras[0].Password != "$CAM_PW" {
		t.Fatalf("password is %q", cfg.Cameras[0].Password)
	}
}

func TestFormAddsANewCamera(t *testing.T) {
	cfg := &config.Config{}
	form := url.Values{
		"name":     {"new"},
		"address":  {"192.0.2.9"},
		"username": {"admin"},
		"password": {""},
		"streams":  {"main", "sub"},
	}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cameras) != 1 || len(cfg.Cameras[0].Streams) != 2 {
		t.Fatalf("got %+v", cfg.Cameras)
	}
}

func TestFormRejectsAStreamTheDaemonDoesNotKnow(t *testing.T) {
	cfg := &config.Config{}
	form := url.Values{
		"name":    {"new"},
		"address": {"192.0.2.9"},
		"streams": {"ultra"},
	}
	if err := applyCameraForm(cfg, form); err == nil {
		t.Fatal("an unknown stream name was accepted")
	}
}
