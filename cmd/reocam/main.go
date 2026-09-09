// Command reocam controls a camera over Baichuan.
//
// It is deliberately separate from the reostream daemon. The daemon's value
// is a small surface and a short list of non-goals; this is where everything
// a camera can be asked to do lives instead.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/adpcm"
	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
)

func usage() {
	fmt.Fprintln(os.Stderr, `reocam controls a Reolink camera over Baichuan.

  reocam -address CAM [-password PW] <command> [args]

Commands:
  heartbeat [N]          send N heartbeats and report the round trip
  support                what hardware this camera actually has
  abilities              what the logged in user may read and write
  get NAME               print one configuration block as the camera sends it
  get all                try every known block and report which the camera has
  snap [main|sub] FILE   save a still image
  talk FILE              play raw 16 bit mono PCM through the camera speaker
  talkinfo               report the two-way audio formats the camera accepts
  fisheye                show the fisheye view mode
  fisheye set N          change it (0 raw, 1 panorama, 2 quad, 3 dual) REBOOTS THE CAMERA
  stitch                 show the dual lens alignment, with its range
  stitch set K=V ...     adjust it (distance, x, y); does not reboot`)
}

func main() {
	addr := flag.String("address", "", "camera address")
	user := flag.String("username", "admin", "username")
	pass := flag.String("password", "", "password")
	withVideo := flag.String("with-video", "", "open this video stream first: main, sub or extern")
	noConfig := flag.Bool("no-config", false, "skip TalkConfig and send audio into an existing session")
	hb2 := flag.Bool("hb-twopart", false, "send the heartbeat as an extension plus a body")
	flag.Usage = usage
	flag.Parse()

	if *addr == "" || flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}

	// Long enough for a full sweep, which asks nearly thirty questions and
	// waits out a timeout for each one a camera ignores.
	baichuan.SetHeartBeatTwoPart(*hb2)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Some messages make a camera hang up rather than answer, so a sweep has
	// to be able to start a fresh connection partway through.
	dial := func() (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, *addr, baichuan.Options{Username: *user, Password: *pass})
	}
	conn, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reocam:", err)
		os.Exit(1)
	}
	defer conn.Close()

	switch flag.Arg(0) {
	case "heartbeat":
		err = heartbeat(conn, flag.Args()[1:], *withVideo)
	case "support":
		err = support(conn)
	case "abilities":
		err = abilities(conn)
	case "get":
		err = get(conn, dial, flag.Args()[1:])
	case "set":
		err = set(conn, flag.Args()[1:])
	case "snap":
		err = snap(conn, flag.Args()[1:])
	case "talkinfo":
		_, err = talkFormat(conn, true)
	case "fisheye":
		err = fisheye(*addr, *user, *pass, flag.Args()[1:])
	case "stitch":
		err = stitch(*addr, *user, *pass, flag.Args()[1:])
	case "floodlight":
		err = floodlight(*addr, *user, *pass, flag.Args()[1:])
	case "probe":
		err = probe(conn, dial)
	case "verify":
		err = verify(conn, dial, flag.Args()[1:])
	case "talk":
		err = talk(conn, flag.Args()[1:], *withVideo, *noConfig)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "reocam:", err)
		os.Exit(1)
	}
}

func snap(conn *baichuan.Conn, args []string) error {
	stream, out := "main", ""
	switch len(args) {
	case 1:
		out = args[0]
	case 2:
		stream, out = args[0], args[1]
	default:
		return fmt.Errorf("usage: snap [main|sub] FILE")
	}

	if err := conn.Snap(stream); err != nil {
		return err
	}

	var r baichuan.SnapReader
	deadline := time.After(20 * time.Second)
	for !r.Done() {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				return fmt.Errorf("connection closed after %d of %d bytes", len(r.Image()), r.Size())
			}
			if m.Header.MsgID == baichuan.MsgIDSnap && m.Header.Status() != 0 && m.Header.Status() != 200 {
				return fmt.Errorf("camera refused the snap with status %d", m.Header.Status())
			}
			if err := r.Read(m); err != nil {
				return err
			}
		case <-deadline:
			return fmt.Errorf("timed out waiting for the image")
		}
	}

	img := r.Image()
	if err := os.WriteFile(out, img, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %d bytes from %s\n", out, len(img), r.Name())
	return nil
}

