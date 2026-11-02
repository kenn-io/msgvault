package mailrecords

import (
	"encoding/json/jsontext"
	"html"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/currency"
)

type fieldSpec struct{ name, paths, kind string }

const (
	fieldText   = "text"
	fieldStatus = "status"
	fieldDate   = "date"
)

var fields = map[string][]fieldSpec{
	"Order": {
		{"merchant", "seller.name|merchant.name|seller", fieldText},
		{"order_number", "orderNumber", fieldText},
		{"amount", "price|priceSpecification.price", "decimal"},
		{"currency", "priceCurrency|priceSpecification.priceCurrency", "currency"},
		{fieldStatus, "orderStatus", fieldStatus},
		{"starts_at", "orderDate", fieldDate},
	},
	"ParcelDelivery": {
		{"tracking_number", "trackingNumber", fieldText},
		{"carrier", "carrier.name|carrier", fieldText},
		{"tracking_url", "trackingUrl", fieldText},
		{"order_number", "partOfOrder.orderNumber", fieldText},
		{"expected_arrival_after", "expectedArrivalFrom", fieldDate},
		{"expected_arrival_before", "expectedArrivalUntil", fieldDate},
		{fieldStatus, "deliveryStatus", fieldStatus},
	},
	"Invoice": {
		{"invoice_number", "confirmationNumber", fieldText},
		{"merchant", "broker.name|provider.name", fieldText},
		{"amount", "totalPaymentDue.value|price", "decimal"},
		{"currency", "totalPaymentDue.currency|priceCurrency", "currency"},
		{"payment_due_date", "paymentDueDate", fieldDate},
		{fieldStatus, "paymentStatus", fieldStatus},
	},
	"FlightReservation": {
		{"confirmation_code", "reservationNumber", fieldText},
		{fieldStatus, "reservationStatus", fieldStatus},
		{"flight_number", "reservationFor.flightNumber", fieldText},
		{"carrier", "reservationFor.airline.name", fieldText},
		{"starts_at", "reservationFor.departureTime", fieldDate},
		{"ends_at", "reservationFor.arrivalTime", fieldDate},
		{"origin", "reservationFor.departureAirport.iataCode|reservationFor.departureAirport.name", fieldText},
		{"destination", "reservationFor.arrivalAirport.iataCode|reservationFor.arrivalAirport.name", fieldText},
	},
	"LodgingReservation": {
		{"confirmation_code", "reservationNumber", fieldText},
		{fieldStatus, "reservationStatus", fieldStatus},
		{"merchant", "reservationFor.name", fieldText},
		{"starts_at", "checkinTime", fieldDate},
		{"ends_at", "checkoutTime", fieldDate},
	},
	"EventReservation": {
		{"confirmation_code", "reservationNumber", fieldText},
		{fieldStatus, "reservationStatus", fieldStatus},
		{"merchant", "reservationFor.name", fieldText},
		{"starts_at", "reservationFor.startDate", fieldDate},
		{"ends_at", "reservationFor.endDate", fieldDate},
		{"location", "reservationFor.location.name", fieldText},
	},
}

func extractRecord(name string, object map[string]any, context jsonldContext, source, visible guardSource, origin string) Record {
	record := Record{Type: name, Source: "json_ld", Fields: map[string]Field{}}
	for _, spec := range fields[name] {
		raw, exists := atPath(object, spec.paths, context)
		if !exists {
			continue
		}
		value, trust, valid := typedValue(raw, spec.kind)
		field := Field{Trust: trust, Origin: origin}
		backed := source.verify(value)
		if decoded, ok := raw.(string); ok && valid {
			// The validated JSON string is literal source evidence too, including
			// Unicode and slash escapes. Decimal checks keep their token guard.
			backed = value == decoded || newGuardSource(decoded).verify(value)
		}
		if valid && backed {
			field.Value = value
			if !visible.verify(value) {
				record.MarkupOnly = true
			}
		} else {
			record.Dropped++
		}
		record.Fields[spec.name] = field
	}
	return record
}

func atPath(object map[string]any, paths string, context jsonldContext) (any, bool) {
	for path := range strings.SplitSeq(paths, "|") {
		var value any = object
		pathContext := context
		found := true
		for key := range strings.SplitSeq(path, ".") {
			m, ok := value.(map[string]any)
			if !ok {
				found = false
				break
			}
			pathContext = objectContext(m, pathContext)
			if pathContext == contextUnsupported {
				found = false
				break
			}
			value, ok = m[key]
			if !ok {
				found = false
				break
			}
		}
		if found {
			return value, true
		}
	}
	return nil, false
}

var decimal = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,17})(\.[0-9]{1,4})?$`)
var numberToken = regexp.MustCompile(`-?[0-9]+(?:[,.][0-9]+)*(?:[eE][+-]?[0-9]+)?(?:[kKmMbB])?`)
var groupedDecimal = regexp.MustCompile(`^-?[0-9]{1,3}(,[0-9]{3})+(\.[0-9]+)?$`)

func typedValue(raw any, kind string) (any, string, bool) {
	trust := "untrusted_text"
	value, ok := raw.(string)
	if number, isNumber := raw.(jsontext.Value); isNumber {
		value, ok = string(number), kind == "decimal" && number.Kind() == '0'
	}
	if !ok || strings.TrimSpace(value) == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > MaxStringRunes {
		return nil, trust, false
	}
	switch kind {
	case "decimal":
		if !decimal.MatchString(value) {
			return nil, trust, false
		}
		return jsontext.Value(value), "typed", true
	case "currency":
		if _, err := currency.ParseISO(value); err != nil {
			return nil, trust, false
		}
		trust = "typed"
	case fieldDate:
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			if _, err = time.Parse("2006-01-02", value); err != nil {
				return nil, trust, false
			}
		}
		trust = "typed"
	case fieldStatus:
		if knownStatus(value) {
			trust = "typed"
		}
	}
	return value, trust, true
}

func knownStatus(value string) bool {
	name := strings.TrimPrefix(strings.TrimPrefix(value, "https://schema.org/"), "http://schema.org/")
	switch name {
	case "OrderCancelled", "OrderDelivered", "OrderInTransit", "OrderPaymentDue", "OrderPickupAvailable", "OrderProcessing", "OrderProblem", "OrderReturned",
		"ReservationCancelled", "ReservationConfirmed", "ReservationHold", "ReservationPending", "PaymentAutomaticallyApplied", "PaymentComplete", "PaymentDeclined", "PaymentDue", "PaymentPastDue":
		return true
	}
	return false
}

// Verify checks that a string occurs verbatim (ignoring case and HTML entities)
// or a decimal is backed by one numeric token. Removing separators never joins
// unrelated numbers, and magnitude suffixes/exponents are never expanded.
func Verify(source string, value any) bool {
	return newGuardSource(source).verify(value)
}

type guardSource struct {
	lower   string
	numbers map[string]bool
}

func newGuardSource(source string) guardSource {
	guard := guardSource{lower: strings.ToLower(html.UnescapeString(source)), numbers: map[string]bool{}}
	for _, token := range numberToken.FindAllString(source, -1) {
		if groupedDecimal.MatchString(token) {
			token = strings.ReplaceAll(token, ",", "")
		}
		guard.numbers[token] = true
	}
	return guard
}

func (source guardSource) verify(value any) bool {
	switch v := value.(type) {
	case string:
		return v != "" && strings.Contains(source.lower, strings.ToLower(v))
	case jsontext.Value:
		return v.Kind() == '0' && source.numbers[string(v)]
	}
	return false
}
