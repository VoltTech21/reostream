// Command bcpcap decodes a Baichuan conversation out of a packet capture.
//
// Everything left of the protocol is blocked on the same thing: the numeric
// message ids. They are not in a table in any firmware here, they live in
// dispatch code, and recovering one costs an afternoon of disassembly. A
// capture of an official client driving a camera settles all of them at once,
// because every id arrives with the XML that names it.
//
// The catch a capture alone does not solve is that everything after login is
// AES encrypted. The key is derivable, though: it is the login nonce and the
// password, and the nonce travels in the handshake under a fixed cipher. So
// given the password, a capture decodes completely.
//
//	bcpcap -password PW capture.pcap
//	bcpcap -password PW -xml capture.pcap      # full XML, not just the root
//
// Capture it at the machine running the client, which needs no network
// tricks:
//
//	Wireshark on the same PC as the Reolink client, filter "tcp port 9000",
//	then click through the settings you want the ids for.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// A capture is two byte streams, one per direction, reassembled from the TCP
// payloads in file order. Retransmits and reordering would need sequence
// tracking; in practice a local capture of a short session has neither, and a
// message that fails to parse is reported rather than silently skipped.
type flowKey struct {
	src, dst string
}

func main() {
	pass := flag.String("password", "", "camera password, needed to derive the AES key")
	user := flag.String("username", "admin", "username")
	showXML := flag.Bool("xml", false, "print each message's full XML")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: bcpcap [-password PW] [-xml] CAPTURE.pcap")
		os.Exit(2)
	}

	packets, err := readPcap(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "bcpcap:", err)
		os.Exit(1)
	}

	flows := map[flowKey][]byte{}
	var order []flowKey
	for _, p := range packets {
		if p.srcPort != 9000 && p.dstPort != 9000 {
			continue
		}
		k := flowKey{fmt.Sprintf("%s:%d", p.src, p.srcPort), fmt.Sprintf("%s:%d", p.dst, p.dstPort)}
		if _, seen := flows[k]; !seen {
			order = append(order, k)
		}
		flows[k] = append(flows[k], p.payload...)
	}
	if len(flows) == 0 {
		fmt.Fprintln(os.Stderr, "bcpcap: no TCP port 9000 traffic in this capture")
		os.Exit(1)
	}

	seen := map[uint32]map[string]int{}
	for _, k := range order {
		fmt.Printf("\n=== %s -> %s, %d bytes\n", k.src, k.dst, len(flows[k]))
		decodeStream(flows[k], *user, *pass, *showXML, seen)
	}

	fmt.Printf("\n=== message ids and the elements seen with them\n")
	ids := make([]uint32, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	for _, id := range ids {
		var names []string
		for n, c := range seen[id] {
			names = append(names, fmt.Sprintf("%s x%d", n, c))
		}
		sort.Strings(names)
		fmt.Printf("  %4d  %s\n", id, strings.Join(names, ", "))
	}
}

var rootRe = regexp.MustCompile(`<([A-Za-z][A-Za-z0-9_]*)[ >/]`)

// rootElement names the first element that is not the xml declaration or a
// generic wrapper, which is what identifies the message.
func rootElement(x []byte) string {
	for _, m := range rootRe.FindAllSubmatch(x, -1) {
		name := string(m[1])
		if name == "body" || name == "xml" {
			continue
		}
		return name
	}
	return ""
}

func decodeStream(buf []byte, user, pass string, showXML bool, seen map[uint32]map[string]int) {
	var key []byte
	nonce := ""

	for off := 0; off+20 <= len(buf); {
		// The magic is the bytes f0 de bc 0a, which read as this value
		// little endian. Both directions use it.
		if binary.LittleEndian.Uint32(buf[off:]) != 0x0abcdef0 {
			off++
			continue
		}
		msgID := binary.LittleEndian.Uint32(buf[off+4:])
		msgLen := binary.LittleEndian.Uint32(buf[off+8:])
		encOffset := binary.LittleEndian.Uint32(buf[off+12:])
		status := int16(binary.LittleEndian.Uint16(buf[off+16:]))
		class := binary.LittleEndian.Uint16(buf[off+18:])

		hdr := 20
		payloadOff := uint32(0)
		if class == 0x6414 || class == 0x0000 {
			hdr = 24
			if off+24 > len(buf) {
				return
			}
			payloadOff = binary.LittleEndian.Uint32(buf[off+20:])
		}
		if off+hdr+int(msgLen) > len(buf) {
			fmt.Printf("  [truncated message %d, wanted %d bytes]\n", msgID, msgLen)
			return
		}
		body := buf[off+hdr : off+hdr+int(msgLen)]

		xmlEnd := len(body)
		if hdr == 24 && payloadOff > 0 && int(payloadOff) <= len(body) {
			xmlEnd = int(payloadOff)
		}
		plain := decrypt(body[:xmlEnd], key, encOffset)

		// The login reply carries the nonce, and everything after it is AES.
		if key == nil {
			if n := findNonce(plain); n != "" {
				nonce = n
				key = aesKey(nonce, pass)
			}
		}

		root := rootElement(plain)
		if root != "" {
			if seen[msgID] == nil {
				seen[msgID] = map[string]int{}
			}
			seen[msgID][root]++
		}
		fmt.Printf("  id %-4d class %#06x status %-4d len %-7d %s\n", msgID, class, status, msgLen, root)
		if showXML && len(plain) > 0 && root != "" {
			fmt.Printf("%s\n", indent(plain))
		}
		off += hdr + int(msgLen)
	}
}

func indent(b []byte) string {
	var sb strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		sb.WriteString("      " + line + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
