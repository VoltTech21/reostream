package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/ftpd"
	"github.com/VoltTech21/reostream/internal/presence"
)

// Saved clips: cameras upload each recording to reostream's own FTP
// receiver (internal/ftpd), so the footage is here before anyone asks for
// it, and looking at it never wakes a battery camera. The receiver runs
// while the config has an [ftp] section; the camera page's "Save clips to
// reostream" button writes that section and points the camera at it.

const (
	ftpDefaultListen  = ":2121"
	ftpDefaultPassive = "2122-2131"
	ftpDefaultKeep    = 14
)

// clipsMinFree is the free space below which clips are refused, so a full
// disk stops saving clips without taking anything else down.
const clipsMinFree = 2 << 30

type clipReceiver struct {
	mu      sync.Mutex
	full    bool
	key     string
	cancel  context.CancelFunc
	janitor bool
}

var receiver clipReceiver

// ftpSettings fills in the defaults of a config's [ftp] section.
func ftpSettings(cfg *config.Config, configPath string) (config.FTPConfig, bool) {
	if cfg == nil || cfg.FTP == nil {
		return config.FTPConfig{}, false
	}
	f := *cfg.FTP
	if f.Listen == "" {
		f.Listen = ftpDefaultListen
	}
	if f.Passive == "" {
		f.Passive = ftpDefaultPassive
	}
	if f.Dir == "" {
		f.Dir = filepath.Join(filepath.Dir(configPath), "clips")
	}
	return f, true
}

func passiveRange(s string) (int, int) {
	a, b, ok := strings.Cut(s, "-")
	lo, err1 := strconv.Atoi(strings.TrimSpace(a))
	hi, err2 := strconv.Atoi(strings.TrimSpace(b))
	if !ok || err1 != nil || err2 != nil || lo <= 0 || hi < lo {
		return 2122, 2131
	}
	return lo, hi
}

// syncReceiver starts, restarts or stops the receiver to match the config.
func (s *Server) syncReceiver() {
	cfg, err := config.Load(s.opts.ConfigPath)
	if err != nil {
		return
	}
	f, on := ftpSettings(cfg, s.opts.ConfigPath)
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	key := ""
	if on && f.Password != "" {
		key = f.Listen + "|" + f.Passive + "|" + f.Host + "|" + f.Dir + "|" + f.Password
	}
	if key == receiver.key {
		return
	}
	if receiver.cancel != nil {
		receiver.cancel()
		receiver.cancel = nil
	}
	receiver.key = ""
	if key == "" {
		return
	}
	lo, hi := passiveRange(f.Passive)
	ctx, cancel := context.WithCancel(context.Background())
	password := f.Password
	_, err = ftpd.Start(ctx, ftpd.Config{
		Listen: f.Listen, Root: f.Dir, PassiveMin: lo, PassiveMax: hi, PublicHost: f.Host,
		Auth: func(user, pass string) (string, bool) {
			if subtle.ConstantTimeCompare([]byte(pass), []byte(password)) != 1 {
				return "", false
			}
			cams, err := loadFleet(s.opts.ConfigPath)
			if err != nil {
				return "", false
			}
			for _, c := range cams {
				if c.Name == user {
					return user, true
				}
			}
			return "", false
		},
		MinFree: clipsMinFree,
		Full: func(free int64) {
			receiver.mu.Lock()
			first := !receiver.full
			receiver.full = true
			receiver.mu.Unlock()
			if first {
				log.Printf("control: clip receiver: disk nearly full (%d MB free), refusing clips until there is room; streams are unaffected", free>>20)
			}
		},
		Stored: func(folder, file string, size int64) {
			receiver.mu.Lock()
			receiver.full = false
			receiver.mu.Unlock()
			log.Printf("control: %s saved %s (%d kB)", folder, filepath.Base(file), size/1024)
			relay.clipArrived(folder)
			if strings.HasSuffix(strings.ToLower(file), ".mp4") {
				if p := presence.Lookup(folder); p != nil {
					p.Play(file)
				}
			}
		},
	})
	if err != nil {
		cancel()
		log.Printf("control: clip receiver: %v", err)
		return
	}
	log.Printf("control: clip receiver listening on %s, saving to %s", f.Listen, f.Dir)
	// Each battery camera's presence stream starts on its newest clip.
	if cams, err := loadFleet(s.opts.ConfigPath); err == nil {
		for _, c := range cams {
			p := presence.Lookup(c.Name)
			if p == nil {
				continue
			}
			clips := savedClips(f.Dir, c.Name)
			for i := len(clips) - 1; i >= 0; i-- {
				if !clips[i].Photo {
					go p.Seed(filepath.Join(f.Dir, c.Name, filepath.FromSlash(clips[i].Rel)))
					break
				}
			}
		}
	}
	receiver.key, receiver.cancel = key, cancel
	if !receiver.janitor {
		receiver.janitor = true
		go s.clipJanitor()
	}
}