// talkFormat asks the camera what two-way audio it accepts.
func talkFormat(conn *baichuan.Conn, print bool) (baichuan.TalkFormat, error) {
	if err := conn.TalkAbility(); err != nil {
		return baichuan.TalkFormat{}, err
	}
	deadline := time.After(8 * time.Second)
	for {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				return baichuan.TalkFormat{}, fmt.Errorf("connection closed before a TalkAbility reply")
			}
			if m.Header.MsgID != baichuan.MsgIDTalkAbility || len(m.XML) == 0 {
				continue
			}
			f, err := baichuan.ParseTalkAbility(m.XML)
			if err != nil {
				return f, err
			}
			if print {
				fmt.Printf("%s %d Hz %d bit %s, %d samples per block, %s\n",
					f.AudioType, f.SampleRate, f.SamplePrecision, f.SoundTrack,
					f.LengthPerEncoder, f.Duplex)
			}
			return f, nil
		case <-deadline:
			return baichuan.TalkFormat{}, fmt.Errorf("no TalkAbility reply")
		}
	}
}

// talk plays a file through the camera's speaker.
//
// Nothing in the protocol confirms a camera actually made a sound: it
// acknowledges the session, accepts every packet, and reports no error either
// way. Two separate bugs here passed all of that while playing silence. To
// check, listen to the camera's own audio stream, and to a neighbour's.
//
// A camera with no speaker is worse than useless here rather than harmlessly
// inert: it answers TalkAbility with a full format, accepts one TalkConfig
// with status 200, and then refuses every later one with 422 until it
// reboots. GetAbility's `talk` does not distinguish them, since it reports
// present on models that plainly cannot speak. The audio output capabilities
// do: alarmAudio, customAudio, supportAudioPlay.
func talk(conn *baichuan.Conn, args []string, withVideo string, noConfig bool) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: talk FILE  (raw signed 16 bit little endian mono PCM)")
	}
	format, err := talkFormat(conn, false)
	if err != nil {
		return err
	}
	if format.AudioType != "adpcm" {
		return fmt.Errorf("this camera wants %q, which is not implemented", format.AudioType)
	}
	// Refuse before opening a session rather than after. A camera with no
	// speaker accepts the configuration, answers 200, plays nothing, and then
	// refuses every later session until it reboots, so getting this wrong
	// costs the feature rather than just the attempt.
	if !noConfig {
		sup, err := fetchSupport(conn)
		if err != nil {
			return fmt.Errorf("could not check whether this camera has a speaker: %w", err)
		}
		if !sup.CanTalk() {
			return fmt.Errorf("this camera has no speaker (Support reports audioTalk 0)")
		}
	}
	if withVideo != "" {
		kind := map[string]string{"main": baichuan.StreamMain, "sub": baichuan.StreamSub,
			"extern": baichuan.StreamExtern}[withVideo]
		if err := conn.StartVideo(kind); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
	}
	if !noConfig {
		if err := conn.TalkConfig(format); err != nil {
			return err
		}
		if err := waitForAck(conn, baichuan.MsgIDTalkConfig); err != nil {
			return err
		}
	}

	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}

	// Real time pacing: the camera plays what arrives, so sending the whole
	// file at once would overrun whatever it buffers.
	var enc adpcm.Encoder
	per := time.Duration(float64(adpcm.SamplesPerBlock) / float64(format.SampleRate) * float64(time.Second))
	next := time.Now()
	for off := 0; off < len(pcm); off += adpcm.SamplesPerBlock {
		end := off + adpcm.SamplesPerBlock
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := conn.Talk(baichuan.TalkPacket(enc.EncodeBlock(pcm[off:end]))); err != nil {
			return err
		}
		next = next.Add(per)
		time.Sleep(time.Until(next))
	}
	fmt.Printf("played %.1f seconds\n", float64(len(pcm))/float64(format.SampleRate))
	return nil
}

// waitForAck checks the status a camera returns for a request. A camera that
// dislikes a request says so here and nowhere else.
func waitForAck(conn *baichuan.Conn, msgID uint32) error {
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				return fmt.Errorf("connection closed before a reply to message %d", msgID)
			}
			if m.Header.MsgID != msgID {
				continue
			}
			if m.Header.Status() != 200 {
				return fmt.Errorf("camera refused message %d with status %d", msgID, m.Header.Status())
			}
			return nil
		case <-deadline:
			return fmt.Errorf("no reply to message %d", msgID)
		}
	}
}

