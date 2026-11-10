package calevent

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
	"go.kenn.io/msgvault/internal/calrecur"
)

var customZonePattern = regexp.MustCompile(`^STD[+-]\d{2}:\d{2}(DST[+-]\d{2}:\d{2},M\d{1,2}\.[1-5]\.[0-6]/\d{2}:\d{2}:\d{2},M\d{1,2}\.[1-5]\.[0-6]/\d{2}:\d{2}:\d{2})?$`)

type zoneObservance struct {
	kind, offset, rule string
	start, wall        time.Time
	until              bool
}
type zoneEra struct {
	start time.Time
	rule  string
}

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
		var obs []zoneObservance
		var hasRule, hasRDate bool
		for _, part := range zone.Children {
			offset, from, start, rule := part.Props.Get("TZOFFSETTO"), part.Props.Get("TZOFFSETFROM"), part.Props.Get("DTSTART"), part.Props.Get("RRULE")
			rdates := part.Props.Values("RDATE")
			hasRule, hasRDate = hasRule || rule != nil, hasRDate || len(rdates) > 0
			if part.Props.Get("EXDATE") != nil || hasRule && hasRDate {
				return nil, fmt.Errorf("unsupported VTIMEZONE %q observance", id)
			}
			if (part.Name != "STANDARD" && part.Name != "DAYLIGHT") || offset == nil || from == nil || start == nil {
				return nil, fmt.Errorf("unsupported VTIMEZONE %q observance", id)
			}
			_, offsetErr := time.Parse("-0700", offset.Value)
			fromTime, fromErr := time.Parse("-0700", from.Value)
			wall, startErr := time.Parse("20060102T150405", start.Value)
			if offsetErr != nil || fromErr != nil || startErr != nil {
				return nil, fmt.Errorf("unsupported VTIMEZONE %q offset or start", id)
			}
			_, seconds := fromTime.Zone()
			o := zoneObservance{kind: part.Name, offset: strings.NewReplacer("+", "-", "-", "+").Replace(offset.Value[:3]) + ":" + offset.Value[3:], wall: wall, start: wall.Add(-time.Duration(seconds) * time.Second)}
			if rule != nil {
				r, err := rrule.StrToROption(rule.Value)
				o.until = strings.Contains(rule.Value, ";UNTIL=")
				count := 2
				if o.until {
					count++
				}
				if err != nil || r.Freq != rrule.YEARLY || len(r.Bymonth) != 1 || len(r.Byweekday) != 1 || strings.Count(rule.Value, ";") != count || r.Bymonth[0] < 1 || r.Bymonth[0] > 12 || r.Byweekday[0].N() < -1 || r.Byweekday[0].N() == 0 || r.Byweekday[0].N() > 4 {
					return nil, fmt.Errorf("unsupported VTIMEZONE %q rule", id)
				}
				o.rule = fmt.Sprintf("M%d.%d.%d/%s", r.Bymonth[0], (r.Byweekday[0].N()+6)%6, (r.Byweekday[0].Day()+1)%7, wall.Format("15:04:05"))
			}
			obs = append(obs, o)
			for _, prop := range rdates {
				for value := range strings.SplitSeq(prop.Value, ",") {
					wall, err := time.Parse("20060102T150405", value)
					if err != nil {
						return nil, fmt.Errorf("unsupported VTIMEZONE %q offset or start", id)
					}
					obs = append(obs, zoneObservance{kind: o.kind, offset: o.offset, wall: wall, start: wall.Add(-time.Duration(seconds) * time.Second)})
				}
			}
		}
		slices.SortFunc(obs, func(a, b zoneObservance) int { return a.wall.Compare(b.wall) })
		for i, o := range obs {
			if o.until {
				replaced := false
				for _, later := range obs[i+1:] {
					if later.kind == o.kind {
						replaced = true
						break
					}
				}
				if !replaced {
					return nil, fmt.Errorf("unsupported VTIMEZONE %q final UNTIL", id)
				}
			}
		}
		active := map[string]zoneObservance{}
		var eras []zoneEra
		for i := 0; i < len(obs); {
			end := i + 1
			boundary := obs[i].start
			for end < len(obs) && obs[end].wall.Equal(obs[i].wall) {
				if obs[end].start.Before(boundary) {
					boundary = obs[end].start
				}
				end++
			}
			for _, o := range obs[i:end] {
				if o.rule == "" {
					clear(active)
				}
				active[o.kind] = o
			}
			std, hasStd := active["STANDARD"]
			dst, hasDst := active["DAYLIGHT"]
			var value string
			if hasStd && hasDst && std.offset != dst.offset && std.rule != "" && dst.rule != "" {
				value = "STD" + std.offset + "DST" + dst.offset + "," + dst.rule + "," + std.rule
			} else if hasStd {
				value = "STD" + std.offset
			} else if hasDst {
				value = "STD" + dst.offset
			}
			if !customZonePattern.MatchString(value) {
				return nil, fmt.Errorf("unsupported VTIMEZONE %q observances", id)
			}
			if len(eras) == 0 || eras[len(eras)-1].rule != value {
				eras = append(eras, zoneEra{boundary, value})
			}
			i = end
		}
		if len(eras) == 0 {
			return nil, fmt.Errorf("VTIMEZONE %q has no observances", id)
		}
		var value strings.Builder
		value.WriteString(eras[0].rule)
		for _, era := range eras[1:] {
			value.WriteString(";" + era.start.UTC().Format("20060102T150405Z") + "@" + era.rule)
		}
		zones[id] = value.String()
	}
	return zones, nil
}

