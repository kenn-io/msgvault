package carddav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ParseMultiStatus parses a bounded DAV multistatus document. Directives are
// forbidden so entities and external subsets cannot alter parser behavior.
func ParseMultiStatus(body []byte, limits XMLLimits) (MultiStatus, error) {
	defaults := DefaultXMLLimits()
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = defaults.MaxBytes
	}
	if limits.MaxDepth <= 0 {
		limits.MaxDepth = defaults.MaxDepth
	}
	if limits.MaxElements <= 0 {
		limits.MaxElements = defaults.MaxElements
	}
	if limits.MaxResponses <= 0 {
		limits.MaxResponses = defaults.MaxResponses
	}
	if limits.MaxPropStats <= 0 {
		limits.MaxPropStats = defaults.MaxPropStats
	}
	if int64(len(body)) > limits.MaxBytes {
		return MultiStatus{}, ErrXMLLimit
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	elements := 0
	responses := 0
	propStats := 0
	rootSeen := false
	unsafeNamespace := false
	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return MultiStatus{}, fmt.Errorf("parsing CardDAV XML: %w", err)
		}
		switch typed := token.(type) {
		case xml.Directive:
			return MultiStatus{}, ErrUnsafeXML
		case xml.StartElement:
			depth++
			elements++
			if typed.Name == (xml.Name{Space: davNamespace, Local: "response"}) {
				responses++
			}
			if typed.Name == (xml.Name{Space: davNamespace, Local: "propstat"}) {
				propStats++
			}
			if depth > limits.MaxDepth || elements > limits.MaxElements ||
				responses > limits.MaxResponses || propStats > limits.MaxPropStats {
				return MultiStatus{}, ErrXMLLimit
			}
			if err := validateDAVElement(typed.Name, !rootSeen); err != nil {
				unsafeNamespace = true
			}
			rootSeen = true
		case xml.EndElement:
			depth--
		}
	}
	if unsafeNamespace {
		return MultiStatus{}, ErrUnsafeXML
	}
	var raw rawMultiStatus
	if err := xml.Unmarshal(body, &raw); err != nil {
		return MultiStatus{}, fmt.Errorf("unmarshaling CardDAV XML: %w", err)
	}
	if raw.XMLName.Space != davNamespace || raw.XMLName.Local != "multistatus" {
		return MultiStatus{}, fmt.Errorf("multistatus root: %w", ErrUnsafeXML)
	}
	result := MultiStatus{SyncToken: strings.TrimSpace(raw.SyncToken)}
	for _, response := range raw.Responses {
		responseStatus, _, err := parseHTTPStatus(response.Status)
		if err != nil {
			return MultiStatus{}, fmt.Errorf("response status: %w", err)
		}
		parsed := MultiStatusResponse{Href: strings.TrimSpace(response.Href), StatusCode: responseStatus}
		for _, propStat := range response.PropStats {
			propStatus, present, err := parseHTTPStatus(propStat.Status)
			if err != nil {
				return MultiStatus{}, fmt.Errorf("propstat status: %w", err)
			}
			if !present {
				return MultiStatus{}, errors.New("propstat status is missing")
			}
			addressData, err := decodeXMLText(propStat.Prop.AddressData.InnerXML)
			if err != nil {
				return MultiStatus{}, fmt.Errorf("decode CardDAV address-data: %w", err)
			}
			properties := Properties{
				GetETag: strings.TrimSpace(propStat.Prop.GetETag), AddressData: addressData,
				SyncToken: strings.TrimSpace(propStat.Prop.SyncToken), DisplayName: strings.TrimSpace(propStat.Prop.DisplayName),
				CurrentUserPrincipal: strings.TrimSpace(propStat.Prop.CurrentUserPrincipal.Href),
				AddressbookHomeSet:   trimmedNonempty(propStat.Prop.AddressbookHomeSet.Hrefs),
			}
			if propStat.Prop.ResourceType != nil {
				properties.ResourceTypePresent = true
				properties.IsCollection = propStat.Prop.ResourceType.Collection != nil
				properties.IsAddressBook = propStat.Prop.ResourceType.AddressBook != nil
			}
			if propStat.Prop.Privileges != nil {
				properties.PrivilegesPresent = true
				for _, privilege := range propStat.Prop.Privileges.Privileges {
					if privilege.Name.Space == davNamespace {
						properties.Privileges = append(properties.Privileges, privilege.Name.Local)
					}
				}
			}
			if propStat.Prop.SupportedReports != nil {
				for _, supported := range propStat.Prop.SupportedReports.Reports {
					switch supported.Report.Name {
					case (xml.Name{Space: davNamespace, Local: "sync-collection"}):
						properties.SupportsSync = true
					case (xml.Name{Space: cardDAVNamespace, Local: "addressbook-multiget"}):
						properties.SupportsMultiget = true
					}
				}
			}
			if propStat.Prop.SupportedAddressData != nil {
				for _, dataType := range propStat.Prop.SupportedAddressData.Types {
					contentType := strings.ToLower(strings.TrimSpace(dataType.ContentType))
					if contentType == "" {
						contentType = "text/vcard"
					}
					if contentType != "text/vcard" && contentType != "text/x-vcard" {
						continue
					}
					version := strings.TrimSpace(dataType.Version)
					if version == "" {
						version = "3.0"
					}
					if !slices.Contains(properties.SupportedVCard, version) {
						properties.SupportedVCard = append(properties.SupportedVCard, version)
					}
				}
			}
			parsed.PropStats = append(parsed.PropStats, PropStat{
				StatusCode: propStatus,
				Properties: properties,
			})
		}
		result.Responses = append(result.Responses, parsed)
	}
	return result, nil
}

