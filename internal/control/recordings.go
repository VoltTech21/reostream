package control

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/ts"
)

// The Recordings tab: what is on the camera's SD card, read over Baichuan
// (baichuan/sd.go) only when someone opens it. A clip plays by asking the
// camera for that time range and muxing what it sends into MPEG-TS on the
// way through; nothing is stored here.

const recTimeFormat = "2006-01-02T15:04:05"

type recClip struct {
	baichuan.RecordFile
	// Left and Width place the clip on the day's 24 hour bar, in percent.
	Left, Width float64
	// Kind is the clip's main record type, for its color and filter.
	Kind string
	// Saved is a clip the camera uploaded here; Rel is its path under the
	// camera's folder. A saved clip plays from disk without the camera.
	Saved bool
	Rel   string
}

func (c recClip) StartParam() string { return c.Start.Format(recTimeFormat) }
func (c recClip) EndParam() string   { return c.End.Format(recTimeFormat) }
func (c recClip) Seconds() int       { return int(c.End.Sub(c.Start).Seconds()) }

type recPage struct {
	Title string
	Head  camHead
	Cam   string
	Days  []time.Time
	Day   time.Time
	Clips []recClip
	Kinds []string
	Err   string
	// CardSkipped: a battery camera whose saved clips are shown without
	// waking it to read its card; Card asks for the card too.
	CardSkipped bool
	Card        bool
}

// clipKind picks one type for a clip from the camera's list ("people,other").
func clipKind(types string) string {
	for _, k := range []string{"people", "vehicle", "dog_cat", "face", "package", "visitor", "md", "manual", "sched", "io", "other"} {
		for _, t := range strings.Split(types, ",") {
			if strings.TrimSpace(t) == k {
				switch k {
				case "dog_cat":
					return "animal"
				case "md":
					return "motion"
				case "sched", "io", "manual":
					return "other"
				}
				return k
			}
		}
	}
	return "other"
}

// sdLocks keeps one SD conversation per camera at a time: a battery camera
// has one radio, and two downloads at once make both crawl.
var (
	sdLocksMu sync.Mutex
	sdLocks   = map[string]chan struct{}{}
)

// sdSessions keeps one open session per camera for SD work, so moving from
// clip to clip does not log in again each time. A battery camera refuses a
// new session for a few seconds after the last one closes, which is what
// made every other clip fail before this. Only a session whose last job
// finished cleanly is reused: one left mid-download would still be
// receiving the old clip.
var sdSessions = map[string]*sdSession{}

type sdSession struct {
	conn *baichuan.Conn
	last time.Time
}

const sdIdle = 45 * time.Second

// sdConn returns the camera's open session or dials one. The caller holds
// the camera's SD slot (sdAcquire).
func (s *Server) sdConn(ctx context.Context, cam Camera) (*baichuan.Conn, error) {
	sdLocksMu.Lock()
	ss := sdSessions[cam.Name]
	sdLocksMu.Unlock()
	if ss != nil && ss.conn.Err() == nil && time.Since(ss.last) < sdIdle {
		return ss.conn, nil
	}
	if ss != nil {
		ss.conn.Close()
	}
	conn, err := s.dial(ctx, cam)
	if err != nil {
		return nil, err
	}
	sdLocksMu.Lock()
	sdSessions[cam.Name] = &sdSession{conn: conn, last: time.Now()}
	sdLocksMu.Unlock()
	return conn, nil
}

// sdDone hands the session back: kept for the next job when this one ended
// cleanly, closed otherwise. A kept session closes itself once idle.
func sdDone(name string, conn *baichuan.Conn, clean bool) {
	sdLocksMu.Lock()
	defer sdLocksMu.Unlock()
	ss := sdSessions[name]
	if ss == nil || ss.conn != conn {
		conn.Close()
		return
	}
	if !clean {
		conn.Close()
		delete(sdSessions, name)
		return
	}
	ss.last = time.Now()
	time.AfterFunc(sdIdle+time.Second, func() {
		sdLocksMu.Lock()
		defer sdLocksMu.Unlock()
		if cur := sdSessions[name]; cur == ss && time.Since(ss.last) >= sdIdle {
			ss.conn.Close()
			delete(sdSessions, name)
		}
	})
}

// sdAcquire waits up to wait for the camera's SD conversation to be free,
// and returns the release, or false when it stayed busy. Waiting rather than
// refusing matters when someone skips clips: the download they left is
// still winding down for a moment.
func sdAcquire(ctx context.Context, name string, wait time.Duration) (func(), bool) {
	sdLocksMu.Lock()
	ch := sdLocks[name]
	if ch == nil {
		ch = make(chan struct{}, 1)
		sdLocks[name] = ch
	}
	sdLocksMu.Unlock()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	case <-t.C:
	case <-ctx.Done():
	}
	return nil, false
}

