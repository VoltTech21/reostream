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
	"time"

	"github.com/VoltTech21/reostream/internal/adpcm"
	"github.com/VoltTech21/reostream/internal/baichuan"
)

func usage() {
	fmt.Fprintln(os.Stderr, `reocam controls a Reolink camera over Baichuan.

  reocam -address CAM [-password PW] <command> [args]

Commands:
  abilities              list what this camera can do and what is writable
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := baichuan.Dial(ctx, *addr, baichuan.Options{Username: *user, Password: *pass})
	if err != nil {
		fmt.Fprintln(os.Stderr, "reocam:", err)
		os.Exit(1)
	}
	defer conn.Close()

	switch flag.Arg(0) {
	case "abilities":
		err = abilities(conn)
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