func validateDAVElement(name xml.Name, root bool) error {
	if root && (name.Space != davNamespace || name.Local != "multistatus") {
		return ErrUnsafeXML
	}
	expectedNamespaces := map[string]string{
		"multistatus":                davNamespace,
		"response":                   davNamespace,
		"href":                       davNamespace,
		"status":                     davNamespace,
		"propstat":                   davNamespace,
		"prop":                       davNamespace,
		"getetag":                    davNamespace,
		"sync-token":                 davNamespace,
		"displayname":                davNamespace,
		"current-user-principal":     davNamespace,
		"address-data":               cardDAVNamespace,
		"addressbook-home-set":       cardDAVNamespace,
		"resourcetype":               davNamespace,
		"collection":                 davNamespace,
		"addressbook":                cardDAVNamespace,
		"supported-report-set":       davNamespace,
		"supported-report":           davNamespace,
		"report":                     davNamespace,
		"sync-collection":            davNamespace,
		"addressbook-multiget":       cardDAVNamespace,
		"current-user-privilege-set": davNamespace,
		"privilege":                  davNamespace,
		"all":                        davNamespace,
		"write":                      davNamespace,
		"bind":                       davNamespace,
		"write-content":              davNamespace,
		"unbind":                     davNamespace,
		"supported-address-data":     cardDAVNamespace,
		"address-data-type":          cardDAVNamespace,
	}
	if expected, known := expectedNamespaces[name.Local]; known && name.Space != expected {
		return ErrUnsafeXML
	}
	return nil
}

type rawMultiStatus struct {
	XMLName   xml.Name      `xml:"multistatus"`
	Responses []rawResponse `xml:"response"`
	SyncToken string        `xml:"sync-token"`
}

type rawResponse struct {
	Href      string        `xml:"href"`
	Status    string        `xml:"status"`
	PropStats []rawPropStat `xml:"propstat"`
}

type rawPropStat struct {
	Prop   rawProperties `xml:"prop"`
	Status string        `xml:"status"`
}

type rawProperties struct {
	GetETag              string                   `xml:"getetag"`
	AddressData          rawAddressData           `xml:"address-data"`
	SyncToken            string                   `xml:"sync-token"`
	DisplayName          string                   `xml:"displayname"`
	CurrentUserPrincipal rawHref                  `xml:"current-user-principal"`
	AddressbookHomeSet   rawHrefs                 `xml:"addressbook-home-set"`
	ResourceType         *rawResourceType         `xml:"resourcetype"`
	SupportedReports     *rawSupportedReports     `xml:"supported-report-set"`
	Privileges           *rawPrivileges           `xml:"current-user-privilege-set"`
	SupportedAddressData *rawSupportedAddressData `xml:"supported-address-data"`
}

