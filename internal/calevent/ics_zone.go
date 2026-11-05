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
			if (part.Name != "STANDARD" && part.Name != "DAYLIGHT") || offsets[part.Name] != "" {
				return nil, fmt.Errorf("unsupported VTIMEZONE %q observances", id)
			}
			offset, start, rule := part.Props.Get("TZOFFSETTO"), part.Props.Get("DTSTART"), part.Props.Get("RRULE")
			if offset == nil || start == nil || rule == nil {
				return nil, fmt.Errorf("VTIMEZONE %q requires TZOFFSETTO, DTSTART, and RRULE", id)
			}
			_, offsetErr := time.Parse("-0700", offset.Value)
			wall, startErr := time.Parse("20060102T150405", start.Value)
			r, err := rrule.StrToROption(rule.Value)
			if offsetErr != nil || startErr != nil || err != nil || r.Freq != rrule.YEARLY || len(r.Bymonth) != 1 || len(r.Byweekday) != 1 || strings.Count(rule.Value, ";") != 2 {
				return nil, fmt.Errorf("unsupported VTIMEZONE %q rule", id)
			}
			week := r.Byweekday[0].N()
			if r.Bymonth[0] < 1 || r.Bymonth[0] > 12 || (week != -1 && (week < 1 || week > 4)) {
				return nil, fmt.Errorf("unsupported VTIMEZONE %q transition", id)
			}
			offsets[part.Name] = strings.NewReplacer("+", "-", "-", "+").Replace(offset.Value[:3]) + ":" + offset.Value[3:]
			rules[part.Name] = fmt.Sprintf("M%d.%d.%d/%s", r.Bymonth[0], (week+6)%6, (r.Byweekday[0].Day()+1)%7, wall.Format("15:04:05"))
		}
		if offsets["STANDARD"] == "" {
			return nil, fmt.Errorf("VTIMEZONE %q requires STANDARD", id)
		}
		zones[id] = "STD" + offsets["STANDARD"]
		if offsets["DAYLIGHT"] != "" {
			zones[id] += "DST" + offsets["DAYLIGHT"] + "," + rules["DAYLIGHT"] + "," + rules["STANDARD"]
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
