package importantdate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/cronexpr"
)

// Config is opt-in and names a destination explicitly declared owner-private.
type Config struct {
	ConsentScope       string        `toml:"-"`
	ProjectUID         string        `toml:"-"`
	Enabled            bool          `toml:"enabled"`
	PrivateDestination bool          `toml:"private_destination"`
	Project            string        `toml:"project"`
	Recipient          string        `toml:"recipient"`
	Timezone           string        `toml:"timezone"`
	Schedule           string        `toml:"schedule"`
	LeapDay            LeapDayPolicy `toml:"leap_day"`
	UseCadencePresets  bool          `toml:"use_cadence_presets"`
	CustomTier         Tier          `toml:"custom_tier"`
	UnassignedTier     Tier          `toml:"unassigned_tier"`
	LeadA              int           `toml:"lead_a"`
	LeadB              int           `toml:"lead_b"`
	LeadC              int           `toml:"lead_c"`
	LeadD              int           `toml:"lead_d"`
}

func (c *Config) ApplyDefaults() {
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if c.Schedule == "" {
		c.Schedule = "15 8 * * *"
	}
	if c.LeapDay == "" {
		c.LeapDay = LeapFebruary28
	}
	if c.LeadA == 0 {
		c.LeadA = 30
	}
	if c.LeadB == 0 {
		c.LeadB = 21
	}
	if c.LeadC == 0 {
		c.LeadC = 14
	}
	if c.LeadD == 0 {
		c.LeadD = 7
	}
}

func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !c.PrivateDestination || strings.TrimSpace(c.Project) == "" || strings.TrimSpace(c.Recipient) == "" {
		return errors.New("important-date alerts require an explicit private destination, project and recipient")
	}
	if c.Timezone == "Local" {
		return errors.New("important-date alerts require a named IANA time zone")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("important-date time zone: %w", err)
	}
	if _, err := cronexpr.Parser.Parse(c.Schedule); err != nil {
		return fmt.Errorf("important-date schedule: %w", err)
	}
	if c.LeapDay != LeapFebruary28 && c.LeapDay != LeapMarch1 && c.LeapDay != LeapOnly {
		return errors.New("invalid important-date leap-day policy")
	}
	for _, tier := range []Tier{c.CustomTier, c.UnassignedTier} {
		if tier != "" && tier != TierA && tier != TierB && tier != TierC && tier != TierD {
			return errors.New("important-date custom and unassigned tiers must be empty or A, B, C, or D")
		}
	}
	for _, days := range []int{c.LeadA, c.LeadB, c.LeadC, c.LeadD} {
		if days < 1 || days > 366 {
			return errors.New("important-date lead days must be between 1 and 366")
		}
	}
	return nil
}

// DestinationScope binds consent to the exact configured endpoint and project.
// A change to either requires a new grant; credentials never enter the scope.
func DestinationScope(endpoint, project, projectUID string) string {
	if strings.TrimSpace(endpoint) == "" || strings.TrimSpace(project) == "" || strings.TrimSpace(projectUID) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("important-date-destination-v1\x00" + endpoint + "\x00" + project + "\x00" + projectUID))
	return hex.EncodeToString(digest[:])
}
