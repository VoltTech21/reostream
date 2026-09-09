package baichuan

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
)

// Ability is one thing a camera can do, and whether the logged in user may
// only read it or also change it.
//
// The camera reports these as a comma separated list of "name_rw" and
// "name_ro" strings grouped by module, so the suffix is the permission and
// the rest is the name.
type Ability struct {
	Module   string
	Name     string
	Writable bool
}

type abilityModule struct {
	Sub []struct {
		ChannelID    int    `xml:"channelId"`
		AbilityValue string `xml:"abilityValue"`
	} `xml:"subModule"`
}

type abilityReply struct {
	XMLName xml.Name `xml:"body"`
	Info    struct {
		UserName string        `xml:"userName"`
		System   abilityModule `xml:"system"`
		Network  abilityModule `xml:"network"`
		Alarm    abilityModule `xml:"alarm"`
		Record   abilityModule `xml:"record"`
		Video    abilityModule `xml:"video"`
		Image    abilityModule `xml:"image"`
	} `xml:"AbilityInfo"`
}

// ParseAbilities reads an AbilityInfo reply into a sorted list.
func ParseAbilities(x []byte) ([]Ability, error) {
	var r abilityReply
	if err := xml.Unmarshal(x, &r); err != nil {
		return nil, fmt.Errorf("baichuan: parse abilities: %w", err)
	}

	var out []Ability
	add := func(module string, m abilityModule) {
		for _, sub := range m.Sub {
			for _, field := range strings.Split(sub.AbilityValue, ",") {
				field = strings.TrimSpace(field)
				if field == "" {
					continue
				}
				a := Ability{Module: module, Name: field}
				switch {
				case strings.HasSuffix(field, "_rw"):
					a.Name, a.Writable = strings.TrimSuffix(field, "_rw"), true
				case strings.HasSuffix(field, "_ro"):
					a.Name = strings.TrimSuffix(field, "_ro")
				}
				out = append(out, a)
			}
		}
	}
	i := r.Info
	add("system", i.System)
	add("network", i.Network)
	add("alarm", i.Alarm)
	add("record", i.Record)
	add("video", i.Video)
	add("image", i.Image)

	sort.Slice(out, func(a, b int) bool {
		if out[a].Module != out[b].Module {
			return out[a].Module < out[b].Module
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}