// clipJanitor deletes saved clips older than keep_days, hourly; keep_days 0
// keeps them forever.
func (s *Server) clipJanitor() {
	for {
		if cfg, err := config.Load(s.opts.ConfigPath); err == nil {
			if f, on := ftpSettings(cfg, s.opts.ConfigPath); on {
				if f.KeepDays > 0 { // 0 keeps clips forever
					pruneClips(f.Dir, time.Now().AddDate(0, 0, -f.KeepDays))
				}
			}
		}
		select {
		case <-s.done:
			return
		case <-time.After(time.Hour):
		}
	}
}

func pruneClips(dir string, before time.Time) {
	var dirs []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir {
				dirs = append(dirs, p)
			}
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().Before(before) {
			os.Remove(p)
		}
		return nil
	})
	// Deepest first, so an emptied day folder goes before its month.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		os.Remove(d) // only succeeds when empty
	}
}

// savedClip is one uploaded recording.
type savedClip struct {
	Rel   string // path under the camera's folder, slash separated
	Start time.Time
	End   time.Time
	Size  int64
	Photo bool
}

var clipStamp = regexp.MustCompile(`(\d{14})\.(mp4|jpg|jpeg)$`)

// savedClips lists a camera's uploads, newest last. The time comes from
// the camera's own file name (Name_00_20261022152202.mp4), else the file's
// modification time; the length from the MP4's own header.
func savedClips(dir, cam string) []savedClip {
	root := filepath.Join(dir, cam)
	var out []savedClip
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		lower := strings.ToLower(d.Name())
		isVideo := strings.HasSuffix(lower, ".mp4")
		isPhoto := strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg")
		if !isVideo && !isPhoto {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		c := savedClip{Rel: filepath.ToSlash(rel), Size: info.Size(), Photo: isPhoto, Start: info.ModTime()}
		if m := clipStamp.FindStringSubmatch(lower); m != nil {
			if t, err := time.ParseInLocation("20060102150405", m[1], time.Local); err == nil {
				c.Start = t
			}
		}
		c.End = c.Start
		if isVideo {
			if secs := mp4Seconds(p); secs > 0 {
				c.End = c.Start.Add(time.Duration(secs * float64(time.Second)))
			}
		}
		out = append(out, c)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// mp4Seconds reads an MP4's length from its mvhd box, 0 when it cannot.
func mp4Seconds(file string) float64 {
	f, err := os.Open(file)
	if err != nil {
		return 0
	}
	defer f.Close()
	var off int64
	hdr := make([]byte, 16)
	for i := 0; i < 64; i++ {
		if _, err := f.ReadAt(hdr[:8], off); err != nil {
			return 0
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		hl := int64(8)
		if size == 1 {
			if _, err := f.ReadAt(hdr[8:16], off+8); err != nil {
				return 0
			}
			size, hl = int64(binary.BigEndian.Uint64(hdr[8:16])), 16
		}
		if typ == "moov" {
			// mvhd is the first child of moov.
			b := make([]byte, 40)
			if _, err := f.ReadAt(b, off+hl); err != nil || string(b[4:8]) != "mvhd" {
				return 0
			}
			if b[8] == 1 {
				b2 := make([]byte, 44)
				if _, err := f.ReadAt(b2, off+hl+8+4+16); err != nil {
					return 0
				}
				scale := binary.BigEndian.Uint32(b2[0:4])
				dur := binary.BigEndian.Uint64(b2[4:12])
				if scale == 0 {
					return 0
				}
				return float64(dur) / float64(scale)
			}
			scale := binary.BigEndian.Uint32(b[20:24])
			dur := binary.BigEndian.Uint32(b[24:28])
			if scale == 0 {
				return 0
			}
			return float64(dur) / float64(scale)
		}
		if size < hl {
			return 0
		}
		off += size
	}
	return 0
}

// serveSavedClip serves one uploaded file, by its path under the camera's
// folder. ServeFile handles ranges, so the browser can seek.
func (s *Server) serveSavedClip(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	cfg, err := config.Load(s.opts.ConfigPath)
	f, on := ftpSettings(cfg, s.opts.ConfigPath)
	if err != nil || !on {
		http.NotFound(w, r)
		return
	}
	rel := filepath.FromSlash(filepath.Clean("/" + r.URL.Query().Get("f")))
	p := filepath.Join(f.Dir, cam.Name, rel)
	if !strings.HasPrefix(p, filepath.Join(f.Dir, cam.Name)+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s"`, cam.Name, filepath.Base(p)))
	}
	http.ServeFile(w, r, p)
}

func newFTPPassword() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ftpSectionText is the [ftp] section as written into the config file.
func ftpSectionText(f config.FTPConfig) string {
	var b strings.Builder
	b.WriteString("[ftp]\n")
	fmt.Fprintf(&b, "listen = %q\n", f.Listen)
	fmt.Fprintf(&b, "passive = %q\n", f.Passive)
	if f.Host != "" {
		fmt.Fprintf(&b, "host = %q\n", f.Host)
	}
	if f.Dir != "" {
		fmt.Fprintf(&b, "dir = %q\n", f.Dir)
	}
	fmt.Fprintf(&b, "password = %q\n", f.Password)
	fmt.Fprintf(&b, "keep_days = %d\n", f.KeepDays)
	return b.String()
}

// upsertFTPSection replaces the config text's [ftp] section, or adds one
// before the first camera, leaving everything else as written.
func upsertFTPSection(text, section string) string { return upsertSection(text, "ftp", section) }

// upsertSection replaces the config text's [name] section, or adds one
// before the first camera, leaving everything else as written.
func upsertSection(text, name, section string) string {
	lines := strings.Split(text, "\n")
	start, end := -1, len(lines)
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "["+name+"]" {
			start = i
			continue
		}
		if start >= 0 && i > start && strings.HasPrefix(t, "[") {
			end = i
			break
		}
	}
	if start >= 0 {
		// Keep a blank line before the next section.
		for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		return strings.Join(lines[:start], "\n") + "\n" + strings.TrimRight(section, "\n") + "\n" + strings.Join(lines[end:], "\n")
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == "[[camera]]" {
			return strings.Join(lines[:i], "\n") + "\n" + section + "\n" + strings.Join(lines[i:], "\n")
		}
	}
	return strings.TrimRight(text, "\n") + "\n\n" + section
}

// ensureReceiver makes sure the config has an [ftp] section, writing one
// when it does not, and returns its settings. host, when given, is the
// address cameras upload to.
func (s *Server) ensureReceiver(host string, keepDays int) (config.FTPConfig, error) {
	cfg, err := config.Load(s.opts.ConfigPath)
	if err != nil {
		return config.FTPConfig{}, err
	}
	f := config.FTPConfig{}
	if cfg.FTP != nil {
		f = *cfg.FTP
	}
	changed := cfg.FTP == nil
	if f.Listen == "" {
		f.Listen, changed = ftpDefaultListen, true
	}
	if f.Passive == "" {
		f.Passive, changed = ftpDefaultPassive, true
	}
	if f.Password == "" {
		f.Password, changed = newFTPPassword(), true
	}
	if host != "" && host != f.Host {
		f.Host, changed = host, true
	}
	// keepDays < 0 means the form did not say; 0 keeps clips forever.
	switch {
	case keepDays >= 0 && keepDays != f.KeepDays:
		f.KeepDays, changed = keepDays, true
	case cfg.FTP == nil && keepDays < 0:
		f.KeepDays = ftpDefaultKeep
	}
	if changed {
		text, err := loadRawConfig(s.opts.ConfigPath)
		if err != nil {
			return config.FTPConfig{}, err
		}
		if _, err := s.writeAndApply(upsertFTPSection(text, ftpSectionText(f))); err != nil {
			return config.FTPConfig{}, err
		}
	}
	s.syncReceiver()
	full, _ := ftpSettings(&config.Config{FTP: &f}, s.opts.ConfigPath)
	return full, nil
}

// clipsView is the camera page's "Auto-save motion clips" section.
type clipsView struct {
	// HostGuess fills the address box: the configured one, else the
	// address this page was opened on when it is a LAN address (a
	// Tailscale or public one is no use to a camera).
	HostGuess string
	On        bool   // the config has a receiver
	Listen    string // its port
	Host      string
	Dir       string
	KeepDays  int
	Count     int
	Last      time.Time
	Uploading bool // the camera's FTP task is on
	PointedAt bool // the camera's FTP server is this receiver
	CamErr    string
	// LowDisk is set when the clips disk has under clipsMinFree free.
	LowDisk string
}

func (s *Server) readClipsView(ctx context.Context, cam Camera, host string) clipsView {
	v := clipsView{KeepDays: ftpDefaultKeep}
	if cfg, err := config.Load(s.opts.ConfigPath); err == nil {
		if f, on := ftpSettings(cfg, s.opts.ConfigPath); on {
			_, port, _ := net.SplitHostPort(f.Listen)
			v.On, v.Listen, v.Host, v.Dir, v.KeepDays = true, port, f.Host, f.Dir, f.KeepDays
			if free, ok := ftpd.FreeBytes(f.Dir); ok && free < clipsMinFree {
				v.LowDisk = fmt.Sprintf("Disk nearly full (%d MB free): new clips are refused until there is room. Live streams are not affected.", free>>20)
			}
			for _, sc := range savedClips(f.Dir, cam.Name) {
				if !sc.Photo {
					v.Count++
					v.Last = sc.Start
				}
			}
		}
	}
	v.HostGuess = uploadHost(cam, v.Host, host)
	conn, err := s.dial(ctx, cam)
	if err != nil {
		v.CamErr = "could not reach the camera: " + err.Error()
		return v
	}
	defer conn.Close()
	for _, g := range groups() {
		switch g.Title {
		case "FTP upload":
			if _, doc, why := resolveGroupBlock(ctx, conn, g); why == "" {
				val, _ := fieldValue(doc, "FtpTask/enable")
				v.Uploading = val == "1"
			}
		case "FTP server":
			if _, doc, why := resolveGroupBlock(ctx, conn, g); why == "" {
				server, _ := fieldValue(doc, "Ftp/server")
				user, _ := fieldValue(doc, "Ftp/userName")
				want := v.HostGuess
				v.PointedAt = v.On && server == want && user == cam.Name
			}
		}
	}
	return v
}

// serveApplyClips points the camera's FTP upload at this receiver and turns
// it on (or, with stop=1, turns the upload off). Each write is read back.
func (s *Server) serveApplyClips(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	back := "/cameras/" + url.PathEscape(cam.Name) + "/settings/clips"
	say := func(outcome, msg string) {
		s.setFlash(w, r, Flash{Outcome: outcome, Message: cam.Name + ": " + msg})
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	stop := r.FormValue("stop") == "1"

	var f config.FTPConfig
	if !stop {
		keep := -1
		if v := strings.TrimSpace(r.FormValue("keep_days")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				say("refused", "keep for wants a number of days, 0 for forever")
				return
			}
			keep = n
		}
		host := strings.TrimSpace(r.FormValue("host"))
		if host != "" && net.ParseIP(host).To4() == nil {
			say("refused", host+" is not an IPv4 address")
			return
		}
		if f, err = s.ensureReceiver(host, keep); err != nil {
			say("refused", "could not set up the receiver: "+err.Error())
			return
		}
		if host == "" {
			host = uploadHost(cam, f.Host, pageHost(r))
		}
		if host == "" {
			say("refused", "could not work out an address this camera can reach reostream on; enter one under Upload to")
			return
		}
		f.Host = host
	}
	stream := "main"
	if r.FormValue("stream") == "sub" {
		stream = "sub"
	}
	if err := s.pointClips(ctx, cam, f, stream, !stop); err != nil {
		say("refused", err.Error())
		return
	}
	if stop {
		say("confirmed", "auto-save is off")
		return
	}
	say("confirmed", "auto-save is on: every motion clip now saves to reostream")
}

// hasSavedClips reports whether the camera has uploaded anything here.
func (s *Server) hasSavedClips(cam string) bool {
	cfg, err := config.Load(s.opts.ConfigPath)
	f, on := ftpSettings(cfg, s.opts.ConfigPath)
	if err != nil || !on {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(f.Dir, cam))
	return err == nil && len(entries) > 0
}

// uploadHost is the address a camera should upload to: the one typed for
// the receiver if there is one, else this machine's own address on the
// route to that camera, which is the one the camera can reach back. That
// is read from the routing table (a UDP dial sends nothing). Inside a
// Docker bridge network that address is the container's, which a camera
// cannot reach, so then the address the page was opened on is the guess.
func uploadHost(cam Camera, configured, pageHost string) string {
	if configured != "" {
		return configured
	}
	if ip := routeAddr(cam.Address); ip != nil && !dockerBridge.Contains(netip.AddrFrom4([4]byte(ip))) {
		return ip.String()
	}
	if ip := net.ParseIP(pageHost).To4(); ip != nil && ip.IsPrivate() {
		return pageHost
	}
	return ""
}

// dockerBridge is the range Docker gives its own networks by default.
var dockerBridge = netip.MustParsePrefix("172.16.0.0/12")

func routeAddr(camAddr string) net.IP {
	host := camAddr
	if h, _, err := net.SplitHostPort(camAddr); err == nil {
		host = h
	}
	c, err := net.Dial("udp4", net.JoinHostPort(host, "9000"))
	if err != nil {
		return nil
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP.To4()
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return nil
	}
	return ip
}

// pointClips points cam's FTP upload at this receiver and turns it on, or
// with on false turns the upload off. Every write is read back. Turning it
// on also turns off the camera's "Normal" (round the clock) upload, so only
// motion and detections are sent.
func (s *Server) pointClips(ctx context.Context, cam Camera, f config.FTPConfig, stream string, on bool) error {
	conn, err := s.dial(ctx, cam)
	if err != nil {
		return fmt.Errorf("could not connect: %w", err)
	}
	var task, server *Group
	for _, g := range groups() {
		g := g
		switch g.Title {
		case "FTP upload":
			task = &g
		case "FTP server":
			server = &g
		}
	}
	if on {
		pair, doc, why := resolveGroupBlock(ctx, conn, *server)
		if why != "" {
			conn.Close()
			return errors.New("this camera has no FTP upload")
		}
		_, port, _ := net.SplitHostPort(f.Listen)
		streamType := "0" // main
		if stream == "sub" {
			streamType = "1"
		}
		body := doc
		for _, e := range [][2]string{
			{"Ftp/server", f.Host}, {"Ftp/port", port}, {"Ftp/userName", cam.Name},
			{"Ftp/password", f.Password}, {"Ftp/remoteDir", ""}, {"Ftp/autoDir", "1"},
			{"Ftp/onlyFtps", "0"}, {"Ftp/nonymous", "0"}, {"Ftp/streamType", streamType},
		} {
			if _, ok := fieldValue(body, e[0]); !ok {
				continue // older firmware without the element keeps its own
			}
			if body, err = setField(body, e[0], e[1]); err != nil {
				conn.Close()
				return err
			}
		}
		conn.Close()
		res, err := s.writeBlock(ctx, cam, pair.Set, body, true)
		if err != nil || res.Outcome == "refused" {
			return errors.New("the camera did not take the FTP server settings")
		}
		if conn, err = s.dial(ctx, cam); err != nil {
			return fmt.Errorf("could not connect: %w", err)
		}
	}
	pair, doc, why := resolveGroupBlock(ctx, conn, *task)
	conn.Close()
	if why != "" {
		return errors.New("this camera has no FTP upload switch")
	}
	want := "1"
	if !on {
		want = "0"
	}
	body, err := setField(doc, "FtpTask/enable", want)
	if err != nil {
		return err
	}
	if on {
		body = noRoundTheClock(body)
	}
	res, err := s.writeBlock(ctx, cam, pair.Set, body, true)
	if err != nil || res.Outcome == "refused" {
		return errors.New("the camera did not take the upload switch")
	}
	return nil
}

// noRoundTheClock clears the schedule of the FTP task's "Normal" type,
// round-the-clock recording, which would upload every hour of video; the
// motion and detection types are left as the camera has them.
func noRoundTheClock(doc []byte) []byte {
	re := regexp.MustCompile(`(<type>Normal</type>\s*<valueTable>)([01]+)(</valueTable>)`)
	return re.ReplaceAllFunc(doc, func(m []byte) []byte {
		sub := re.FindSubmatch(m)
		return append(append(append([]byte{}, sub[1]...), bytes.Repeat([]byte("0"), len(sub[2]))...), sub[3]...)
	})
}
