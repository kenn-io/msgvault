package tiktokexport

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
)

// TXT exports use labelled fields. Content continues until the next Date or
// chat header; blank lines are preserved inside content, then trimmed at EOF.
func (a *archive) parseTXT(data []byte, kind string) error {
	a.recognized = true
	scanner := bufio.NewScanner(strings.NewReader(strings.ReplaceAll(string(data), "\r\n", "\n")))
	scanner.Buffer(make([]byte, 4096), int(maxFileBytes))
	chat := ""
	record := map[string]string{}
	last := ""
	flush := func() error {
		if len(record) == 0 {
			return nil
		}
		if kind == "dm" && chat == "" {
			return errors.New("TXT direct message is missing a Chat History with header")
		}
		o := object{}
		for k, v := range record {
			b, err := json.Marshal(strings.TrimRight(v, "\n"))
			if err != nil {
				return err
			}
			o[k] = jsontext.Value(b)
		}
		err := a.addRecord(o, chat, kind == "comment", "tiktok-export-txt")
		record = map[string]string{}
		last = ""
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if kind == "profile" {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			switch normalized(key) {
			case "username":
				u := username(value)
				if a.Username != "" && a.Username != u {
					return errors.New("conflicting TXT profile usernames")
				}
				a.Username = u
			case "nickname":
				a.Nickname = strings.TrimSpace(value)
			}
			continue
		}
		if partner, ok := chatPartner(line); ok && kind == "dm" {
			if err := flush(); err != nil {
				return err
			}
			chat = partner
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		name := normalized(key)
		if ok && (name == "date" || name == "from" || name == "content" || name == "comment" || name == "commentcontent" || name == "link" || name == "videolink" || name == "originalpostlandingpagelink") {
			if name == "date" {
				if err := flush(); err != nil {
					return err
				}
			}
			last = key
			record[key] = strings.TrimPrefix(value, " ")
		} else if last != "" {
			record[last] += "\n" + line
		} else if strings.TrimSpace(line) != "" {
			return errors.New("unrecognized TXT record header")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
