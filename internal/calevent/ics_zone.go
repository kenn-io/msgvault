package calevent

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

var customZonePattern = regexp.MustCompile(`^STD[+-]\d{2}:\d{2}(DST[+-]\d{2}:\d{2},M\d{1,2}\.[1-5]\.[0-6]/\d{2}:\d{2}:\d{2},M\d{1,2}\.[1-5]\.[0-6]/\d{2}:\d{2}:\d{2})?$`)

func customICSZones(cal *ical.Calendar) (map[string]string, error) {
	zones := make(map[string]string)
	for _, zone := range cal.Children {
		if zone.Name != "VTIMEZONE" {
			continue
		}
		id := normalizeICSZone(zone.Props.Get("TZID").Value)
		if _, err := time.LoadLocation(id); err == nil {
			continue
		}
		offsets, rules := make(map[string]string), make(map[string]string)
		for _, part := range zone.Children {
			offset, from, start, rule := part.Props.Get("TZOFFSETTO"), part.Props.Get("TZOFFSETFROM"), part.Props.Get("DTSTART"), part.Props.Get("RRULE")
			if (part.Name != "STANDARD" && part.Name != "DAYLIGHT") || offsets[part.Name] != "" || offset == nil || start == nil {
				return nil, fmt.Errorf("VTIMEZONE %q requires distinct STANDARD or DAYLIGHT observances with TZOFFSETTO and DTSTART", id)
			}
			_, offsetErr := time.Parse("-0700", offset.Value)
			wall, startErr := time.Parse("20060102T150405", start.Value)
			if offsetErr != nil || startErr != nil || rule == nil && (from == nil || from.Value != offset.Value) {
				return nil, fmt.Errorf("unsupported VTIMEZONE %q offset or start", id)
			}
			offsets[part.Name] = strings.NewReplacer("+", "-", "-", "+").Replace(offset.Value[:3]) + ":" + offset.Value[3:]
			if rule == nil {
				continue // Exchange writes zones without daylight saving as observances without RRULE.
			}
			r, err := rrule.StrToROption(rule.Value)
			if err != nil || r.Freq != rrule.YEARLY || len(r.Bymonth) != 1 || len(r.Byweekday) != 1 || strings.Count(rule.Value, ";") != 2 ||
				r.Bymonth[0] < 1 || r.Bymonth[0] > 12 || r.Byweekday[0].N() < -1 || r.Byweekday[0].N() == 0 || r.Byweekday[0].N() > 4 {
				return nil, fmt.Errorf("unsupported VTIMEZONE %q rule", id)
			}
			rules[part.Name] = fmt.Sprintf("M%d.%d.%d/%s", r.Bymonth[0], (r.Byweekday[0].N()+6)%6, (r.Byweekday[0].Day()+1)%7, wall.Format("15:04:05"))
		}
		zones[id] = "STD" + offsets["STANDARD"]
		if dst := offsets["DAYLIGHT"]; dst != "" && dst != offsets["STANDARD"] {
			zones[id] += "DST" + dst + "," + rules["DAYLIGHT"] + "," + rules["STANDARD"]
		}
		if !customZonePattern.MatchString(zones[id]) {
			return nil, fmt.Errorf("VTIMEZONE %q requires STANDARD, and RRULE on both observances when daylight saving changes the offset", id)
		}
	}
	return zones, nil
}

// LoadZone reloads named zones and the embedded rules retained from an ICS invite.
func LoadZone(name string) (*time.Location, error) {
	loc, err := time.LoadLocation(name)
	if err != nil && customZonePattern.MatchString(name) {
		// Two TZif v2 blocks with no transitions apply the POSIX footer to all times.
		data := []byte("TZif2" + strings.Repeat("\x00", 46))
		data[39], data[43] = 1, 1
		data = append(data, data...)
		loc, err = time.LoadLocationFromTZData(name, append(data, []byte("\n"+name+"\n")...))
	}
	if err != nil {
		return nil, fmt.Errorf("load time zone %q: %w", name, err)
	}
	return loc, nil
}