type rawAddressData struct {
	InnerXML []byte `xml:",innerxml"`
}

type rawHrefs struct {
	Hrefs []string `xml:"href"`
}

func trimmedNonempty(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func decodeXMLText(raw []byte) (string, error) {
	decoded := make([]byte, 0, len(raw))
	for offset := 0; offset < len(raw); {
		switch raw[offset] {
		case '<':
			const opener = "<![CDATA["
			if !bytes.HasPrefix(raw[offset:], []byte(opener)) {
				return "", ErrUnsafeXML
			}
			contentStart := offset + len(opener)
			contentEnd := bytes.Index(raw[contentStart:], []byte("]]>"))
			if contentEnd < 0 {
				return "", ErrUnsafeXML
			}
			contentEnd += contentStart
			decoded = append(decoded, raw[contentStart:contentEnd]...)
			offset = contentEnd + len("]]>")
		case '&':
			end := bytes.IndexByte(raw[offset+1:], ';')
			if end < 0 {
				return "", ErrUnsafeXML
			}
			end += offset + 1
			entity, err := decodeXMLEntity(raw[offset+1 : end])
			if err != nil {
				return "", err
			}
			decoded = append(decoded, entity...)
			offset = end + 1
		default:
			decoded = append(decoded, raw[offset])
			offset++
		}
	}
	return string(decoded), nil
}

func decodeXMLEntity(entity []byte) ([]byte, error) {
	switch string(entity) {
	case "amp":
		return []byte{'&'}, nil
	case "lt":
		return []byte{'<'}, nil
	case "gt":
		return []byte{'>'}, nil
	case "apos":
		return []byte{'\''}, nil
	case "quot":
		return []byte{'"'}, nil
	}
	base := 10
	if len(entity) <= 1 || entity[0] != '#' {
		return nil, ErrUnsafeXML
	}
	digits := entity[1:]
	if len(digits) > 1 && (digits[0] == 'x' || digits[0] == 'X') {
		base, digits = 16, digits[1:]
	}
	if len(digits) == 0 {
		return nil, ErrUnsafeXML
	}
	value, err := strconv.ParseUint(string(digits), base, 32)
	if err != nil || value > utf8.MaxRune {
		return nil, ErrUnsafeXML
	}
	decoded := rune(value) // #nosec G115 -- value is explicitly bounded to utf8.MaxRune above.
	if !validXMLRune(decoded) {
		return nil, ErrUnsafeXML
	}
	return []byte(string(decoded)), nil
}

func validXMLRune(value rune) bool {
	return value == '\t' || value == '\n' || value == '\r' ||
		value >= 0x20 && value <= 0xd7ff ||
		value >= 0xe000 && value <= 0xfffd ||
		value >= 0x10000 && value <= 0x10ffff
}

type rawResourceType struct {
	Collection  *struct{} `xml:"collection"`
	AddressBook *struct{} `xml:"addressbook"`
}

type rawPrivileges struct {
	Privileges []rawPrivilege `xml:"privilege"`
}

type rawPrivilege struct {
	Name xml.Name `xml:",any"`
}

type rawSupportedReports struct {
	Reports []rawSupportedReport `xml:"supported-report"`
}

type rawSupportedReport struct {
	Report rawReport `xml:"report"`
}

type rawReport struct {
	Name xml.Name `xml:",any"`
}

type rawSupportedAddressData struct {
	Types []rawAddressDataType `xml:"address-data-type"`
}

type rawAddressDataType struct {
	ContentType string `xml:"content-type,attr"`
	Version     string `xml:"version,attr"`
}

type rawHref struct {
	Href string `xml:"href"`
}

var davHTTPStatusPattern = regexp.MustCompile(`^HTTP/[0-9]\.[0-9] ([1-5][0-9]{2})(?: .*)?$`)

func parseHTTPStatus(value string) (int, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false, nil
	}
	match := davHTTPStatusPattern.FindStringSubmatch(value)
	if match == nil {
		return 0, true, fmt.Errorf("invalid HTTP/DAV status line %q", value)
	}
	status, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, true, fmt.Errorf("invalid HTTP/DAV status code: %w", err)
	}
	return status, true, nil
}