// abilities prints what the camera says it can do.
func abilities(conn *baichuan.Conn) error {
	if err := conn.Abilities(); err != nil {
		return err
	}
	deadline := time.After(8 * time.Second)
	for {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				return fmt.Errorf("connection closed before an ability reply")
			}
			if m.Header.MsgID != baichuan.MsgIDAbilityInfo || len(m.XML) == 0 {
				continue
			}
			list, err := baichuan.ParseAbilities(m.XML)
			if err != nil {
				return err
			}
			module := ""
			for _, a := range list {
				if a.Module != module {
					module = a.Module
					fmt.Printf("\n%s\n", module)
				}
				access := "read"
				if a.Writable {
					access = "read/write"
				}
				fmt.Printf("  %-18s %s\n", a.Name, access)
			}
			fmt.Printf("\n%d abilities\n", len(list))
			return nil
		case <-deadline:
			return fmt.Errorf("no ability reply")
		}
	}
}

// get prints a configuration block, or sweeps every known one.
//
// A camera answers 405 for a message it does not implement, so the sweep is
// also the cheapest survey of what a model supports, and unlike the ability
// lists it reports what the camera will actually do rather than what it
// claims.
func get(conn *baichuan.Conn, dial func() (*baichuan.Conn, error), args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: get NAME|all\n  names: %s", strings.Join(baichuan.ConfigNames(), " "))
	}
	if args[0] != "all" {
		id, ok := baichuan.ConfigMessages[args[0]]
		if !ok {
			return fmt.Errorf("unknown block %q\n  names: %s", args[0], strings.Join(baichuan.ConfigNames(), " "))
		}
		xml, status, err := fetch(conn, id)
		if err != nil {
			return err
		}
		if status != 200 && status != 0 {
			return fmt.Errorf("camera answered status %d", status)
		}
		fmt.Printf("%s\n", xml)
		return nil
	}

	var hangups []string
	for _, name := range baichuan.ConfigNames() {
		id := baichuan.ConfigMessages[name]
		xml, status, err := fetch(conn, id)
		switch {
		case err != nil:
			// A camera that hangs up on a request has still told us
			// something, and the rest of the sweep is worth having, so
			// reconnect and carry on rather than stopping here.
			fmt.Printf("%-14s %-4d %v, reconnecting\n", name, id, err)
			hangups = append(hangups, name)
			conn.Close()
			if conn, err = dial(); err != nil {
				return fmt.Errorf("could not reconnect after %s: %w", name, err)
			}
		case len(xml) > 0:
			fmt.Printf("%-14s %-4d supported, %d bytes\n", name, id, len(xml))
		default:
			fmt.Printf("%-14s %-4d status %d\n", name, id, status)
		}
	}
	if len(hangups) > 0 {
		fmt.Printf("\nthe camera hung up on: %s\n", strings.Join(hangups, " "))
	}
	return nil
}

// fetch sends one config request and waits for the matching reply.
func fetch(conn *baichuan.Conn, id uint32) (xml []byte, status int16, err error) {
	if err := conn.GetConfig(id); err != nil {
		return nil, 0, err
	}
	deadline := time.After(4 * time.Second)
	for {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				return nil, 0, fmt.Errorf("connection closed")
			}
			// A ping reply, or anything else in flight, is not the answer.
			if m.Header.MsgID != id {
				continue
			}
			return m.XML, m.Header.Status(), nil
		case <-deadline:
			return nil, 0, fmt.Errorf("no reply")
		}
	}
}

// fetchSupport reads the camera's hardware description.
func fetchSupport(conn *baichuan.Conn) (baichuan.Support, error) {
	x, status, err := fetch(conn, baichuan.ConfigMessages["support"])
	if err != nil {
		return baichuan.Support{}, err
	}
	if len(x) == 0 {
		return baichuan.Support{}, fmt.Errorf("camera answered status %d", status)
	}
	return baichuan.ParseSupport(x)
}

