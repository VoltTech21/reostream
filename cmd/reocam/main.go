// Command reocam controls a camera over Baichuan.
//
// It is deliberately separate from the reostream daemon. The daemon's value
// is a small surface and a short list of non-goals; this is where everything
// a camera can be asked to do lives instead.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/adpcm"
	"github.com/VoltTech21/reostream/internal/baichuan"
)

func usage() {
	fmt.Fprintln(os.Stderr, `reocam controls a Reolink camera over Baichuan.

  reocam -address CAM [-password PW] <command> [args]

Commands:
  support                what hardware this camera actually has
  abilities              what the logged in user may read and write
  get NAME               print one configuration block as the camera sends it
  get all                try every known block and report which the camera has
  snap [main|sub] FILE   save a still image
  talk FILE              play raw 16 bit mono PCM through the camera speaker
  talkinfo               report the two-way audio formats the camera accepts`)
}

func main() {
	addr := flag.String("address", "", "camera address")
	user := flag.String("username", "admin", "username")
	pass := flag.String("password", "", "password")
	withVideo := flag.String("with-video", "", "open this video stream first: main, sub or extern")
	noConfig := flag.Bool("no-config", false, "skip TalkConfig and send audio into an existing session")
	flag.Usage = usage
	flag.Parse()

	if *addr == "" || flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}

	// Long enough for a full sweep, which asks nearly thirty questions and
	// waits out a timeout for each one a camera ignores.
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
	case "support":
		err = support(conn)
	case "abilities":
		err = abilities(conn)
	case "get":
		err = get(conn, dial, flag.Args()[1:])
	case "snap":
		err = snap(conn, flag.Args()[1:])
	case "talkinfo":
		_, err = talkFormat(conn, true)
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
	return nil
}