func isCustomZone(name string) bool {
	parts := strings.Split(name, ";")
	if !customZonePattern.MatchString(parts[0]) {
		return false
	}
	for _, part := range parts[1:] {
		fields := strings.Split(part, "@")
		if len(fields) != 2 || !customZonePattern.MatchString(fields[1]) {
			return false
		}
		_, err := time.Parse("20060102T150405Z", fields[0])
		if err != nil {
			return false
		}
	}
	return true
}

const emptyTZifBlock = "TZif2" + "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00"

func footerZone(name string) (*time.Location, error) {
	data := []byte(emptyTZifBlock)
	data = append(data, data...)
	loc, err := time.LoadLocationFromTZData(name, append(data, []byte("\n"+name+"\n")...))
	if err != nil {
		return nil, fmt.Errorf("load footer zone: %w", err)
	}
	return loc, nil
}

// LoadZone reloads named zones and the embedded rules retained from an ICS invite.
func LoadZone(name string) (*time.Location, error) {
	loc, err := time.LoadLocation(name)
	if err != nil && isCustomZone(name) {
		parts := strings.Split(name, ";")
		if len(parts) == 1 {
			loc, err = footerZone(name)
		} else {
			loc, err = historyZone(name, parts)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("load time zone %q: %w", name, err)
	}
	return loc, nil
}

type zoneType struct {
	offset int
	dst    bool
}

func historyZone(name string, parts []string) (*time.Location, error) {
	eras := []zoneEra{{start: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), rule: parts[0]}}
	for _, p := range parts[1:] {
		fields := strings.Split(p, "@")
		at, _ := time.Parse("20060102T150405Z", fields[0])
		eras = append(eras, zoneEra{at, fields[1]})
	}
	var times []int64
	var indexes []byte
	var types []zoneType
	tooManyTypes := false
	add := func(at time.Time, loc *time.Location) {
		z := at.In(loc)
		_, offset := z.Zone()
		typ := zoneType{offset, z.IsDST()}
		idx := slices.Index(types, typ)
		if idx < 0 {
			types = append(types, typ)
			idx = len(types) - 1
		}
		if idx > math.MaxUint8 {
			tooManyTypes = true
			return
		}
		if len(times) > 0 && times[len(times)-1] == at.Unix() {
			indexes[len(indexes)-1] = byte(idx)
		} else {
			times = append(times, at.Unix())
			indexes = append(indexes, byte(idx))
		}
	}
	first := 0
	for first+1 < len(eras) && !eras[first+1].start.After(eras[0].start) {
		first++
	}
	for i := first; i < len(eras); i++ {
		era := eras[i]
		loc, err := footerZone(era.rule)
		if err != nil {
			return nil, err
		}
		start := era.start
		if start.Before(eras[0].start) {
			start = eras[0].start
		}
		add(start, loc)
		if i == len(eras)-1 {
			break
		}
		end := eras[i+1].start
		for cursor := start; cursor.Before(end); {
			next := calrecur.ZoneEnd(cursor.In(loc))
			if next.IsZero() || !next.Before(end) {
				break
			}
			add(next, loc)
			cursor = next
		}
	}
	if tooManyTypes {
		return nil, fmt.Errorf("too many time zone types: %d", len(types))
	}
	if uint64(len(times)) > math.MaxUint32 {
		return nil, fmt.Errorf("too many time zone transitions: %d", len(times))
	}
	var out bytes.Buffer
	header := func(n int) {
		out.WriteString("TZif2")
		out.Write(make([]byte, 15))
		for _, v := range []uint32{0, 0, 0, uint32(n), uint32(len(types)), 8} { //nolint:gosec // Counts are bounded above.
			_ = binary.Write(&out, binary.BigEndian, v)
		}
	}
	out.WriteString(emptyTZifBlock)
	header(len(times))
	for _, v := range times {
		_ = binary.Write(&out, binary.BigEndian, v)
	}
	out.Write(indexes)
	for _, typ := range types {
		_ = binary.Write(&out, binary.BigEndian, int32(typ.offset)) //nolint:gosec // The validated rule has a two-digit hour offset.
		if typ.dst {
			out.WriteByte(1)
			out.WriteByte(4)
		} else {
			out.WriteByte(0)
			out.WriteByte(0)
		}
	}
	out.WriteString("STD\x00DST\x00")
	out.WriteString("\n" + eras[len(eras)-1].rule + "\n")
	loc, err := time.LoadLocationFromTZData(name, out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("load history zone: %w", err)
	}
	return loc, nil
}
