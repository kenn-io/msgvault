package synctechsms

import (
	"database/sql"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

func Parse(r io.Reader) (*Document, error) {
	dec := xml.NewDecoder(r)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty SMS Backup & Restore XML")
		}
		if err != nil {
			return nil, fmt.Errorf("read XML root: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "smses":
			return parseMessages(dec, start)
		case "calls":
			return parseCalls(dec, start)
		default:
			return nil, fmt.Errorf("unsupported SMS Backup & Restore XML root %q", start.Name.Local)
		}
	}
}

func ParseEach(r io.Reader, handle func(Record) error) error {
	dec := xml.NewDecoder(r)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return errors.New("empty SMS Backup & Restore XML")
		}
		if err != nil {
			return fmt.Errorf("read XML root: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "smses":
			return parseEachMessage(dec, start, handle)
		case "calls":
			return parseEachCall(dec, start, handle)
		default:
			return fmt.Errorf("unsupported SMS Backup & Restore XML root %q", start.Name.Local)
		}
	}
}

func parseEachMessage(dec *xml.Decoder, root xml.StartElement, handle func(Record) error) error {
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read messages XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "sms":
				sms := parseSMS(t)
				if err := handle(Record{Kind: RecordSMS, SMS: &sms}); err != nil {
					return err
				}
			case "mms":
				mms, err := parseMMS(dec, t)
				if err != nil {
					return err
				}
				if err := handle(Record{Kind: RecordMMS, MMS: &mms}); err != nil {
					return err
				}
			}
		case xml.EndElement:
			if t.Name.Local == root.Name.Local {
				return nil
			}
		}
	}
}

func parseEachCall(dec *xml.Decoder, root xml.StartElement, handle func(Record) error) error {
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read calls XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "call" {
				call := parseCallElement(t)
				if err := handle(Record{Kind: RecordCall, Call: &call}); err != nil {
					return err
				}
			}
		case xml.EndElement:
			if t.Name.Local == root.Name.Local {
				return nil
			}
		}
	}
}

func parseMessages(dec *xml.Decoder, root xml.StartElement) (*Document, error) {
	doc := &Document{Kind: KindMessages, Count: atoi(attr(root, "count")), BackupSet: attr(root, "backup_set")}
	doc.BackupDate = unixMillis(attr(root, "backup_date"))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return doc, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read messages XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "sms":
				doc.SMS = append(doc.SMS, parseSMS(t))
			case "mms":
				m, err := parseMMS(dec, t)
				if err != nil {
					return nil, err
				}
				doc.MMS = append(doc.MMS, m)
			}
		case xml.EndElement:
			if t.Name.Local == root.Name.Local {
				return doc, nil
			}
		}
	}
}

func parseSMS(start xml.StartElement) SMS {
	return SMS{
		Protocol:      attr(start, "protocol"),
		Address:       attr(start, "address"),
		Timestamp:     unixMillis(attr(start, "date")),
		Type:          SMSType(atoi(attr(start, "type"))),
		Subject:       nullString(attr(start, "subject")),
		Body:          attr(start, "body"),
		ServiceCenter: nullString(attr(start, "service_center")),
		Read:          attr(start, "read") == "1",
		Status:        atoi(attr(start, "status")),
		SubID:         nullString(attr(start, "sub_id")),
		ContactName:   nullString(attr(start, "contact_name")),
		RawAttrs:      attrs(start),
	}
}

func parseMMS(dec *xml.Decoder, start xml.StartElement) (MMS, error) {
	m := MMS{
		Timestamp:   unixMillis(attr(start, "date")),
		MessageBox:  MMSBox(atoi(attr(start, "msg_box"))),
		Address:     nullString(attr(start, "address")),
		MessageID:   nullString(attr(start, "m_id")),
		Subject:     nullString(attr(start, "sub")),
		ContentType: nullString(attr(start, "ct_t")),
		Read:        attr(start, "read") == "1",
		ContactName: nullString(attr(start, "contact_name")),
		RawAttrs:    attrs(start),
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return m, fmt.Errorf("read MMS XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "part":
				part, err := parseMMSPart(t)
				if err != nil {
					return m, err
				}
				m.Parts = append(m.Parts, part)
			case "addr":
				m.Addresses = append(m.Addresses, MMSAddress{
					Address: attr(t, "address"),
					Type:    MMSAddressType(atoi(attr(t, "type"))),
					Charset: nullString(attr(t, "charset")),
				})
			}
		case xml.EndElement:
			if t.Name.Local == "mms" {
				return m, nil
			}
		}
	}
}

func parseMMSPart(start xml.StartElement) (MMSPart, error) {
	p := MMSPart{
		Sequence:    atoi(attr(start, "seq")),
		ContentType: attr(start, "ct"),
		Name:        nullString(attr(start, "name")),
		Filename:    nullString(attr(start, "cl")),
		Charset:     nullString(attr(start, "chset")),
		Text:        nullString(attr(start, "text")),
		RawAttrs:    attrs(start),
	}
	if encoded := attr(start, "data"); encoded != "" && encoded != "null" {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return p, fmt.Errorf("decode MMS part data: %w", err)
		}
		p.Data = data
	}
	return p, nil
}

func parseCalls(dec *xml.Decoder, root xml.StartElement) (*Document, error) {
	doc := &Document{Kind: KindCalls, Count: atoi(attr(root, "count")), BackupSet: attr(root, "backup_set")}
	doc.BackupDate = unixMillis(attr(root, "backup_date"))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return doc, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read calls XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "call" {
				doc.Calls = append(doc.Calls, parseCallElement(t))
			}
		case xml.EndElement:
			if t.Name.Local == root.Name.Local {
				return doc, nil
			}
		}
	}
}

func parseCallElement(start xml.StartElement) Call {
	return Call{
		Number:          attr(start, "number"),
		DurationSeconds: atoi(attr(start, "duration")),
		Timestamp:       unixMillis(attr(start, "date")),
		Type:            CallType(atoi(attr(start, "type"))),
		Presentation:    atoi(attr(start, "presentation")),
		SubscriptionID:  nullString(attr(start, "subscription_id")),
		ContactName:     nullString(attr(start, "contact_name")),
		RawAttrs:        attrs(start),
	}
}

func attr(start xml.StartElement, name string) string {
	for _, a := range start.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func attrs(start xml.StartElement) map[string]string {
	out := make(map[string]string, len(start.Attr))
	for _, a := range start.Attr {
		out[a.Name.Local] = a.Value
	}
	return out
}

func atoi(s string) int {
	if s == "" || s == "null" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

func unixMillis(s string) time.Time {
	if s == "" || s == "null" {
		return time.Time{}
	}
	ms, _ := strconv.ParseInt(s, 10, 64)
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func nullString(s string) sql.NullString {
	if strings.TrimSpace(s) == "" || s == "null" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
