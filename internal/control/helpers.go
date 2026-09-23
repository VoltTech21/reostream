package control

import (
	"errors"
	"runtime/debug"
	"strings"
)

// dict builds a map from alternating keys and values, so a template can
// hand one {{define}} more than one value.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, errors.New("dict: odd argument count")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, errors.New("dict: non-string key")
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

// buildVersion is the rail's version chip: the module version when the
// binary was built from a tag, else the short VCS revision, else "dev".
// It is a template function rather than a field on every page struct.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += "+"
	}
	return rev
}

// streamOf is the stream half of a "<camera>/<stream>" status name, for a
// page that is already about one camera.
func streamOf(name string) string {
	if _, stream, ok := strings.Cut(name, "/"); ok {
		return stream
	}
	return name
}
