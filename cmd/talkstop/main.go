package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

func main() {
	addr := flag.String("address", "", "")
	pass := flag.String("password", "", "")
	id := flag.Uint("id", 203, "message id to try as a talk stop")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	c, err := baichuan.Dial(ctx, *addr, baichuan.Options{Username: "admin", Password: *pass})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer c.Close()

	if err := c.RawChannelRequest(uint32(*id)); err != nil {
		fmt.Fprintln(os.Stderr, "send:", err)
		os.Exit(1)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-c.Messages():
			if !ok {
				fmt.Println("closed")
				return
			}
			if m.Header.MsgID == uint32(*id) {
				fmt.Printf("id %d -> status %d, xml %q\n", *id, m.Header.Status(), m.XML)
				return
			}
		case <-deadline:
			fmt.Printf("id %d -> no reply\n", *id)
			return
		}
	}
}
