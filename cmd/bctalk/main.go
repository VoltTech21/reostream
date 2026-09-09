// Command bctalk exercises a camera's two-way audio.
//
// It runs in three steps, each of which can be run on its own, because only
// the last one makes a noise in the room the camera is in:
//
//	bctalk -address CAM                 ask what the camera supports
//	bctalk -address CAM -negotiate      also open a session, silently
//	bctalk -address CAM -play FILE      also send audio, which is audible
//
// FILE is raw signed 16 bit little endian mono PCM at the camera's sample
// rate. Produce it with: ffmpeg -i in.mp3 -f s16le -ac 1 -ar 16000 out.raw
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

func main() {
	addr := flag.String("address", "", "camera address")
	pass := flag.String("password", "", "password")
	negotiate := flag.Bool("negotiate", false, "also open a talk session, silently")
	withVideo := flag.String("with-video", "", "start this video stream first: main, sub or extern")
	play := flag.String("play", "", "also send this raw PCM file, which is audible")
	flag.Parse()

	if *addr == "" {
		fmt.Fprintln(os.Stderr, "bctalk: -address is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := baichuan.Dial(ctx, *addr, baichuan.Options{Username: "admin", Password: *pass})
	if err != nil {
		fmt.Fprintln(os.Stderr, "bctalk:", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := conn.TalkAbility(); err != nil {
		fmt.Fprintln(os.Stderr, "bctalk: ask:", err)
		os.Exit(1)
	}

	format, err := waitForAbility(conn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bctalk:", err)
		os.Exit(1)
	}
	fmt.Printf("camera accepts: %s %d Hz %d bit %s, %d samples per block, %s\n",
		format.AudioType, format.SampleRate, format.SamplePrecision,
		format.SoundTrack, format.LengthPerEncoder, format.Duplex)

	if !*negotiate && *play == "" {
		return
	}
	if format.AudioType != "adpcm" {
		fmt.Fprintf(os.Stderr, "bctalk: this camera wants %q, which is not implemented\n", format.AudioType)
		os.Exit(1)
	}

	if *withVideo != "" {
		kind := map[string]string{"main": baichuan.StreamMain, "sub": baichuan.StreamSub,
			"extern": baichuan.StreamExtern}[*withVideo]
		if err := conn.StartVideo(kind); err != nil {
			fmt.Fprintln(os.Stderr, "bctalk: start video:", err)
			os.Exit(1)
		}
		time.Sleep(2 * time.Second)
		fmt.Println("video stream started")
	}

	if err := conn.TalkConfig(format); err != nil {
		fmt.Fprintln(os.Stderr, "bctalk: config:", err)
		os.Exit(1)
	}
	// A camera that does not like the config says so in the status byte of
	// its reply. Without this check an ignored request looks like a working
	// one right up until nothing comes out of the speaker.
	if err := waitForAck(conn, baichuan.MsgIDTalkConfig); err != nil {
		fmt.Fprintln(os.Stderr, "bctalk: config:", err)
		os.Exit(1)
	}
	fmt.Println("talk session opened, camera acknowledged")

	if *play == "" {
		fmt.Println("no audio sent")
		return
	}

	pcm, err := readPCM(*play)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bctalk:", err)
		os.Exit(1)
	}
	secs := float64(len(pcm)) / float64(format.SampleRate)
	fmt.Printf("sending %d samples, %.1f seconds\n", len(pcm), secs)

	// Real time pacing. The camera plays what arrives; sending a whole file at
	// once would overrun whatever it buffers.
	var enc adpcm.Encoder
	per := time.Duration(float64(adpcm.SamplesPerBlock) / float64(format.SampleRate) * float64(time.Second))
	next := time.Now()
	for off := 0; off < len(pcm); off += adpcm.SamplesPerBlock {
		end := off + adpcm.SamplesPerBlock
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := conn.Talk(baichuan.TalkPacket(enc.EncodeBlock(pcm[off:end]))); err != nil {
			fmt.Fprintln(os.Stderr, "bctalk: send:", err)
			os.Exit(1)
		}
		next = next.Add(per)
		time.Sleep(time.Until(next))
	}
	fmt.Println("done")
}

func waitForAbility(conn *baichuan.Conn) (baichuan.TalkFormat, error) {
	deadline := time.After(8 * time.Second)
	for {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				return baichuan.TalkFormat{}, fmt.Errorf("connection closed before a TalkAbility reply")
			}
			if m.Header.MsgID == baichuan.MsgIDTalkAbility && len(m.XML) > 0 {
				return baichuan.ParseTalkAbility(m.XML)
			}
		case <-deadline:
			return baichuan.TalkFormat{}, fmt.Errorf("no TalkAbility reply")
		}
	}
}

// waitForAck waits for the camera's reply to a request and checks its status
// byte. 200 is success, as in HTTP.
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

func readPCM(path string) ([]int16, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return out, nil
}