// support prints what the camera says its hardware is.
func support(conn *baichuan.Conn) error {
	s, err := fetchSupport(conn)
	if err != nil {
		return err
	}
	v := s.Support
	yes := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	fmt.Printf("channels          %d\n", v.ChannelNum)
	fmt.Printf("speaker (talk)    %s\n", yes(s.CanTalk()))
	fmt.Printf("audio alarm       %s\n", yes(v.AudioAlarm != 0))
	fmt.Printf("balanced stream   %s\n", yes(s.HasExternStream()))
	fmt.Printf("ptz               %s (mode %q)\n", yes(s.HasPTZ()), v.PTZMode)
	fmt.Printf("wifi              %s\n", yes(v.WiFi != 0))
	fmt.Printf("gps               %s\n", yes(v.GPS != 0))
	fmt.Printf("power saving      %s\n", yes(v.PowerSavingCfg != 0))
	fmt.Printf("rs485             %s\n", yes(v.B485 != 0))
	fmt.Printf("rf alarm          %s\n", yes(v.RFVersion != 0))
	fmt.Printf("disks             %d\n", v.DiskNum)
	fmt.Printf("alarm in/out      %d/%d\n", v.IOInputPortNum, v.IOOutputPortNum)

	ch, ok := s.Channel(0)
	if !ok {
		return nil
	}
	fmt.Printf("\nchannel 0\n")
	// Several of these are bitmasks or versions rather than flags, so the
	// value is printed as well as whether it is set.
	for _, f := range []struct {
		name string
		v    int
	}{
		{"fisheye modes", ch.FishEye}, {"dual lens stitch", ch.BinoCfg},
		{"battery", ch.Battery}, {"battery analysis", ch.BatAnalysis},
		{"ptz control", ch.PTZControl}, {"ptz preset", ch.PTZPreset},
		{"ptz patrol", ch.PTZPatrol}, {"ptz pattern", ch.PTZTattern},
		{"auto pan/tilt", ch.AutoPT}, {"auto focus", ch.AutoFocus},
		{"zoom/focus backlash", ch.ZFBacklash}, {"led control", ch.LEDCtrl},
		{"isp", ch.ISPCfg}, {"isp (new)", ch.NewISPCfg}, {"osd", ch.OSDCfg},
		{"encoder control", ch.EncCtrl}, {"motion", ch.Motion},
		{"ai types", ch.AIType}, {"snapshot", ch.Snap}, {"video clip", ch.VideoClip},
		{"timelapse", ch.Timelapse}, {"thumbnail", ch.Thumbnail},
		{"rf alarm", ch.RFCfg}, {"audio version", ch.AudioVer},
	} {
		mark := " "
		if f.v != 0 {
			mark = "*"
		}
		fmt.Printf("  %s %-20s %d\n", mark, f.name, f.v)
	}
	return nil
}

// fisheye and stitch go over the camera's HTTP API rather than Baichuan.
// Both settings exist over Baichuan too, since the official NVR writes them,
// but their message ids have not been recovered.
func fisheye(addr, user, pass string, args []string) error {
	c, err := cgi.Dial(addr, user, pass)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		f, err := c.GetFishEye(0)
		if err != nil {
			return err
		}
		fmt.Printf("imageType     %d  %s\n", int(f.ImageType), f.ImageType)
		fmt.Printf("installType   %d\n", f.InstallType)
		fmt.Printf("rotationAngle %d\n", f.RotationAngle)
		return nil
	}
	if len(args) != 2 || args[0] != "set" {
		return fmt.Errorf("usage: fisheye [set N]")
	}
	mode, err := strconv.Atoi(args[1])
	if err != nil || mode < 0 || mode > 3 {
		return fmt.Errorf("mode must be 0, 1, 2 or 3")
	}

	f, err := c.GetFishEye(0)
	if err != nil {
		return err
	}
	f.ImageType = cgi.FishEyeMode(mode)
	if err := c.SetFishEye(0, f); err != nil {
		return err
	}
	fmt.Printf("set to %s; the camera is rebooting, which takes 15 to 20 seconds\n", f.ImageType)
	fmt.Println("any motion mask or detection zone drawn on the old geometry now points somewhere else")
	return nil
}

