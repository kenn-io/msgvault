// Package caldav archives calendar resources through the shared DAV transport.
package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/carddav"
)

const calNS = "urn:ietf:params:xml:ns:caldav"
const maxResources = 10000

var errMissingCurrentUserPrincipal = errors.New("CalDAV current-user-principal is missing")

type Client struct {
	dav    *carddav.Client
	origin *url.URL
}
type Calendar struct {
	URL                            *url.URL
	DisplayName                    string
	SupportsSync, SupportsMultiget bool
}
type resource struct {
	Raw     []byte
	ETag    string
	Deleted bool
}
type plan struct {
	Resources map[string]resource
	Token     string
	Full      bool
}

type properties struct {
	ETag      string  `xml:"DAV: getetag"`
	Data      *string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
	Principal struct {
		Href string `xml:"DAV: href"`
	} `xml:"DAV: current-user-principal"`
	Homes struct {
		Hrefs []string `xml:"DAV: href"`
	} `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
	DisplayName string `xml:"DAV: displayname"`
	Types       *struct {
		Collection *struct{} `xml:"DAV: collection"`
		Calendar   *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
	} `xml:"DAV: resourcetype"`
	Reports struct {
		Items []struct {
			Report struct {
				Sync     *struct{} `xml:"DAV: sync-collection"`
				Multiget *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar-multiget"`
			} `xml:"DAV: report"`
		} `xml:"DAV: supported-report"`
	} `xml:"DAV: supported-report-set"`
}
type response struct {
	Href   string `xml:"DAV: href"`
	Status string `xml:"DAV: status"`
	Props  []struct {
		Properties properties `xml:"DAV: prop"`
		Status     string     `xml:"DAV: status"`
	} `xml:"DAV: propstat"`
}
type document struct {
	Responses    []response `xml:"DAV: response"`
	Token        string     `xml:"DAV: sync-token"`
	EffectiveURL *url.URL   `xml:"-"`
}

func NewClient(options carddav.ClientOptions) (*Client, error) {
	dav, err := carddav.NewClient(options)
	if err != nil {
		return nil, err
	}
	origin := *canonicalURL(options.CredentialOrigin)
	return &Client{dav: dav, origin: &origin}, nil
}
func status(raw string) (int, error) {
	fields := strings.Fields(raw)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0, errors.New("missing or invalid DAV status")
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("parse DAV status code: %w", err)
	}
	return code, nil
}
func escape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func propfind(names string) []byte {
	return []byte(`<D:propfind xmlns:D="DAV:" xmlns:C="` + calNS + `"><D:prop>` + names + `</D:prop></D:propfind>`)
}

