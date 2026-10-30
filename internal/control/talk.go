package control

import (
	"context"
	"encoding/binary"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/talk"
)

// talkUpgrader refuses a request whose Origin is not this page's own host:
// without that, any site the operator visits could open a talk session
// through their logged-in browser. originMatches is the library's default
// check plus one case, Home Assistant's ingress, where the browser's host
// may reach this page only as X-Forwarded-Host.
var talkUpgrader = websocket.Upgrader{ReadBufferSize: 8192, CheckOrigin: originMatches}

// serveTalk is the camera page's hold-to-talk button. The browser sends 16
// bit mono PCM at ?rate= over a WebSocket for as long as the button is held,
// and it goes to the camera's speaker through internal/talk, the same path
// the RTSP back channel uses. Text messages back are status for the page.
func (s *Server) serveTalk(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	rate, err := strconv.Atoi(r.URL.Query().Get("rate"))
	if err != nil || rate < 8000 || rate > 96000 {
		http.Error(w, "rate must be a sample rate between 8000 and 96000", http.StatusBadRequest)
		return
	}
	ws, err := talkUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	say := func(msg string) { _ = ws.WriteMessage(websocket.TextMessage, []byte(msg)) }

	sess, err := talk.Open(r.Context(), func(ctx context.Context) (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, cam.Address, baichuan.Options{
			Username: cam.Username,
			Password: cam.Password,
			Channel:  cam.Channel,
			// A battery camera keeps port 9000 shut; it talks over the
			// same UDP session its video uses.
			UID: cam.UID,
			// Nothing comes back during talk; the session ends with the button.
			IdleTimeout: 24 * time.Hour,
		})
	})
	if err != nil {
		say("could not start talk: " + err.Error())
		return
	}
	defer sess.Close()
	say("talking, release to stop")

	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		pcm := make([]int16, len(data)/2)
		for i := range pcm {
			pcm[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
		}
		if err := sess.Write(rate, pcm); err != nil {
			say("talk stopped: " + err.Error())
			return
		}
	}
}
