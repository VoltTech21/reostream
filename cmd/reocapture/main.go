// Command reocapture records what a Reolink app asks a camera for, so a
// feature reostream does not support yet can be added for a camera nobody
// here owns (privacy mode on the E1 series, a doorbell's talk button).
//
// It sits between the Reolink desktop client (or the phone app, on the same
// network) and the camera: add the camera in the client by this computer's
// address instead of the camera's, use the feature, then send the log. It
// forwards every byte unchanged, decrypts the camera's protocol with the
// camera password, and writes the requests and replies as text. Video and
// audio are counted, never saved, and logins, UIDs, serial numbers, MAC
// addresses, Wi-Fi names and passwords are blanked out of the log.
//
// It is a separate test build, not part of reostream: run it only while
// capturing, then delete it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

func main() {
	camera := flag.String("camera", "", "the camera's IP address (required)")
	listen := flag.String("listen", "0.0.0.0:9000", "address the Reolink client connects to")
	pass := flag.String("password", "", "the camera's password (or set CAM_PW)")
	out := flag.String("out", "", "log file (default reocapture-<time>.txt)")
	flag.Parse()
	if *pass == "" {
		*pass = os.Getenv("CAM_PW")
	}
	if *camera == "" || *pass == "" {
		fmt.Fprintln(os.Stderr, "usage: reocapture -camera 192.168.1.50 -password <camera password>")
		os.Exit(2)
	}
	if *out == "" {
		*out = "reocapture-" + time.Now().Format("20060102-150405") + ".txt"
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	lg := &logger{w: f, start: time.Now()}
	lg.printf("reocapture: camera %s", redact(*camera))

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Listening on %s. In the Reolink client, add the camera by this computer's IP address, port %s.\n", *listen, portOf(*listen))
	fmt.Printf("Use the feature (for example turn Privacy Mode on, wait 20 seconds, turn it off), then press Ctrl+C.\n")
	fmt.Printf("Writing %s. Send that file; it has no passwords, UIDs or serial numbers in it.\n", *out)
	for n := 1; ; n++ {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go relay(lg, n, c, net.JoinHostPort(*camera, "9000"), *pass)
	}
}

func portOf(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return "9000"
}

type logger struct {
	mu    sync.Mutex
	w     io.Writer
	start time.Time
}

func (l *logger) printf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "%8.3f %s\n", time.Since(l.start).Seconds(), fmt.Sprintf(format, a...))
}

// session is one client connection's decoding state, shared by its two
// directions: the camera's nonce decides the AES key both use.
type session struct {
	mu    sync.Mutex
	nonce string
	pass  string
}

func (s *session) key() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nonce == "" {
		return nil
	}
	return baichuan.AESKey(s.nonce, s.pass)
}

func relay(lg *logger, n int, client net.Conn, camAddr, pass string) {
	defer client.Close()
	cam, err := net.DialTimeout("tcp", camAddr, 10*time.Second)
	if err != nil {
		lg.printf("conn %d: cannot reach the camera: %v", n, err)
		return
	}
	defer cam.Close()
	lg.printf("conn %d: opened", n)
	s := &session{pass: pass}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pipe(lg, n, "APP>CAM", cam, client, s) }()
	go func() { defer wg.Done(); pipe(lg, n, "CAM>APP", client, cam, s) }()
	wg.Wait()
	lg.printf("conn %d: closed", n)
}

// pipe copies src to dst unchanged and decodes a copy of the bytes.
func pipe(lg *logger, n int, dir string, dst io.WriteCloser, src io.Reader, s *session) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		decode(lg, n, dir, pr, s)
		_, _ = io.Copy(io.Discard, pr)
	}()
	_, _ = io.Copy(io.MultiWriter(dst, pw), src)
	pw.Close()
	dst.Close()
	<-done
}

type mediaCount struct {
	msgs, bytes int
	info        string
}

func decode(lg *logger, n int, dir string, r io.Reader, s *session) {
	rd := baichuan.NewReader(r)
	logins := 0
	media := map[uint32]*mediaCount{}
	deps := map[uint32]*baichuan.Depacketiser{}
	defer func() {
		for enc, m := range media {
			lg.printf("conn %d %s media enc=%08x: %d messages, %d bytes, %s", n, dir, enc, m.msgs, m.bytes, m.info)
		}
	}()
	for {
		m, err := rd.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				lg.printf("conn %d %s: stopped decoding: %v", n, dir, err)
			}
			return
		}
		h := m.Header
		if h.MsgID == baichuan.MsgIDLogin {
			logins++
			if dir == "CAM>APP" && logins == 1 {
				if nonce := nonceRe.FindSubmatch(m.XML); nonce != nil {
					s.mu.Lock()
					s.nonce = string(nonce[1])
					s.mu.Unlock()
				}
			}
			// Everything after the second login message is AES.
			if logins == 2 {
				if k := s.key(); k != nil {
					rd.SetAESKey(k)
				}
			}
			body := "(login, not logged)"
			if dir == "CAM>APP" && logins == 2 {
				body = clean(m.XML) // the device info: model and firmware
			}
			lg.printf("conn %d %s msg 1 (login) status=%d %s", n, dir, h.Status(), body)
			continue
		}
		if h.MsgID == baichuan.MsgIDVideo && dir == "CAM>APP" && len(m.Payload) > 0 {
			mc := media[h.EncOffset]
			if mc == nil {
				mc = &mediaCount{}
				media[h.EncOffset] = mc
				deps[h.EncOffset] = baichuan.NewDepacketiser()
				lg.printf("conn %d %s msg 3 media starts enc=%08x status=%d", n, dir, h.EncOffset, h.Status())
			}
			mc.msgs++
			mc.bytes += len(m.Payload)
			d := deps[h.EncOffset]
			d.Write(m.Payload, m.StartsPacket)
			for {
				f, ok := d.Next()
				if !ok {
					break
				}
				if f.Kind == baichuan.FrameInfo && mc.info == "" {
					mc.info = fmt.Sprintf("%dx%d@%d", f.Width, f.Height, f.FPS)
				}
			}
			continue
		}
		if dir == "APP>CAM" && h.MsgID == baichuan.MsgIDTalk && len(m.Payload) > 0 && len(m.XML) == 0 {
			continue // talk audio
		}
		doc := m.XML
		if len(m.PayloadXML) > 0 && strings.Contains(string(m.PayloadXML), "<") {
			doc = append(append([]byte(nil), doc...), m.PayloadXML...)
		}
		lg.printf("conn %d %s msg %d (%s) enc=%08x status=%d %s", n, dir, h.MsgID, baichuan.MsgName(h.MsgID), h.EncOffset, h.Status(), clean(doc))
	}
}

var (
	nonceRe  = regexp.MustCompile(`<nonce>([^<]+)</nonce>`)
	secretRe = regexp.MustCompile(`(?is)<(uid|serialNumber|mac|password|passwd|userName|user|token|ssid|key|nonce|p2pUid|email|phone|wifiPassword)>[^<]*</`)
	ipRe     = regexp.MustCompile(`\b(\d{1,3}\.){3}\d{1,3}\b`)
)

// clean turns a decrypted document into one log line with the secrets
// blanked.
func clean(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	s = strings.TrimPrefix(s, `<?xml version="1.0" encoding="UTF-8" ?>`)
	s = secretRe.ReplaceAllString(s, "<$1>(removed)</")
	s = redact(s)
	if strings.TrimSpace(s) == "" {
		return "(empty)"
	}
	return strings.TrimSpace(s)
}

func redact(s string) string { return ipRe.ReplaceAllString(s, "x.x.x.x") }
