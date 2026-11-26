package control

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// mountinfo is read to find the filesystem under the data directory. A var
// so a test can hand it a captured table.
var mountinfo = "/proc/self/mountinfo"

// memoryFS are filesystems that live in RAM. Unraid's own root is rootfs, so
// a Data path outside /mnt/user and the pools, which Docker creates as an
// empty root-owned folder when it is missing, shows up inside the container
// as rootfs and is emptied at every reboot (#46).
var memoryFS = map[string]bool{"rootfs": true, "tmpfs": true, "ramfs": true}

// dataDirFS is the filesystem type of the mount holding dir, or "" when
// mountinfo cannot be read.
func dataDirFS(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	f, err := os.Open(mountinfo)
	if err != nil {
		return ""
	}
	defer f.Close()
	best, fstype := -1, ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		// id parent major:minor root mountpoint options [optional...] - fstype source superopts
		line := sc.Text()
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields, after := strings.Fields(pre), strings.Fields(post)
		if len(fields) < 5 || len(after) < 1 {
			continue
		}
		mp := unescapeMount(fields[4])
		if (dir == mp || mp == "/" || strings.HasPrefix(dir, mp+"/")) && len(mp) >= best {
			best, fstype = len(mp), after[0]
		}
	}
	return fstype
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// DataDirWarning is a warning for a data directory that lives in memory, or
// "" for one that does not.
func DataDirWarning(dir string) string {
	fs := dataDirFS(dir)
	if !memoryFS[fs] {
		return ""
	}
	msg := fmt.Sprintf("The folder at %s is in memory (%s), so it is emptied every time the host restarts and reostream starts over, unclaimed.", dir, fs)
	if fs == "rootfs" {
		msg += " On Unraid, set Data to a folder under /mnt/user, such as /mnt/user/appdata/reostream."
	} else {
		msg += " Start reostream with -data pointing at a folder on a disk."
	}
	return msg
}