func stitch(addr, user, pass string, args []string) error {
	c, err := cgi.Dial(addr, user, pass)
	if err != nil {
		return err
	}
	cur, factory, lim, err := c.GetStitch(0)
	if err != nil {
		return err
	}

	if len(args) == 0 {
		fmt.Printf("%-9s %-8s %-8s %s\n", "field", "current", "factory", "range")
		fmt.Printf("%-9s %-8.1f %-8.1f %.1f to %.1f\n", "distance", cur.Distance, factory.Distance,
			lim.Distance.Min, lim.Distance.Max)
		fmt.Printf("%-9s %-8d %-8d %d to %d\n", "x", cur.XMove, factory.XMove, lim.XMove.Min, lim.XMove.Max)
		fmt.Printf("%-9s %-8d %-8d %d to %d\n", "y", cur.YMove, factory.YMove, lim.YMove.Min, lim.YMove.Max)
		fmt.Println("\ndistance is the depth the seam is made to line up at, not a third offset:")
		fmt.Println("x and y correct lens mounting, distance corrects parallax and only at one depth")
		return nil
	}
	if args[0] != "set" || len(args) < 2 {
		return fmt.Errorf("usage: stitch [set distance=N x=N y=N]")
	}

	want := cur
	for _, kv := range args[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("expected key=value, got %q", kv)
		}
		switch k {
		case "distance":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("distance: %w", err)
			}
			if f < lim.Distance.Min || f > lim.Distance.Max {
				return fmt.Errorf("distance %.1f is outside %.1f to %.1f", f, lim.Distance.Min, lim.Distance.Max)
			}
			want.Distance = f
		case "x", "y":
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			min, max := lim.XMove.Min, lim.XMove.Max
			if k == "y" {
				min, max = lim.YMove.Min, lim.YMove.Max
			}
			if n < min || n > max {
				return fmt.Errorf("%s %d is outside %d to %d", k, n, min, max)
			}
			if k == "x" {
				want.XMove = n
			} else {
				want.YMove = n
			}
		default:
			return fmt.Errorf("unknown field %q, want distance, x or y", k)
		}
	}

	if err := c.SetStitch(0, want); err != nil {
		return err
	}
	fmt.Printf("distance %.1f, x %d, y %d\n", want.Distance, want.XMove, want.YMove)
	return nil
}

// heartbeat measures the round trip and the camera's clock offset.
func heartbeat(conn *baichuan.Conn, args []string, withVideo string) error {
	if withVideo != "" {
		kind := map[string]string{"main": baichuan.StreamMain, "sub": baichuan.StreamSub,
			"extern": baichuan.StreamExtern}[withVideo]
		if err := conn.StartVideo(kind); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
	}
	n := 3
	if len(args) == 1 {
		var err error
		if n, err = strconv.Atoi(args[0]); err != nil || n < 1 {
			return fmt.Errorf("count must be a positive number")
		}
	}
	for i := 0; i < n; i++ {
		sent := time.Now()
		if err := conn.HeartBeat(); err != nil {
			return err
		}
		x, status, err := await(conn, baichuan.MsgIDHeartBeat)
		if err != nil {
			return err
		}
		rtt := time.Since(sent)
		if len(x) == 0 {
			return fmt.Errorf("camera answered status %d with no body; message 5 may not be the heartbeat on this model", status)
		}
		hb, err := baichuan.ParseHeartBeat(x)
		if err != nil {
			return err
		}
		fmt.Printf("rtt %-8s camera clock %s  offset %s  overlap %d  delay %d\n",
			rtt.Round(time.Microsecond), hb.CameraTime().Format("15:04:05.000"),
			time.Since(hb.CameraTime()).Round(time.Millisecond), hb.OverlapCount, hb.Delay)
		if i+1 < n {
			time.Sleep(time.Second)
		}
	}
	return nil
}

// await waits for the reply to a message id.
func await(conn *baichuan.Conn, id uint32) ([]byte, int16, error) {
	deadline := time.After(6 * time.Second)
	for {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				return nil, 0, fmt.Errorf("connection closed")
			}
			if m.Header.MsgID != id {
				continue
			}
			return m.XML, m.Header.Status(), nil
		case <-deadline:
			return nil, 0, fmt.Errorf("no reply to message %d", id)
		}
	}
}

// set writes a configuration message. The body comes in on stdin, and the
// only body that is ever right is what the matching get returned with one
// field changed.
//
// The reply status is not proof. A camera answers 200 to a write it then
// ignores, so anything using this has to go and look at the effect.
func set(conn *baichuan.Conn, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: set ID < body.xml")
	}
	id, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		return fmt.Errorf("message id: %w", err)
	}
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading body: %w", err)
	}
	if len(body) == 0 {
		return fmt.Errorf("empty body on stdin")
	}
	if err := conn.SetConfig(uint32(id), body); err != nil {
		return err
	}
	// Drain everything the camera says for a few seconds rather than
	// waiting on this id alone. A write may well be confirmed on another
	// message entirely, and filtering to the id would throw that away
	// without ever showing it. 580 is "cfg modify report".
	deadline := time.After(6 * time.Second)
	seen := 0
	for {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				fmt.Fprintln(os.Stderr, "connection closed")
				return nil
			}
			seen++
			fmt.Fprintf(os.Stderr, "  <- msg %d (%s) status %d, %d xml bytes\n",
				m.Header.MsgID, baichuan.MsgName(m.Header.MsgID),
				m.Header.Status(), len(m.XML))
			if len(m.XML) > 0 {
				fmt.Printf("%s\n", m.XML)
			}
		case <-deadline:
			if seen == 0 {
				fmt.Fprintln(os.Stderr, "  <- nothing at all")
			}
			return nil
		}
	}
}

