// Command bcprobe connects to one camera and writes its video elementary
// stream to stdout. It proves the protocol implementation against a real
// camera; the daemon is built on the same package.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

func main() {
	addr := flag.String("address", "", "camera address, host or host:port")
	user := flag.String("username", "admin", "username")
	pass := flag.String("password", "", "password")
	stream := flag.String("stream", "main", "main, sub or extern")
	seconds := flag.Int("seconds", 0, "stop after this many seconds, 0 for no limit")
	dump := flag.String("dump-payload", "", "also write the raw reassembled media payload here, for protocol work")
	flag.Parse()

	if *addr == "" {
		fmt.Fprintln(os.Stderr, "bcprobe: --address is required")
		os.Exit(2)
	}
	kind := map[string]string{
		"main": baichuan.StreamMain, "sub": baichuan.StreamSub, "extern": baichuan.StreamExtern,
	}[*stream]
	if kind == "" {
		fmt.Fprintf(os.Stderr, "bcprobe: unknown stream %q\n", *stream)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := baichuan.Dial(ctx, *addr, baichuan.Options{Username: *user, Password: *pass})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bcprobe: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "bcprobe: logged in, nonce %s\n", conn.Nonce())

	// Always close cleanly: closing is what releases the camera's session.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		conn.Close()
	}()

	if err := conn.StartVideo(kind); err != nil {
		fmt.Fprintf(os.Stderr, "bcprobe: start video: %v\n", err)
		conn.Close()
		os.Exit(1)
	}

	out := bufio.NewWriterSize(os.Stdout, 256*1024)
	d := baichuan.NewDepacketiser()
	var deadline time.Time
	if *seconds > 0 {
		deadline = time.Now().Add(time.Duration(*seconds) * time.Second)
	}
	var dumpFile *os.File
	if *dump != "" {
		dumpFile, err = os.Create(*dump)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bcprobe: %v\n", err)
			conn.Close()
			os.Exit(1)
		}
		defer dumpFile.Close()
	}

	lastPing := time.Now()
	var frames, keyframes int
	var codec string
	sawKeyframe := false

	for m := range conn.Messages() {
		if len(m.Payload) > 0 {
			if dumpFile != nil {
				_, _ = dumpFile.Write(m.Payload)
			}
			d.Write(m.Payload, m.StartsPacket)
			for {
				f, ok := d.Next()
				if !ok {
					break
				}
				switch f.Kind {
				case baichuan.FrameIFrame:
					sawKeyframe, keyframes, codec = true, keyframes+1, f.Codec
				case baichuan.FramePFrame:
					if !sawKeyframe {
						continue // starting mid-GOP only confuses a decoder
					}
				default:
					continue
				}
				if _, err := out.Write(f.Video()); err != nil {
					fmt.Fprintf(os.Stderr, "bcprobe: write: %v\n", err)
					conn.Close()
					os.Exit(1)
				}
				frames++
			}
		}

		// Checked after each message rather than in a select alongside the
		// message channel: frames arrive every few tens of milliseconds, so a
		// timer case beside a ready channel is effectively never chosen.
		if time.Since(lastPing) >= 10*time.Second {
			lastPing = time.Now()
			if err := conn.Ping(); err != nil {
				fmt.Fprintf(os.Stderr, "bcprobe: ping: %v\n", err)
				break
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
	}

	conn.Close()
	out.Flush()
	fmt.Fprintf(os.Stderr, "bcprobe: %d frames (%d keyframes) codec=%s skipped=%d\n",
		frames, keyframes, codec, d.Skipped())
	if frames == 0 {
		os.Exit(1)
	}
}