func parseDocument(body []byte) (document, error) {
	if _, err := carddav.ParseMultiStatus(body, carddav.XMLLimits{}); err != nil {
		return document{}, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	depth, roots := 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return document{}, fmt.Errorf("parse CalDAV XML token: %w", err)
		}
		switch v := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			depth++
			switch v.Name.Local {
			case "calendar-data", "calendar-home-set", "calendar", "calendar-multiget":
				if v.Name.Space != calNS {
					return document{}, carddav.ErrUnsafeXML
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	if roots != 1 {
		return document{}, carddav.ErrUnsafeXML
	}
	var d document
	if err := xml.Unmarshal(body, &d); err != nil {
		return d, fmt.Errorf("decode CalDAV multistatus: %w", err)
	}
	return d, nil
}
func (c *Client) request(ctx context.Context, method string, target *url.URL, depth *int, body []byte, budget *carddav.Budget) (*carddav.Response, error) {
	return c.dav.DoWithBudget(ctx, carddav.Request{Method: method, URL: target.String(), Depth: depth, Body: body}, budget)
}
func (c *Client) report(ctx context.Context, method string, target *url.URL, depth int, body []byte, budget *carddav.Budget) (document, error) {
	r, err := c.request(ctx, method, target, &depth, body, budget)
	if err != nil {
		return document{}, err
	}
	if r.StatusCode != http.StatusMultiStatus {
		return document{}, fmt.Errorf("CalDAV expected multistatus, got %d", r.StatusCode)
	}
	d, err := parseDocument(r.Body)
	if err != nil {
		return document{}, err
	}
	d.EffectiveURL = r.EffectiveURL
	if d.EffectiveURL == nil {
		d.EffectiveURL = target
	}
	return d, nil
}

func (c *Client) Discover(ctx context.Context, entered string) ([]Calendar, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return c.discover(ctx, entered, carddav.NewBudget(256<<20))
}
func (c *Client) discover(ctx context.Context, entered string, budget *carddav.Budget) ([]Calendar, error) {
	base, err := url.Parse(entered)
	if err != nil {
		return nil, fmt.Errorf("parse CalDAV discovery URL: %w", err)
	}
	principalURL, err := c.principal(ctx, base, budget)
	if err != nil && !errors.Is(err, errMissingCurrentUserPrincipal) {
		var st *carddav.StatusError
		if !errors.As(err, &st) || (st.StatusCode != http.StatusNotFound && st.StatusCode != http.StatusMethodNotAllowed) {
			return nil, err
		}
	}
	if principalURL == nil {
		wellKnown := c.origin.ResolveReference(&url.URL{Path: "/.well-known/caldav"})
		principalURL, err = c.principal(ctx, wellKnown, budget)
		if err != nil && !errors.Is(err, errMissingCurrentUserPrincipal) {
			return nil, err
		}
	}
	if principalURL == nil {
		return nil, errMissingCurrentUserPrincipal
	}
	d, err := c.report(ctx, "PROPFIND", principalURL, 0, propfind(`<C:calendar-home-set/>`), budget)
	if err != nil {
		return nil, err
	}
	var calendars []Calendar
	seen := map[string]bool{}
	homes := map[string]bool{}
	for _, r := range d.Responses {
		for _, ps := range r.Props {
			code, err := status(ps.Status)
			if err != nil {
				return nil, err
			}
			if code != http.StatusOK {
				continue
			}
			for _, href := range ps.Properties.Homes.Hrefs {
				home, err := c.resolveResponseHref(responseBase(d, principalURL), strings.TrimSpace(href))
				if err != nil {
					return nil, fmt.Errorf("resolve CalDAV home URL: %w", err)
				}
				if homes[canonicalURL(home).String()] {
					continue
				}
				homes[canonicalURL(home).String()] = true
				children, err := c.report(ctx, "PROPFIND", home, 1, propfind(`<D:resourcetype/><D:displayname/><D:supported-report-set/>`), budget)
				if err != nil {
					return nil, err
				}
				effectiveHome := responseBase(children, home)
				foundHomeResponse := false
				for _, child := range children.Responses {
					href := strings.TrimSpace(child.Href)
					responseURL, err := c.resolveResponseHref(effectiveHome, href)
					if err != nil {
						return nil, fmt.Errorf("resolve CalDAV home response: %w", err)
					}
					isHome := sameCollectionURL(responseURL, effectiveHome)
					if child.Status != "" {
						code, err := status(child.Status)
						if err != nil {
							return nil, err
						}
						if code < 200 || code >= 300 {
							if isHome {
								return nil, fmt.Errorf("CalDAV home collection response failed with status %d", code)
							}
							return nil, fmt.Errorf("CalDAV calendar discovery response failed with status %d", code)
						}
					}
					if isHome {
						if foundHomeResponse {
							return nil, errors.New("CalDAV discovery returned duplicate home collection responses")
						}
					}
					homeCollection := false
					for _, p := range child.Props {
						code, err := status(p.Status)
						if err != nil {
							return nil, err
						}
						if isHome && code == http.StatusOK && p.Properties.Types != nil {
							if p.Properties.Types.Collection == nil {
								return nil, errors.New("CalDAV discovery returned an invalid home collection response")
							}
							homeCollection = true
						}
						if code != http.StatusOK || p.Properties.Types == nil || p.Properties.Types.Calendar == nil {
							continue
						}
						u, err := c.dav.ValidateChildHref(ctx, effectiveHome, href)
						if err != nil {
							return nil, err
						}
						if !strings.HasSuffix(u.Path, "/") {
							return nil, errors.New("CalDAV collection URL must end with slash")
						}
						if seen[canonicalURL(u).String()] {
							continue
						}
						seen[canonicalURL(u).String()] = true
						u = canonicalURL(u)
						cal := Calendar{URL: u, DisplayName: p.Properties.DisplayName}
						for _, report := range child.Props {
							for _, item := range report.Properties.Reports.Items {
								cal.SupportsSync = cal.SupportsSync || item.Report.Sync != nil
								cal.SupportsMultiget = cal.SupportsMultiget || item.Report.Multiget != nil
							}
						}
						calendars = append(calendars, cal)
						if len(calendars) > 1000 {
							return nil, errors.New("CalDAV calendar limit exceeded")
						}
					}
					if isHome {
						if !homeCollection {
							return nil, errors.New("CalDAV discovery returned an invalid home collection response")
						}
						foundHomeResponse = true
					}
				}
				if !foundHomeResponse {
					return nil, errors.New("CalDAV discovery did not include the home collection response")
				}
			}
		}
	}
	if len(homes) == 0 {
		return nil, errors.New("CalDAV calendar-home-set is missing")
	}
	return calendars, nil
}
func (c *Client) principal(ctx context.Context, u *url.URL, budget *carddav.Budget) (*url.URL, error) {
	d, err := c.report(ctx, "PROPFIND", u, 0, propfind(`<D:current-user-principal/>`), budget)
	if err != nil {
		return nil, err
	}
	for _, r := range d.Responses {
		for _, p := range r.Props {
			code, err := status(p.Status)
			if err != nil {
				return nil, err
			}
			if code == http.StatusOK && p.Properties.Principal.Href != "" {
				return c.resolveResponseHref(responseBase(d, u), strings.TrimSpace(p.Properties.Principal.Href))
			}
		}
	}
	return nil, errMissingCurrentUserPrincipal
}

func (c *Client) resolveResponseHref(base *url.URL, href string) (*url.URL, error) {
	if base == nil || href == "" {
		return nil, carddav.ErrUnsafeHref
	}
	resolved, err := base.Parse(href)
	if err != nil || resolved.User != nil || resolved.Fragment != "" || !sameOriginURL(c.origin, resolved) {
		return nil, carddav.ErrUnsafeHref
	}
	return resolved, nil
}

func responseBase(d document, fallback *url.URL) *url.URL {
	if d.EffectiveURL != nil {
		return d.EffectiveURL
	}
	return fallback
}

func (c *Client) pull(ctx context.Context, cal Calendar, token string, budget *carddav.Budget) (plan, error) {
	if !cal.SupportsSync {
		return c.snapshot(ctx, cal, budget)
	}
	p, err := c.syncReport(ctx, cal, token, budget)
	var st *carddav.StatusError
	if errors.As(err, &st) && token != "" && st.Precondition == "valid-sync-token" {
		return c.syncReport(ctx, cal, "", budget)
	}
	if errors.As(err, &st) && (st.StatusCode == http.StatusMethodNotAllowed || st.StatusCode == http.StatusNotImplemented) {
		return c.snapshot(ctx, cal, budget)
	}
	return p, err
}
func (c *Client) syncReport(ctx context.Context, cal Calendar, token string, budget *carddav.Budget) (plan, error) {
	body := []byte(`<D:sync-collection xmlns:D="DAV:"><D:sync-token>` + escape(token) + `</D:sync-token><D:sync-level>1</D:sync-level><D:prop><D:getetag/></D:prop></D:sync-collection>`)
	d, err := c.report(ctx, "REPORT", cal.URL, 0, body, budget)
	if err != nil {
		return plan{}, err
	}
	if strings.TrimSpace(d.Token) == "" {
		return plan{}, errors.New("CalDAV sync token missing")
	}
	return c.resources(ctx, cal, d, token == "", false, strings.TrimSpace(d.Token), budget)
}
func (c *Client) snapshot(ctx context.Context, cal Calendar, budget *carddav.Budget) (plan, error) {
	d, err := c.report(ctx, "PROPFIND", cal.URL, 1, propfind(`<D:getetag/><D:resourcetype/>`), budget)
	if err != nil {
		return plan{}, err
	}
	return c.resources(ctx, cal, d, true, true, "", budget)
}
func (c *Client) resources(ctx context.Context, cal Calendar, d document, full, requireCollection bool, token string, budget *carddav.Budget) (plan, error) {
	p := plan{Resources: map[string]resource{}, Token: token, Full: full}
	collection := responseBase(d, cal.URL)
	requestCalendar := cal
	requestCalendar.URL = collection
	collectionFound := false
	var hrefs []string
	for _, r := range d.Responses {
		u, err := c.dav.ValidateChildHref(ctx, collection, strings.TrimSpace(r.Href))
		if err != nil {
			return plan{}, err
		}
		u = canonicalURL(u)
		if sameCollectionURL(u, collection) {
			if collectionFound {
				return plan{}, errors.New("duplicate CalDAV collection response")
			}
			collectionFound = true
			if r.Status != "" {
				code, err := status(r.Status)
				if err != nil {
					return plan{}, err
				}
				if code != http.StatusOK {
					return plan{}, fmt.Errorf("CalDAV collection status %d", code)
				}
			}
			collectionTypeFound := false
			for _, ps := range r.Props {
				code, err := status(ps.Status)
				if err != nil {
					return plan{}, err
				}
				if code == http.StatusInsufficientStorage {
					return plan{}, errors.New("CalDAV collection report is incomplete")
				}
				if code == http.StatusOK && ps.Properties.Types != nil && ps.Properties.Types.Collection != nil {
					collectionTypeFound = true
				}
			}
			if requireCollection && !collectionTypeFound {
				return plan{}, errors.New("CalDAV snapshot collection response missing successful collection resource type")
			}
			continue
		}
		if _, exists := p.Resources[u.String()]; exists {
			return plan{}, errors.New("duplicate CalDAV resource response")
		}
		item := resource{}
		if r.Status != "" {
			code, err := status(r.Status)
			if err != nil {
				return plan{}, err
			}
			if code == http.StatusNotFound || code == http.StatusGone {
				item.Deleted = true
			} else if code != http.StatusOK {
				return plan{}, fmt.Errorf("CalDAV resource status %d", code)
			}
		}
		collection := false
		for _, ps := range r.Props {
			code, err := status(ps.Status)
			if err != nil {
				return plan{}, err
			}
			if code != http.StatusOK {
				return plan{}, fmt.Errorf("CalDAV resource property status %d", code)
			}
			if ps.Properties.ETag != "" {
				item.ETag = ps.Properties.ETag
			}
			collection = collection || ps.Properties.Types != nil && ps.Properties.Types.Collection != nil
		}
		if collection {
			continue
		}
		if !item.Deleted && item.ETag == "" {
			return plan{}, errors.New("CalDAV resource ETag missing")
		}
		p.Resources[u.String()] = item
		if !item.Deleted {
			hrefs = append(hrefs, u.String())
		}
		if len(p.Resources) > maxResources {
			return plan{}, errors.New("CalDAV resource limit exceeded")
		}
	}
	if requireCollection && !collectionFound {
		return plan{}, errors.New("CalDAV snapshot omitted collection response")
	}
	for start := 0; start < len(hrefs); start += 100 {
		batch := hrefs[start:min(start+100, len(hrefs))]
		if cal.SupportsMultiget {
			err := c.multiget(ctx, requestCalendar, batch, p.Resources, budget)
			var st *carddav.StatusError
			if err == nil {
				continue
			}
			if !errors.As(err, &st) || (st.StatusCode != http.StatusMethodNotAllowed && st.StatusCode != http.StatusNotImplemented) {
				return plan{}, err
			}
			cal.SupportsMultiget = false
		}
		for _, href := range batch {
			u, err := url.Parse(href)
			if err != nil {
				return plan{}, fmt.Errorf("parse CalDAV resource URL: %w", err)
			}
			r, err := c.request(ctx, http.MethodGet, u, nil, nil, budget)
			if err != nil {
				return plan{}, err
			}
			item := p.Resources[href]
			item.Raw = r.Body
			if tag := r.Header.Get("ETag"); tag != "" {
				item.ETag = tag
			}
			p.Resources[href] = item
		}
	}
	return p, nil
}
func (c *Client) multiget(ctx context.Context, cal Calendar, hrefs []string, items map[string]resource, budget *carddav.Budget) error {
	var body strings.Builder
	body.WriteString(`<C:calendar-multiget xmlns:D="DAV:" xmlns:C="` + calNS + `"><D:prop><D:getetag/><C:calendar-data/></D:prop>`)
	requested := map[string]bool{}
	for _, href := range hrefs {
		requested[href] = true
		body.WriteString(`<D:href>` + escape(href) + `</D:href>`)
	}
	body.WriteString(`</C:calendar-multiget>`)
	d, err := c.report(ctx, "REPORT", cal.URL, 1, []byte(body.String()), budget)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	collection := responseBase(d, cal.URL)
	for _, r := range d.Responses {
		u, err := c.dav.ValidateChildHref(ctx, collection, strings.TrimSpace(r.Href))
		if err != nil {
			return err
		}
		href := canonicalURL(u).String()
		if !requested[href] || seen[href] {
			return errors.New("unexpected or duplicate CalDAV multiget response")
		}
		seen[href] = true
		if r.Status != "" {
			code, err := status(r.Status)
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("CalDAV multiget status %d", code)
			}
		}
		item := items[href]
		hasData := false
		for _, ps := range r.Props {
			code, err := status(ps.Status)
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("CalDAV multiget property status %d", code)
			}
			if ps.Properties.Data != nil {
				if hasData {
					return errors.New("duplicate CalDAV calendar-data")
				}
				hasData = true
				item.Raw = []byte(*ps.Properties.Data)
			}
			if ps.Properties.ETag != "" {
				item.ETag = ps.Properties.ETag
			}
		}
		if !hasData || item.ETag == "" {
			return errors.New("incomplete CalDAV multiget resource")
		}
		items[href] = item
	}
	if len(seen) != len(requested) {
		return errors.New("incomplete CalDAV multiget response")
	}
	return nil
}