func (s *Server) serveRecordings(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	page := recPage{Title: cam.Name + " recordings", Cam: cam.Name}
	page.Head = s.camHeadFor(r.Context(), cam, "recordings", "")

	q := r.URL.Query()
	page.Card = q.Get("card") == "1"
	var saved []savedClip
	if cfg, err := config.Load(s.opts.ConfigPath); err == nil {
		if f, on := ftpSettings(cfg, s.opts.ConfigPath); on {
			for _, c := range savedClips(f.Dir, cam.Name) {
				if !c.Photo {
					saved = append(saved, c)
				}
			}
		}
	}
	savedDays := map[time.Time]bool{}
	for _, c := range saved {
		savedDays[time.Date(c.Start.Year(), c.Start.Month(), c.Start.Day(), 0, 0, 0, 0, time.Local)] = true
	}

	// The card: always on a wired camera with one; on a battery camera only
	// when there is nothing saved, or when asked, since reading it wakes the
	// camera.
	var files []baichuan.RecordFile
	readCard := page.Head.HasSD && (cam.UID == "" || len(saved) == 0 || page.Card)
	page.CardSkipped = page.Head.HasSD && !readCard
	if readCard {
		release, ok := sdAcquire(r.Context(), cam.Name, 30*time.Second)
		if !ok {
			http.Error(w, "the camera is busy", http.StatusServiceUnavailable)
			return
		}
		defer release()
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		// A battery camera's link drops a reply now and then; one retry on
		// a fresh connection covers it.
		for attempt := 0; attempt < 2; attempt++ {
			page.Days, page.Err = nil, ""
			files, err = s.readRecordings(ctx, cam, q.Get("day"), &page)
			if err == nil {
				break
			}
			page.Err = err.Error()
		}
	}
	for d := range savedDays {
		found := false
		for _, x := range page.Days {
			if x.Equal(d) {
				found = true
			}
		}
		if !found {
			page.Days = append(page.Days, d)
		}
	}
	sort.Slice(page.Days, func(i, j int) bool { return page.Days[i].Before(page.Days[j]) })
	if d, err := time.ParseInLocation("2006-01-02", q.Get("day"), time.Local); err == nil {
		page.Day = d
	} else if page.Day.IsZero() && len(page.Days) > 0 {
		page.Day = page.Days[len(page.Days)-1]
	}
	// Saved copies first; a card clip starting within a few seconds of one
	// is the same recording and is left out.
	var dayClips []recClip
	for _, c := range saved {
		if sameDay(c.Start, page.Day) {
			dayClips = append(dayClips, recClip{RecordFile: baichuan.RecordFile{Start: c.Start, End: c.End, Size: c.Size, Type: "saved"}, Saved: true, Rel: c.Rel, Kind: "saved"})
		}
	}
	for _, f := range files {
		dup := false
		for _, c := range dayClips {
			if d := f.Start.Sub(c.Start); d > -6*time.Second && d < 6*time.Second {
				dup = true
			}
		}
		if !dup && sameDay(f.Start, page.Day) {
			dayClips = append(dayClips, recClip{RecordFile: f, Kind: clipKind(f.Type)})
		}
	}
	sort.Slice(dayClips, func(i, j int) bool { return dayClips[i].Start.Before(dayClips[j].Start) })
	midnight := time.Date(page.Day.Year(), page.Day.Month(), page.Day.Day(), 0, 0, 0, 0, page.Day.Location())
	seen := map[string]bool{}
	for _, c := range dayClips {
		c.Left = c.Start.Sub(midnight).Seconds() / 864
		c.Width = c.End.Sub(c.Start).Seconds() / 864
		if c.Width < 0.15 {
			c.Width = 0.15
		}
		if !seen[c.Kind] {
			seen[c.Kind] = true
			page.Kinds = append(page.Kinds, c.Kind)
		}
		page.Clips = append(page.Clips, c)
	}
	sort.Strings(page.Kinds)
	s.render(w, r, "recordings.html", page)
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// readRecordings fills page.Days and page.Day and returns that day's files.
func (s *Server) readRecordings(ctx context.Context, cam Camera, want string, page *recPage) (files []baichuan.RecordFile, err error) {
	conn, err := s.sdConn(ctx, cam)
	if err != nil {
		return nil, fmt.Errorf("could not connect: %w", err)
	}
	defer func() { sdDone(cam.Name, conn, err == nil) }()
	now := time.Now()
	for _, m := range []time.Time{now.AddDate(0, -1, 0), now} {
		days, err := conn.RecordDays(ctx, m)
		if err != nil {
			return nil, err
		}
		page.Days = append(page.Days, days...)
	}
	if d, err := time.ParseInLocation("2006-01-02", want, time.Local); err == nil {
		page.Day = d
	} else if len(page.Days) > 0 {
		page.Day = page.Days[len(page.Days)-1]
	}
	if page.Day.IsZero() {
		return nil, nil
	}
	files, err = conn.RecordFiles(ctx, page.Day)
	return files, err
}

// serveRecordingClip streams one time range off the card as MPEG-TS, to the
// player or, with download=1, as a file.
func (s *Server) serveRecordingClip(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	from, err1 := time.ParseInLocation(recTimeFormat, q.Get("start"), time.Local)
	to, err2 := time.ParseInLocation(recTimeFormat, q.Get("end"), time.Local)
	if err1 != nil || err2 != nil || !to.After(from) || to.Sub(from) > 2*time.Hour {
		http.Error(w, "bad time range", http.StatusBadRequest)
		return
	}
	sub := q.Get("stream") != "main"

	release, ok := sdAcquire(r.Context(), cam.Name, 15*time.Second)
	if !ok {
		http.Error(w, "the camera is busy with another recording; try again in a moment", http.StatusConflict)
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	conn, err := s.sdConn(ctx, cam)
	if err != nil {
		http.Error(w, "could not connect: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	if q.Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.ts"`, cam.Name, from.Format("20060102-150405")))
	}
	flusher, _ := w.(http.Flusher)
	rw := ts.NewRecordingWriter(w, q.Get("audio") != "0")
	frames := 0
	end, err := conn.Download(ctx, from.Add(-2*time.Second), to, sub, 4*time.Second, func(f baichuan.Frame) error {
		if err := rw.Write(f); err != nil {
			return err
		}
		frames++
		if flusher != nil && frames%10 == 0 {
			flusher.Flush()
		}
		return nil
	})
	sdDone(cam.Name, conn, err == nil && end != "")
	log.Printf("control: %s recording %s-%s: %d frames, %s%v", cam.Name, from.Format("15:04:05"), to.Format("15:04:05"), frames, end, err)
	if frames == 0 && err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
}
