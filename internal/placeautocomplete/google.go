// Package placeautocomplete implements explicit, transient Google Places lookups.
// Its command shape is informed by openclaw/goplaces, MIT licensed; see
// third_party/goplaces/LICENSE.
package placeautocomplete

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const APIKeyEnv = "MSGVAULT_GOOGLE_PLACES_API_KEY"
const endpoint = "https://places.googleapis.com/v1"
const maxResponse = 1 << 20

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,36}$`)
var placeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type Bias struct {
	Latitude     float64
	Longitude    float64
	RadiusMeters float64
}
type Request struct {
	Input        string
	SessionToken string
	Language     string
	Region       string
	Limit        int
	Bias         *Bias
}
type Candidate struct {
	PlaceID string `json:"place_id"`
	Text    string `json:"text"`
}
type Suggestions struct {
	SessionToken string      `json:"session_token"`
	Attribution  string      `json:"attribution"`
	Candidates   []Candidate `json:"candidates"`
}
type Details struct {
	PlaceID           string         `json:"place_id"`
	FormattedAddress  string         `json:"formatted_address"`
	PostalAddress     jsontext.Value `json:"postal_address,omitzero"`
	AddressComponents jsontext.Value `json:"address_components,omitzero"`
	Attribution       string         `json:"attribution"`
}

type Client struct {
	Key     string
	HTTP    *http.Client
	baseURL string
}

func NewSessionToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("create session token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (c *Client) Autocomplete(ctx context.Context, input Request) (Suggestions, error) {
	if strings.TrimSpace(input.Input) == "" || !utf8.ValidString(input.Input) || utf8.RuneCountInString(input.Input) > 256 {
		return Suggestions{}, errors.New("input must contain 1 to 256 characters")
	}
	if input.Limit < 1 || input.Limit > 5 {
		return Suggestions{}, errors.New("limit must be between 1 and 5")
	}
	if !tokenPattern.MatchString(input.SessionToken) {
		return Suggestions{}, errors.New("session token must contain 1 to 36 letters, digits, underscores or hyphens")
	}
	if len(input.Language) > 35 || len(input.Region) > 2 {
		return Suggestions{}, errors.New("invalid language or region hint")
	}
	payload := map[string]any{"input": input.Input, "sessionToken": input.SessionToken}
	if input.Language != "" {
		payload["languageCode"] = input.Language
	}
	if input.Region != "" {
		payload["regionCode"] = input.Region
	}
	if input.Bias != nil {
		b := input.Bias
		if math.IsNaN(b.Latitude) || math.IsNaN(b.Longitude) || math.IsNaN(b.RadiusMeters) || b.Latitude < -90 || b.Latitude > 90 || b.Longitude < -180 || b.Longitude > 180 || b.RadiusMeters <= 0 || b.RadiusMeters > 50000 {
			return Suggestions{}, errors.New("location bias requires valid latitude, longitude and radius between 0 and 50000 metres")
		}
		payload["locationBias"] = map[string]any{"circle": map[string]any{"center": map[string]float64{"latitude": b.Latitude, "longitude": b.Longitude}, "radius": b.RadiusMeters}}
	}
	var response struct {
		Suggestions []struct {
			Prediction *struct {
				PlaceID string `json:"placeId"`
				Text    struct {
					Text string `json:"text"`
				} `json:"text"`
			} `json:"placePrediction"`
		} `json:"suggestions"`
	}
	if err := c.request(ctx, http.MethodPost, "/places:autocomplete", "", "suggestions.placePrediction.placeId,suggestions.placePrediction.text.text", payload, &response); err != nil {
		return Suggestions{}, err
	}
	out := Suggestions{SessionToken: input.SessionToken, Attribution: "Google Maps", Candidates: []Candidate{}}
	for _, item := range response.Suggestions {
		if item.Prediction != nil && placeIDPattern.MatchString(item.Prediction.PlaceID) && item.Prediction.Text.Text != "" {
			out.Candidates = append(out.Candidates, Candidate{PlaceID: item.Prediction.PlaceID, Text: item.Prediction.Text.Text})
			if len(out.Candidates) == input.Limit {
				break
			}
		}
	}
	return out, nil
}

func (c *Client) Select(ctx context.Context, placeID, sessionToken, language, region string) (Details, error) {
	if !placeIDPattern.MatchString(placeID) {
		return Details{}, errors.New("invalid place ID")
	}
	if !tokenPattern.MatchString(sessionToken) {
		return Details{}, errors.New("invalid session token")
	}
	if len(language) > 35 || len(region) > 2 {
		return Details{}, errors.New("invalid language or region hint")
	}
	query := url.Values{"sessionToken": {sessionToken}}
	if language != "" {
		query.Set("languageCode", language)
	}
	if region != "" {
		query.Set("regionCode", region)
	}
	var response struct {
		FormattedAddress  string         `json:"formattedAddress"`
		PostalAddress     jsontext.Value `json:"postalAddress"`
		AddressComponents jsontext.Value `json:"addressComponents"`
	}
	if err := c.request(ctx, http.MethodGet, "/places/"+placeID, query.Encode(), "formattedAddress,postalAddress,addressComponents", nil, &response); err != nil {
		return Details{}, err
	}
	return Details{PlaceID: placeID, FormattedAddress: response.FormattedAddress, PostalAddress: response.PostalAddress, AddressComponents: response.AddressComponents, Attribution: "Google Maps"}, nil
}

func (c *Client) request(ctx context.Context, method, path, query, mask string, body any, out any) error {
	if strings.TrimSpace(c.Key) == "" {
		return errors.New("Google Places key is not configured")
	}
	base := c.baseURL
	if base == "" {
		base = endpoint
	}
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("could not encode Places request")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	u := base + path
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(data))
	if err != nil {
		return errors.New("could not create Places request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", c.Key)
	req.Header.Set("X-Goog-FieldMask", mask)
	client := http.Client{}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Google Places request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Google Places request failed (HTTP %d)", resp.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Google Places response unavailable")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("invalid Google Places response")
	}
	return nil
}