// PropfindBody builds a namespace-correct DAV PROPFIND body.
func PropfindBody(properties []PropertyName) ([]byte, error) {
	return propertyBody(xml.Name{Space: davNamespace, Local: "propfind"}, properties, nil)
}

// AddressbookQueryBody builds an addressbook-query REPORT body.
func AddressbookQueryBody(properties []PropertyName) ([]byte, error) {
	return propertyBody(xml.Name{Space: cardDAVNamespace, Local: "addressbook-query"}, properties, func(encoder *xml.Encoder) error {
		return encodeEmptyElement(encoder, xml.Name{Space: cardDAVNamespace, Local: "filter"})
	})
}

// AddressbookMultigetBody builds an addressbook-multiget REPORT body.
func AddressbookMultigetBody(properties []PropertyName, hrefs []string) ([]byte, error) {
	return propertyBody(xml.Name{Space: cardDAVNamespace, Local: "addressbook-multiget"}, properties, func(encoder *xml.Encoder) error {
		for _, href := range hrefs {
			start := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "href"}}
			if err := encoder.EncodeToken(start); err != nil {
				return fmt.Errorf("encode CardDAV multiget href: %w", err)
			}
			if err := encoder.EncodeToken(xml.CharData(href)); err != nil {
				return fmt.Errorf("encode CardDAV multiget href value: %w", err)
			}
			if err := encoder.EncodeToken(start.End()); err != nil {
				return fmt.Errorf("close CardDAV multiget href: %w", err)
			}
		}
		return nil
	})
}

func propertyBody(root xml.Name, properties []PropertyName, extra func(*xml.Encoder) error) ([]byte, error) {
	var body bytes.Buffer
	encoder := xml.NewEncoder(&body)
	if err := encoder.EncodeToken(xml.StartElement{Name: root}); err != nil {
		return nil, fmt.Errorf("encode CardDAV XML root: %w", err)
	}
	prop := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "prop"}}
	if err := encoder.EncodeToken(prop); err != nil {
		return nil, fmt.Errorf("encode CardDAV XML properties: %w", err)
	}
	for _, property := range properties {
		if err := validatePropertyName(property); err != nil {
			return nil, err
		}
		if err := encodeEmptyElement(encoder, xml.Name{Space: property.Namespace, Local: property.Local}); err != nil {
			return nil, err
		}
	}
	if err := encoder.EncodeToken(prop.End()); err != nil {
		return nil, fmt.Errorf("close CardDAV XML properties: %w", err)
	}
	if extra != nil {
		if err := extra(encoder); err != nil {
			return nil, err
		}
	}
	if err := encoder.EncodeToken(xml.EndElement{Name: root}); err != nil {
		return nil, fmt.Errorf("close CardDAV XML root: %w", err)
	}
	if err := encoder.Flush(); err != nil {
		return nil, fmt.Errorf("flush CardDAV XML: %w", err)
	}
	return body.Bytes(), nil
}

func encodeEmptyElement(encoder *xml.Encoder, name xml.Name) error {
	start := xml.StartElement{Name: name}
	if err := encoder.EncodeToken(start); err != nil {
		return fmt.Errorf("encode empty CardDAV XML element: %w", err)
	}
	if err := encoder.EncodeToken(start.End()); err != nil {
		return fmt.Errorf("close empty CardDAV XML element: %w", err)
	}
	return nil
}

func validatePropertyName(property PropertyName) error {
	if property.Namespace != davNamespace && property.Namespace != cardDAVNamespace {
		return ErrInvalidProperty
	}
	if property.Local == "" {
		return ErrInvalidProperty
	}
	for index, character := range property.Local {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' ||
			(index > 0 && ((character >= '0' && character <= '9') || character == '.' || character == '-')) {
			continue
		}
		return ErrInvalidProperty
	}
	return nil
}
