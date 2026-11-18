package control

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/VoltTech21/reostream/internal/adpcm"
	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Custom siren sound: the page decodes any audio file in the browser to 16
// kHz mono 16-bit PCM and posts it here; it is encoded to the camera's raw
// ADPCM stream, uploaded (import audio 261 + save 262) and selected as the
// siren sound (audio cfg audioSelect 1), the same three steps the Reolink app
// takes. The app records at most 5 seconds, so that is the limit here too.

const soundMaxBytes = 5 * 16000 * 2

func (s *Server) serveSound(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	reply := func(ok bool, detail string) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": ok, "detail": detail})
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, soundMaxBytes+1))
	if err != nil || len(raw) < 2 || len(raw)%2 != 0 {
		reply(false, "the sound did not arrive whole")
		return
	}
	if len(raw) > soundMaxBytes {
		reply(false, "a siren sound is at most 5 seconds")
		return
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	conn, err := s.dial(ctx, cam)
	if err != nil {
		reply(false, err.Error())
		return
	}
	defer conn.Close()
	if err := baichuan.UploadSound(ctx, conn, adpcm.EncodeStream(pcm)); err != nil {
		reply(false, err.Error())
		return
	}
	// Select it, as the app does after saving one.
	pair, _ := pairForName("audio cfg get")
	doc, status, err := baichuan.ReadConfig(ctx, conn, pair.Get)
	if err == nil && status == 200 {
		if body, err := setField(doc, "audioCfg/audioSelect", "1"); err == nil {
			baichuan.WriteConfig(ctx, conn, pair.Set, body)
		}
	}
	reply(true, fmt.Sprintf("uploaded %.1f seconds and set as the siren sound", float64(len(pcm))/16000))
}

// serveSoundTest plays the selected siren sound once on the camera (play
// stop audio, 263, the app's Sound Test).
func (s *Server) serveSoundTest(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	conn, err := s.dial(ctx, cam)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	defer conn.Close()
	status, err := baichuan.WriteConfig(ctx, conn, baichuan.MsgIDPlayStopAudio, baichuan.SirenXML(cam.Channel))
	if err != nil || status != 200 {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "detail": fmt.Sprintf("%v %s", err, baichuan.ExplainStatus(status))})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "detail": "playing"})
}
