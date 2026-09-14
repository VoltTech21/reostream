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

// Absent and unparseable are two different things, and this is where they
// part company. A config file that is not there is a fresh install that
// nobody has claimed yet: it has no cameras, and every page rendered before
// the claim -- the claim screen itself first of all -- draws the sidebar
// from this list, so an error there would make the very first page a new
// user sees fail. A config that exists and will not load is a broken file
// somebody wrote, and reading it as an empty fleet would make every camera
// look like it had vanished.
func TestAMissingConfigIsAnEmptyFleetButABrokenOneIsAnError(t *testing.T) {
	s, err := New(Options{AllowNoPassword: true, ConfigPath: "/nonexistent/config.toml"})
	if err != nil {
		t.Fatal(err)
	}
	cams, err := s.fleet()
	if err != nil {
		t.Fatalf("a missing config errored, but a fresh install has no config yet: %v", err)
	}
	if len(cams) != 0 {
		t.Fatalf("got %d cameras from a config that is not there", len(cams))
	}

	broken := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(broken, []byte("listen = \"unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bs, err := New(Options{AllowNoPassword: true, ConfigPath: broken})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bs.fleet(); err == nil {
		t.Fatal("a config that will not parse read as an empty fleet, which looks like every camera vanished")
	}
}