// floodlight reads or drives the white light over the camera's HTTP API.
//
// The Baichuan path (288, FloodlightManual) answers 200 for any well formed
// body and does nothing observable, so this is the one shown to work.
//
// mode and state are separate: mode is what the light does, state is whether
// it is lit right now. Mapped against the Baichuan FloodlightTask on a live
// camera, mode 0 reads back as alarmMode 0 and is off, mode 1 and 2 both read
// as alarmMode 1 and light on detection, and mode 3 reads as alarmMode 3 and
// runs on the schedule. So "on" is mode 1 with state 1, and "motion" is the
// same mode with state 0, which is how these cameras ship.
func floodlight(addr, user, pass string, args []string) error {
	c, err := cgi.Dial(addr, user, pass)
	if err != nil {
		return err
	}
	value, _, rng, err := c.Get("GetWhiteLed", 0)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Printf("current %s\n", value)
		fmt.Printf("range   %s\n", rng)
		return nil
	}

	var cur struct {
		WhiteLed map[string]any `json:"WhiteLed"`
	}
	if err := json.Unmarshal(value, &cur); err != nil {
		return fmt.Errorf("parsing current state: %w", err)
	}
	if cur.WhiteLed == nil {
		return fmt.Errorf("camera returned no WhiteLed block")
	}
	// Change only what was asked for, and send the camera's own document
	// back, so fields this build has never heard of survive the write.
	switch args[0] {
	case "on":
		cur.WhiteLed["state"] = 1
		cur.WhiteLed["mode"] = 1
	case "off":
		cur.WhiteLed["state"] = 0
		cur.WhiteLed["mode"] = 0
	case "motion":
		// Dark now, lighting when the camera detects something. mode is
		// what the light does, state is whether it is lit right now, so
		// "on" and "motion" are the same mode with a different state.
		cur.WhiteLed["mode"] = 1
		cur.WhiteLed["state"] = 0
	case "auto":
		cur.WhiteLed["mode"] = 2
		cur.WhiteLed["state"] = 0
	case "schedule":
		cur.WhiteLed["mode"] = 3
		cur.WhiteLed["state"] = 0
	default:
		// Numeric mode, for pinning down what a firmware means by each.
		m, err := strconv.Atoi(args[0])
		if err != nil || m < 0 || m > 3 {
			return fmt.Errorf("usage: floodlight [on|off|motion|auto|schedule|0-3] [brightness]")
		}
		cur.WhiteLed["mode"] = m
		cur.WhiteLed["state"] = 0
	}
	cur.WhiteLed["channel"] = 0
	if len(args) == 2 {
		b, err := strconv.Atoi(args[1])
		if err != nil || b < 0 || b > 100 {
			return fmt.Errorf("brightness must be 0..100")
		}
		cur.WhiteLed["bright"] = b
	}
	if err := c.Set("SetWhiteLed", map[string]any{"WhiteLed": cur.WhiteLed}); err != nil {
		return err
	}
	after, _, _, err := c.Get("GetWhiteLed", 0)
	if err != nil {
		return err
	}
	fmt.Printf("now %s\n", after)
	return nil
}

