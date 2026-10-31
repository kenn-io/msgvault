package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// XConfig selects provider policy by the verified numeric account identity.
type XConfig struct {
	Accounts []XAccountConfig `toml:"accounts"`
	Sends    []XSendConfig    `toml:"sends"`
}

// XSendConfig opts a verified numeric owner into explicit reply sending.
// Local source row IDs are deliberately not selectors: they can be reused.
type XSendConfig struct {
	UserID  string `toml:"user_id"`
	Enabled bool   `toml:"enabled"`
}

// XAccountConfig holds an account's local spending estimates and send opt-in.
// Monetary fields are integer USD micro-units, never floating-point dollars.
type XAccountConfig struct {
	UserID               string        `toml:"user_id"`
	MonthlyBudgetMicros  int64         `toml:"-"`
	OwnedReads           bool          `toml:"owned_reads"`
	AllowSend            bool          `toml:"-"`
	RefreshBatchSize     int           `toml:"refresh_batch_size"`
	RefreshSweepInterval time.Duration `toml:"refresh_sweep_interval"`
	// PostReadPriceOverride records an explicit standard-price estimate. When
	// false, eligible owned user endpoints may use the separate owned rate.
	// Explicit prices take precedence even on qualifying owned endpoints.
	PostReadPriceOverride   bool  `toml:"-"`
	PostReadPriceMicros     int64 `toml:"-"`
	UserReadPriceMicros     int64 `toml:"-"`
	CreateReservationMicros int64 `toml:"-"`
}

func defaultXAccount(userID string) XAccountConfig {
	return XAccountConfig{
		UserID: userID, MonthlyBudgetMicros: 5_000_000,
		RefreshBatchSize: 100, RefreshSweepInterval: 24 * time.Hour,
		PostReadPriceMicros: 5_000, UserReadPriceMicros: 10_000,
		CreateReservationMicros: 200_000,
	}
}

// XAccount returns the selected policy, or conservative defaults for a verified
// owner with no override. It never applies another account's opt-ins.
func (c *Config) XAccount(userID string) (XAccountConfig, error) {
	if err := validateXUserID(userID); err != nil {
		return XAccountConfig{}, err
	}
	if err := c.X.Validate(); err != nil {
		return XAccountConfig{}, err
	}
	policy := defaultXAccount(userID)
	for _, account := range c.X.Accounts {
		if account.UserID == userID {
			policy = account
			break
		}
	}
	policy.AllowSend = false
	for _, send := range c.X.Sends {
		if send.UserID == userID {
			policy.AllowSend = send.Enabled
			break
		}
	}
	return policy, nil
}

// Validate rejects ambiguous selectors and invalid policies, including configs
// assembled directly by callers rather than loaded from TOML.
func (x XConfig) Validate() error {
	sendOwners := make(map[string]bool, len(x.Sends))
	for _, send := range x.Sends {
		if err := validateXUserID(send.UserID); err != nil {
			return fmt.Errorf("x.sends: %w", err)
		}
		if sendOwners[send.UserID] {
			return fmt.Errorf("duplicate x.sends user_id %q", send.UserID)
		}
		sendOwners[send.UserID] = true
	}
	seen := make(map[string]bool, len(x.Accounts))
	for _, account := range x.Accounts {
		if seen[account.UserID] {
			return fmt.Errorf("duplicate x.accounts user_id %q", account.UserID)
		}
		if err := account.Validate(account.UserID); err != nil {
			return err
		}
		seen[account.UserID] = true
	}
	return nil
}

// Validate binds this policy to its verified source owner before use.
func (a XAccountConfig) Validate(userID string) error {
	if err := validateXUserID(userID); err != nil {
		return err
	}
	if a.UserID != userID {
		return errors.New("x.accounts user_id does not match verified source owner")
	}
	if a.MonthlyBudgetMicros < 0 || a.PostReadPriceMicros < 0 || a.UserReadPriceMicros < 0 {
		return errors.New("x.accounts monetary values must be nonnegative")
	}
	if a.CreateReservationMicros < 200_000 {
		return errors.New("x.accounts create_reservation must be at least 0.200 USD")
	}
	if a.RefreshBatchSize < 1 || a.RefreshBatchSize > 100 {
		return errors.New("x.accounts refresh_batch_size must be between 1 and 100")
	}
	if a.RefreshSweepInterval <= 0 {
		return errors.New("x.accounts refresh_sweep_interval must be positive")
	}
	return nil
}

