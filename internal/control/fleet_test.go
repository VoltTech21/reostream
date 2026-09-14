package control

import (
	"os"
	"path/filepath"
	"testing"
)

// The fleet comes from reostream's config read with the RESOLVING loader,
// because connecting to a camera needs a real password rather than a "$NAME"
// reference. This is the opposite of the operator page's camera form, which
// must use LoadRaw precisely because it writes that file back and resolving
// would bake every secret into it. This program never writes that file.
func TestFleetResolvesPasswordsFromTheEnvironment(t *testing.T) {
	t.Setenv("CAM_PW", "the-real-secret")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte(`listen = "0.0.0.0:8560"

[[camera]]
name = "lounge"
address = "192.0.2.50"
username = "admin"
password = "$CAM_PW"
streams = ["main"]
`), 0o600)

	s, err := New(Options{AllowNoPassword: true, ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	cams, err := s.fleet()
	if err != nil {
		t.Fatal(err)
	}
	if len(cams) != 1 {
		t.Fatalf("got %d cameras", len(cams))
	}
	if cams[0].Password != "the-real-secret" {
		t.Fatalf("password is %q, want the resolved value: this program has to connect", cams[0].Password)
	}
}

func TestFleetReportsAMissingConfigRatherThanServingAnEmptyFleet(t *testing.T) {
	s, err := New(Options{AllowNoPassword: true, ConfigPath: "/nonexistent/config.toml"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.fleet(); err == nil {
		t.Fatal("a missing config read as an empty fleet, which looks like every camera vanished")
	}
}
