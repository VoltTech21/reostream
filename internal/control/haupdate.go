package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/mqtt"
)

// reostream itself in Home Assistant (#39): a device of its own with one
// MQTT update entity, so a new release shows under Settings > Updates beside
// Home Assistant's others, with its notes and a link. It reports versions
// only; there is no install button, since how reostream is installed (Docker,
// the add-on, Unraid, a binary) decides how it is updated.
//
// The latest version is read from GitHub's releases API. That is the one
// call reostream makes to the internet, so update_check = false under
// [events] turns it off; the entity then shows only the installed version.

// releasesLatest is GitHub's latest-release document for the repository. A
// var so a test can point it at its own server.
var releasesLatest = "https://api.github.com/repos/VoltTech21/reostream/releases/latest"

// updateEvery is how often the latest release is read. GitHub allows 60
// unauthenticated reads an hour per address; four a day is far below that.
var updateEvery = 6 * time.Hour

// haSummaryMax is Home Assistant's limit on release_summary; a longer one
// makes it reject the whole state.
const haSummaryMax = 255

type release struct {
	Tag  string `json:"tag_name"`
	Name string `json:"name"`
	URL  string `json:"html_url"`
	Body string `json:"body"`
}

// haUpdateNode is the device id for this reostream. The topic prefix is in
// it so two reostreams on one broker are two devices, and it starts
// "reostream-" where every camera's starts "reostream_", so no camera name
// can land on it.
func haUpdateNode(prefix string) string {
	return "reostream-" + strings.TrimPrefix(haNodeID(prefix), "reostream_")
}

func haUpdateStateTopic(prefix string) string { return prefix + "/reostream/update" }

// haUpdateConfig is the update entity's discovery document.
func haUpdateConfig(prefix, version string) discoveryMessage {
	node := haUpdateNode(prefix)
	doc := map[string]any{
		"name":                  "Update",
		"unique_id":             node + "_update",
		"state_topic":           haUpdateStateTopic(prefix),
		"availability_topic":    haStatusTopic(prefix),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"device_class":          "firmware",
		"entity_category":       "diagnostic",
		"device": map[string]any{
			"identifiers":  []string{node},
			"name":         "reostream",
			"manufacturer": "reostream",
			"sw_version":   version,
		},
		"origin": map[string]any{
			"name": "reostream",
			"sw":   version,
			"url":  "https://github.com/VoltTech21/reostream",
		},
	}
	b, _ := json.Marshal(doc)
	return discoveryMessage{Topic: haDiscoveryPrefix + "/update/" + node + "/update/config", Payload: b}
}

// haUpdateState is the entity's state. Without a release read it carries
// only the installed version, and Home Assistant shows no update.
func haUpdateState(installed string, rel *release) []byte {
	st := map[string]any{"installed_version": installed, "title": "reostream"}
	if rel != nil && rel.Tag != "" {
		st["latest_version"] = rel.Tag
		if strings.HasPrefix(rel.URL, "https://") {
			st["release_url"] = rel.URL
		}
		if s := releaseSummary(rel.Body); s != "" {
			st["release_summary"] = s
		}
	} else {
		st["latest_version"] = installed
	}
	b, _ := json.Marshal(st)
	return b
}

// releaseSummary is the start of the release notes, cut at a line end to
// fit Home Assistant's limit. The full notes are a click away on
// release_url.
func releaseSummary(body string) string {
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if len(body) <= haSummaryMax {
		return body
	}
	cut := body[:haSummaryMax-1]
	if i := strings.LastIndex(cut, "\n"); i > haSummaryMax/3 {
		cut = cut[:i]
	} else if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}

func fetchLatestRelease(ctx context.Context, c *http.Client, installed string) (*release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesLatest, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "reostream/"+installed)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var r release
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// publishUpdate keeps the entity's state current (startDiscovery announces
// the entity itself): the installed version at once, and the latest release
// every updateEvery when check is on. A failed read keeps the last good one
// and is logged once until a read works again.
func publishUpdate(ctx context.Context, mq *mqtt.Client, prefix, installed string, check bool) {
	var latest *release
	client := &http.Client{Timeout: 30 * time.Second}
	failing := false
	for {
		if check {
			if r, err := fetchLatestRelease(ctx, client, installed); err != nil {
				if !failing && ctx.Err() == nil {
					log.Printf("control: could not read the latest reostream release: %v", err)
				}
				failing = true
			} else {
				latest, failing = r, false
			}
		}
		if err := mq.Publish(haUpdateStateTopic(prefix), haUpdateState(installed, latest), 0, true); err != nil && ctx.Err() == nil {
			log.Printf("control: home assistant update entity: %v", err)
		}
		if !check {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(updateEvery):
		}
	}
}