func validateXUserID(userID string) error {
	if len(userID) == 0 || len(userID) > 128 || userID[0] < '1' || userID[0] > '9' {
		return errors.New("x.accounts user_id must be a positive canonical decimal ID of at most 128 digits")
	}
	for i := range userID {
		if userID[i] < '0' || userID[i] > '9' {
			return errors.New("x.accounts user_id must contain only ASCII decimal digits")
		}
	}
	return nil
}

// ParseUSDMicros parses decimal USD, optionally using a decimal exponent. It
// rejects negative, nonfinite, overflowing and fractional-micro-unit amounts.
func ParseUSDMicros(value string) (int64, error) {
	invalid := errors.New("USD amount must be a nonnegative decimal in whole micro-units within int64")
	if value == "" {
		return 0, invalid
	}
	if value[0] == '+' {
		value = value[1:]
	}
	// Bound exponents without constructing large powers of ten.
	if len(value) == 0 || value[0] < '0' || value[0] > '9' {
		return 0, invalid
	}
	mantissa, expText, hasExp := strings.Cut(strings.ToLower(value), "e")
	exponent := int64(0)
	if hasExp {
		var err error
		exponent, err = strconv.ParseInt(expText, 10, 32)
		if err != nil {
			return 0, invalid
		}
	}
	whole, fraction, hasDot := strings.Cut(mantissa, ".")
	if whole == "" || (hasDot && fraction == "") {
		return 0, invalid
	}
	digits := whole + fraction
	for i := range digits {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, invalid
		}
	}
	scale := int64(6) - int64(len(fraction)) + exponent
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return 0, nil
	}
	if scale < 0 {
		drop := -scale
		if drop > int64(len(digits)) {
			return 0, invalid
		}
		for _, digit := range digits[len(digits)-int(drop):] {
			if digit != '0' {
				return 0, invalid
			}
		}
		digits = digits[:len(digits)-int(drop)]
		scale = 0
	}
	if int64(len(digits))+scale > 19 {
		return 0, invalid
	}
	var micros int64
	for _, digit := range digits {
		next := int64(digit - '0')
		if micros > (math.MaxInt64-next)/10 {
			return 0, invalid
		}
		micros = micros*10 + next
	}
	for ; scale > 0; scale-- {
		if micros > math.MaxInt64/10 {
			return 0, invalid
		}
		micros *= 10
	}
	return micros, nil
}

// xAccountTOML keeps omission distinct from zero. Monetary literals are quoted
// in a second, X-only decode so the TOML library cannot round them to float64.
type xAccountTOML struct {
	UserID               string         `toml:"user_id"`
	MonthlyBudget        *string        `toml:"monthly_budget"`
	OwnedReads           bool           `toml:"owned_reads"`
	AllowSend            bool           `toml:"allow_send"`
	RefreshBatchSize     *int           `toml:"refresh_batch_size"`
	RefreshSweepInterval *time.Duration `toml:"refresh_sweep_interval"`
	PostReadPrice        *string        `toml:"post_read_price"`
	UserReadPrice        *string        `toml:"user_read_price"`
	CreateReservation    *string        `toml:"create_reservation"`
}

func decodeXConfig(content []byte) (XConfig, error) {
	var raw struct {
		X struct {
			Accounts []xAccountTOML `toml:"accounts"`
			Sends    []XSendConfig  `toml:"sends"`
		} `toml:"x"`
	}
	metadata, err := toml.Decode(quoteXMonetaryLiterals(string(content)), &raw)
	if err != nil {
		return XConfig{}, fmt.Errorf("decode X policy: %w", err)
	}
	for _, key := range metadata.Undecoded() {
		if len(key) > 0 && key[0] == "x" {
			return XConfig{}, fmt.Errorf("unknown X config key %q", key.String())
		}
	}
	result := XConfig{Sends: raw.X.Sends}
	for _, account := range raw.X.Accounts {
		policy := defaultXAccount(account.UserID)
		policy.OwnedReads = account.OwnedReads
		if account.AllowSend {
			return XConfig{}, errors.New("x.accounts allow_send=true is obsolete; use [[x.sends]] with user_id and enabled=true")
		}
		policy.PostReadPriceOverride = account.PostReadPrice != nil
		if account.RefreshBatchSize != nil {
			policy.RefreshBatchSize = *account.RefreshBatchSize
		}
		if account.RefreshSweepInterval != nil {
			policy.RefreshSweepInterval = *account.RefreshSweepInterval
		}
		for _, money := range []struct {
			name string
			raw  *string
			dst  *int64
		}{
			{"monthly_budget", account.MonthlyBudget, &policy.MonthlyBudgetMicros},
			{"post_read_price", account.PostReadPrice, &policy.PostReadPriceMicros},
			{"user_read_price", account.UserReadPrice, &policy.UserReadPriceMicros},
			{"create_reservation", account.CreateReservation, &policy.CreateReservationMicros},
		} {
			if money.raw == nil {
				continue
			}
			value, err := ParseUSDMicros(*money.raw)
			if err != nil {
				return XConfig{}, fmt.Errorf("x.accounts %s: %w", money.name, err)
			}
			*money.dst = value
		}
		result.Accounts = append(result.Accounts, policy)
	}
	return result, result.Validate()
}

