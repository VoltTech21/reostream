package control

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Mount tables as a container sees them. The rootfs line is what Docker on
// Unraid 7.3 gave a Data path of /mnt/reo46-ramtest/reostream, a folder it
// had to create because nothing was there (#46).
const (
	mountUnraidRAM = `1880 1879 0:180 / / rw,relatime - overlay overlay rw
1889 1880 0:2 /mnt/reo46-ramtest/reostream /data rw - rootfs rootfs rw,size=8131140k
`
	mountUnraidAppdata = `1880 1879 0:180 / / rw,relatime - overlay overlay rw
1890 1880 0:50 /appdata/reostream /data rw,nosuid,nodev,noatime - fuse.shfs shfs rw,user_id=0
`
	mountVolume = `1880 1879 0:180 / / rw,relatime - overlay overlay rw
1890 1880 8:1 /var/lib/docker/volumes/reostream-data/_data /data rw,relatime - ext4 /dev/sda1 rw
1891 1880 0:60 / /data/tmp rw - tmpfs tmpfs rw
`
	mountTmp = `25 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw
40 25 0:35 / /tmp rw,nosuid,nodev - tmpfs tmpfs rw
41 25 0:36 / /my\040data rw - ext4 /dev/sdb1 rw
`
)

func withMountinfo(t *testing.T, table string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	old := mountinfo
	mountinfo = p
	t.Cleanup(func() { mountinfo = old })
}

func TestDataDirWarning(t *testing.T) {
	for _, c := range []struct {
		name, table, dir, want string
	}{
		{"unraid path in RAM", mountUnraidRAM, "/data", "/mnt/user/appdata/reostream"},
		{"unraid appdata", mountUnraidAppdata, "/data", ""},
		{"docker volume", mountVolume, "/data", ""},
		{"a deeper mount does not count for its parent", mountVolume, "/data", ""},
		{"plain binary in /tmp", mountTmp, "/tmp/reostream-data", "-data"},
		{"tmpfs prefix is not a parent", mountTmp, "/tmpdata", ""},
		{"escaped mount point", mountTmp, "/my data/reostream", ""},
		{"container layer itself", mountUnraidRAM, "/srv", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			withMountinfo(t, c.table)
			got := DataDirWarning(c.dir)
			if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("DataDirWarning(%q) = %q, want one containing %q", c.dir, got, c.want)
			}
		})
	}
}

func TestDataDirWarningWithoutMountinfo(t *testing.T) {
	old := mountinfo
	mountinfo = filepath.Join(t.TempDir(), "none")
	defer func() { mountinfo = old }()
	if got := DataDirWarning("/data"); got != "" {
		t.Fatalf("with no mount table = %q, want nothing", got)
	}
}

func TestTheClaimScreenSaysWhenTheDataFolderIsInMemory(t *testing.T) {
	dir := t.TempDir()
	withMountinfo(t, "1 0 0:2 / / rw - overlay overlay rw\n2 1 0:3 "+dir+" "+dir+" rw - rootfs rootfs rw\n")
	s := newTestServer(t, Options{ConfigPath: filepath.Join(dir, "config.toml")})
	for _, req := range []*http.Request{
		httptest.NewRequest("GET", "/claim", nil),
		httptest.NewRequest("POST", "/claim", strings.NewReader("token=wrong&password=x")),
	} {
		req.RemoteAddr = "192.168.1.10:5000"
		if req.Method == "POST" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", "http://"+req.Host)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), "/mnt/user/appdata/reostream") {
			t.Fatalf("%s /claim (%d) does not warn about the folder in memory", req.Method, rec.Code)
		}
	}
}