// probe reports what this camera implements.
//
// It exists because no table here can say what a given model does. The
// recovered message ids come from an NVR and a hub, and the set any one
// camera answers is its own. So rather than assume, ask: every read in
// ConfigMessages is safe to send, an unimplemented one comes back 405
// instead of failing the connection, and what is left is the truth for
// this hardware.
//
// This is the first thing to run against a camera nobody here has seen.
func probe(conn *baichuan.Conn, dial func() (*baichuan.Conn, error)) error {
	names := baichuan.ConfigNames()
	var ok, absent, needsArgs, hungUp []string

	for _, name := range names {
		id := baichuan.ConfigMessages[name]
		xml, status, err := fetch(conn, id)
		switch {
		case err != nil:
			// Some messages make a camera hang up rather than answer.
			// That is a fact about the model worth reporting, so record
			// it, reconnect, and keep going.
			hungUp = append(hungUp, fmt.Sprintf("%s (%d)", name, id))
			conn.Close()
			if conn, err = dial(); err != nil {
				return fmt.Errorf("could not reconnect after %s: %w", name, err)
			}
		case len(xml) > 0:
			ok = append(ok, fmt.Sprintf("%s (%d, %d bytes)", name, id, len(xml)))
		case status == 400:
			// Understood, but it wants parameters this generic read does
			// not send. Supported, not readable this way.
			needsArgs = append(needsArgs, fmt.Sprintf("%s (%d)", name, id))
		default:
			absent = append(absent, fmt.Sprintf("%s (%d)", name, id))
		}
	}

	report := func(title string, items []string) {
		fmt.Printf("\n%s: %d\n", title, len(items))
		for _, it := range items {
			fmt.Printf("  %s\n", it)
		}
	}
	fmt.Printf("probed %d reads\n", len(names))
	report("supported", ok)
	report("needs parameters (400)", needsArgs)
	report("not implemented (405)", absent)
	if len(hungUp) > 0 {
		report("hung up the connection", hungUp)
	}
	return nil
}

// verify checks that this camera accepts a configuration write, for every
// read/write pair it implements, without changing anything.
//
// For each pair it reads the document, writes that exact document back, and
// reads again. A camera that accepts the write and returns an identical
// document has demonstrated the write path for that message with no change
// to the camera. It does not demonstrate that a *different* document would
// take effect, which needs a real edit and an observed result.
//
// Writes are skipped unless -write is given, and some are skipped regardless:
// re-applying an encoder or image configuration reconfigures the pipeline and
// interrupts the stream. Those are listed rather than silently dropped.
func verify(conn *baichuan.Conn, dial func() (*baichuan.Conn, error), args []string) error {
	doWrite := len(args) == 1 && args[0] == "-write"
	if len(args) > 0 && !doWrite {
		return fmt.Errorf("usage: verify [-write]")
	}
	if !doWrite {
		fmt.Println("read-only; pass -write to send each document back unchanged")
	}

	var accepted, refused, unreadable, held []string
	for _, p := range baichuan.ConfigPairs() {
		before, status, err := fetch(conn, p.Get)
		if err != nil {
			conn.Close()
			if conn, err = dial(); err != nil {
				return fmt.Errorf("reconnect after %d: %w", p.Get, err)
			}
			continue
		}
		if len(before) == 0 {
			// 405 is "this model does not have it", which is not a failure.
			_ = status
			continue
		}
		label := fmt.Sprintf("%s (%d->%d)", p.Name, p.Get, p.Set)
		if baichuan.UnsafeToRewrite(p.Set) {
			held = append(held, label)
			continue
		}
		if !doWrite {
			unreadable = append(unreadable, label)
			continue
		}

		if err := conn.SetConfig(p.Set, before); err != nil {
			return fmt.Errorf("write %d: %w", p.Set, err)
		}
		_, wstatus, err := await(conn, p.Set)
		if err != nil {
			conn.Close()
			if conn, err = dial(); err != nil {
				return fmt.Errorf("reconnect after writing %d: %w", p.Set, err)
			}
			refused = append(refused, label+" hung up")
			continue
		}
		after, _, err := fetch(conn, p.Get)
		if err != nil {
			return fmt.Errorf("re-read %d: %w", p.Get, err)
		}
		switch {
		case wstatus != 200 && wstatus != 0:
			refused = append(refused, fmt.Sprintf("%s status %d", label, wstatus))
		case !bytes.Equal(before, after):
			// Accepted, and the document moved. Worth knowing loudly:
			// the camera rewrote something we did not ask it to.
			refused = append(refused, label+" ACCEPTED BUT DOCUMENT CHANGED")
		default:
			accepted = append(accepted, label)
		}
	}

	report := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Printf("\n%s: %d\n", title, len(items))
		for _, it := range items {
			fmt.Printf("  %s\n", it)
		}
	}
	report("readable, not written (no -write)", unreadable)
	report("accepted, document unchanged", accepted)
	report("refused or changed", refused)
	report("held back as disruptive to rewrite", held)
	return nil
}