// quoteXMonetaryLiterals is a lexical pass, not a TOML parser: the original
// document has already passed toml.Decode. Only the X-only second decode uses
// its output. Strings/comments stay intact, including multiline strings and
// quoted keys; the TOML decoder still resolves tables, arrays and dotted keys.
func quoteXMonetaryLiterals(content string) string {
	var out strings.Builder
	key := ""
	for i := 0; i < len(content); {
		start := i
		switch content[i] {
		case '#':
			for i < len(content) && content[i] != '\n' {
				i++
			}
		case '\'', '"':
			quote := content[i]
			width := 1
			if strings.HasPrefix(content[i:], strings.Repeat(string(quote), 3)) {
				width = 3
			}
			i += width
			for i < len(content) {
				if quote == '"' && content[i] == '\\' {
					i += 2
					continue
				}
				if strings.HasPrefix(content[i:], strings.Repeat(string(quote), width)) {
					i += width
					// TOML permits one or two quotes before a triple-string terminator.
					if width == 3 {
						for i < len(content) && content[i] == quote {
							i++
						}
					}
					break
				}
				i++
			}
			if i > len(content) {
				i = len(content)
			}
			var decoded struct{ Key string }
			if _, err := toml.Decode("Key="+content[start:i], &decoded); err == nil {
				key = decoded.Key
			}
		case '=':
			i++
			out.WriteString(content[start:i])
			switch key {
			case "monthly_budget", "post_read_price", "user_read_price", "create_reservation":
				start = i
				for i < len(content) && (content[i] == ' ' || content[i] == '\t') {
					i++
				}
				out.WriteString(content[start:i])
				start = i
				for i < len(content) && !strings.ContainsRune(" \t\r\n,#]}\"'[{", rune(content[i])) {
					i++
				}
				if start < i {
					out.WriteString(strconv.Quote(strings.ReplaceAll(content[start:i], "_", "")))
				}
			}
			key = ""
			continue
		case ' ', '\t', '\r', '\n', '.', '[', ']', '{', '}', ',':
			i++
		default:
			for i < len(content) && !strings.ContainsRune(" \t\r\n.=[]{}#,\"'", rune(content[i])) {
				i++
			}
			key = content[start:i]
		}
		out.WriteString(content[start:i])
	}
	return out.String()
}

// MarshalTOML emits exact decimal dollars, preserving explicit zeros. The
// inline account tables are equivalent to [[x.accounts]] on reload.
func (a XAccountConfig) MarshalTOML() ([]byte, error) {
	if err := a.Validate(a.UserID); err != nil {
		return nil, err
	}
	usd := func(micros int64) string { return fmt.Sprintf("%d.%06d", micros/1_000_000, micros%1_000_000) }
	postPrice := ""
	if a.PostReadPriceOverride {
		postPrice = ", post_read_price = " + usd(a.PostReadPriceMicros)
	}
	return []byte(fmt.Sprintf("{ user_id = %q, monthly_budget = %s, owned_reads = %t, refresh_batch_size = %d, refresh_sweep_interval = %q%s, user_read_price = %s, create_reservation = %s }",
		a.UserID, usd(a.MonthlyBudgetMicros), a.OwnedReads, a.RefreshBatchSize, a.RefreshSweepInterval.String(), postPrice, usd(a.UserReadPriceMicros), usd(a.CreateReservationMicros))), nil
}
