package vcard

import (
	"encoding/base64"
	"fmt"
	"mime"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// valueTypeText is the VALUE parameter value for free text (RFC 6350 §5.2).
const valueTypeText = "text"

// propertyRender carries one property through its conversion to the target
// wire version together with the facts lifted out of its source parameters.
type propertyRender struct {
	property Property
	source   Version
	target   Version
	// mediaProperty is true when the media format lives in TYPE on the source
	// side (a decoded inline binary, or a vCard 3 media reference rendered for
	// vCard 4): the TYPE media token is lifted into mediaType instead of being
	// kept as a type.
	mediaProperty bool
	// mediaURI is true once a legacy inline binary was decoded to a data URI.
	mediaURI bool
	// inlineBinary is true once a data URI was spelled back as vCard 3
	// ENCODING=b, which then carries no VALUE parameter.
	inlineBinary bool
	pref         bool
	valueType    string
	mediaType    string
}

// normalizePropertyForVersion converts one property to the target version.
// It reports omit=true when the vCard 3 compatibility view has no spelling for
// the occurrence; the canonical vCard 4 render never omits.
//
// A vCard 4 property rendered for vCard 4 is left untouched unless it still
// carries a transfer declaration (CHARSET, ENCODING, or a bare 2.1 token):
// the canonical render is idempotent and preserves the source spelling of
// every parameter it does not own.
func normalizePropertyForVersion(
	property Property, sourceVersion, targetVersion Version,
) (Property, bool, error) {
	if sourceVersion == Version40 && targetVersion == Version40 &&
		!hasTransferParameters(property) {
		return property, false, nil
	}
	render := propertyRender{property: property, source: sourceVersion, target: targetVersion}
	if err := render.decodeTransferEncoding(); err != nil {
		return Property{}, false, err
	}
	render.normalizeParameters()
	if err := render.normalizeValue(); err != nil {
		return Property{}, false, err
	}
	if render.omittedFromV3View() {
		return Property{}, true, nil
	}
	if err := render.emitTargetParameters(); err != nil {
		return Property{}, false, err
	}
	return render.property, false, nil
}

func hasTransferParameters(property Property) bool {
	for _, parameter := range property.Parameters {
		if parameter.Bare || isTransferParameter(parameter) {
			return true
		}
	}
	return false
}

// decodeTransferEncoding replaces a legacy transfer-encoded value with its
// decoded form: inline base64 becomes a data URI, quoted-printable and
// charset-declared text becomes UTF-8. vCard 2.1 text then has its literal
// backslashes escaped, since 2.1 knows no backslash escapes beyond "\;", and
// decoded line breaks become the vCard text escape.
func (r *propertyRender) decodeTransferEncoding() error {
	if propertyIsBase64Encoded(r.property) {
		data, err := decodeLegacyBinary(r.property.RawValue)
		if err != nil {
			return fmt.Errorf("decode legacy binary %s: %w", r.property.Name, err)
		}
		mediaType := "application/octet-stream"
		if isInlineMediaName(r.property.Name) {
			mediaType = inferMediaType(r.property)
		}
		r.property.RawValue = "data:" + mediaType + ";base64," +
			base64.StdEncoding.EncodeToString(data)
		r.mediaURI = true
		r.mediaProperty = isInlineMediaName(r.property.Name)
		return nil
	}
	decoded, quotedPrintable, err := decodeLegacyTransfer(r.property)
	if err != nil {
		return fmt.Errorf("decode legacy text %s: %w", r.property.Name, err)
	}
	if r.source == Version21 {
		decoded = escapeLegacyBackslashes(decoded)
	}
	if quotedPrintable {
		decoded = escapeDecodedLineBreaks(decoded)
	}
	r.property.RawValue = decoded
	r.mediaProperty = r.target == Version40 && r.source != Version40 &&
		isInlineMediaName(r.property.Name)
	return nil
}

// escapeLegacyBackslashes escapes the backslashes of a vCard 2.1 value that
// vCard 3 and 4 would read as escapes. vCard 2.1 escapes only the structured
// delimiter ("\;"); this keeps that escape, the same-shaped "\," and "\\" many
// 2.1 producers also write, and doubles every other backslash so a value such
// as C:\Users\name survives as text.
func escapeLegacyBackslashes(value string) string {
	if !strings.ContainsRune(value, '\\') {
		return value
	}
	var escaped strings.Builder
	escaped.Grow(len(value) + 8)
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			escaped.WriteByte(value[index])
			continue
		}
		if index+1 < len(value) && strings.IndexByte(`;,\`, value[index+1]) >= 0 {
			escaped.WriteByte('\\')
			escaped.WriteByte(value[index+1])
			index++
			continue
		}
		escaped.WriteString(`\\`)
	}
	return escaped.String()
}

// normalizeParameters rewrites the property's parameters for the target
// version, lifting VALUE, the media type, and the preference flag out of the
// source spelling. Transfer declarations are always dropped: the value has
// been decoded.
func (r *propertyRender) normalizeParameters() {
	kept := make([]Parameter, 0, len(r.property.Parameters))
	for _, parameter := range r.property.Parameters {
		parameter.Values = append([]ParameterValue(nil), parameter.Values...)
		if r.liftParameter(parameter) {
			continue
		}
		parameter = r.retargetParameter(parameter)
		if strings.EqualFold(parameter.Name, parameterTypeName) {
			parameter.Values = r.keptTypeValues(parameter.Values)
			if len(parameter.Values) == 0 {
				continue
			}
		}
		if r.target == Version30 && r.dropsFromV3(parameter) {
			continue
		}
		kept = append(kept, parameter)
	}
	r.property.Parameters = kept
}

// liftParameter records what a parameter declares and reports whether it is
// consumed rather than kept.
func (r *propertyRender) liftParameter(parameter Parameter) bool {
	switch strings.ToUpper(parameter.Name) {
	case "VALUE":
		if len(parameter.Values) > 0 {
			r.valueType = strings.ToLower(strings.TrimSpace(parameter.Values[0].Decoded))
		}
		return true
	case "CHARSET", "ENCODING":
		return true
	case "MEDIATYPE":
		if len(parameter.Values) > 0 {
			r.mediaType = strings.TrimSpace(parameter.Values[0].Decoded)
		}
		return r.mediaProperty
	}
	return parameter.Bare && isLegacyEncodingToken(parameter)
}

// retargetParameter spells a bare 2.1 token as TYPE and drops preserved raw
// syntax whenever the parameter is rendered for another version, so the
// encoder derives it from the decoded value.
func (r *propertyRender) retargetParameter(parameter Parameter) Parameter {
	if parameter.Bare {
		parameter.Bare = false
		parameter.Name = parameterTypeName
		parameter.OriginalName = parameterTypeName
		invalidateParameterRawSyntax(&parameter)
	} else if r.source != r.target {
		invalidateParameterRawSyntax(&parameter)
	}
	return parameter
}

// keptTypeValues filters TYPE values: "pref" is lifted into the preference
// flag (a vCard 4 render spells it PREF=1) and a media format token on a media
// property is lifted into the media type.
func (r *propertyRender) keptTypeValues(values []ParameterValue) []ParameterValue {
	kept := values[:0]
	for _, value := range values {
		token := strings.TrimSpace(value.Decoded)
		if strings.EqualFold(token, "pref") {
			r.pref = true
			if r.target == Version40 {
				continue
			}
		}
		if r.mediaProperty && isMediaTypeToken(token) {
			if r.mediaType == "" {
				r.mediaType = token
			}
			continue
		}
		kept = append(kept, value)
	}
	return kept
}

// dropsFromV3 reports whether the vCard 3 view drops the parameter. PREF=1
// survives as TYPE=pref; the other vCard 4 parameters have no spelling.
func (r *propertyRender) dropsFromV3(parameter Parameter) bool {
	name := strings.ToUpper(parameter.Name)
	if name == "PREF" {
		for _, value := range parameter.Values {
			r.pref = r.pref || strings.TrimSpace(value.Decoded) == "1"
		}
		return true
	}
	return isV4OnlyParameter(name)
}

// normalizeValue converts the value spellings that differ between vCard 3
// and vCard 4: TEL, GEO, inline media, and the component count of N and ADR.
func (r *propertyRender) normalizeValue() error {
	r.normalizeTelephone()
	if err := r.normalizeGeo(); err != nil {
		return err
	}
	if r.target == Version30 {
		r.inlineMediaForV3()
		r.property.RawValue = v3StructuredValue(r.property.Name, r.property.RawValue)
	}
	return nil
}

// normalizeTelephone converts between the vCard 3 phone-number text and the
// vCard 4 tel URI.
func (r *propertyRender) normalizeTelephone() {
	if !strings.EqualFold(r.property.Name, "TEL") {
		return
	}
	switch r.target {
	case Version40:
		r.telephoneForV4()
	case Version30:
		r.telephoneForV3()
	case Version21:
		// vCard 2.1 is import-only; RenderView rejects it before rendering.
	}
}

// telephoneForV4 spells a legacy phone number as a tel URI. A value vCard 4
// cannot spell that way, or one declared VALUE=text, is kept as text (RFC
// 6350 §6.4.1 allows TEL;VALUE=text).
func (r *propertyRender) telephoneForV4() {
	value := strings.TrimSpace(r.property.RawValue)
	if r.valueType == valueTypeText || IsURIValue(value) {
		return
	}
	uri, ok := telURIFromLegacyNumber(value)
	if !ok {
		r.valueType = valueTypeText
		return
	}
	r.property.RawValue = uri
	r.valueType = ""
}

// telephoneForV3 spells a tel URI as vCard 3 phone-number text. The URI's
// parameters (such as ext) stay in the text with their ";" escaped, so a
// structured-value reader does not split the number.
func (r *propertyRender) telephoneForV3() {
	// A value declared VALUE=text is literal text however URI-shaped it
	// looks; only an actual tel URI is respelled.
	if r.valueType == valueTypeText {
		return
	}
	value := strings.TrimSpace(r.property.RawValue)
	const scheme = "tel:"
	if len(value) < len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) {
		return
	}
	phone, err := url.PathUnescape(value[len(scheme):])
	if err != nil {
		return
	}
	r.property.RawValue = EscapeText(phone)
	r.valueType = ""
}

// telURIFromLegacyNumber spells a vCard 3 phone number as a tel URI when it
// consists only of dial characters after removing whitespace.
func telURIFromLegacyNumber(value string) (string, bool) {
	var normalized strings.Builder
	for _, r := range value {
		if unicode.IsSpace(r) {
			continue
		}
		if r > unicode.MaxASCII || (!unicode.IsLetter(r) && !unicode.IsDigit(r) &&
			!strings.ContainsRune("+-.()=;,*#", r)) {
			return "", false
		}
		normalized.WriteRune(r)
	}
	if normalized.Len() == 0 {
		return "", false
	}
	return "tel:" + normalized.String(), true
}

// normalizeGeo converts GEO between the two wire spellings of the same
// coordinates: RFC 2426 writes `lat;long`, while RFC 6350 writes a single
// URI, normally the RFC 5870 geo URI. A legacy value that is not two floats
// cannot be spelled as a geo URI, so canonical normalization fails rather than
// emit guessed syntax. A canonical URI vCard 3 cannot express is left
// untouched here and omitted from the vCard 3 view by omitFromV3View.
func (r *propertyRender) normalizeGeo() error {
	if !strings.EqualFold(r.property.Name, "GEO") {
		return nil
	}
	value := strings.TrimSpace(r.property.RawValue)
	switch r.target {
	case Version40:
		if IsURIValue(value) {
			r.property.RawValue = value
			return nil
		}
		latitude, longitude, ok := v3GeoCoordinates(value)
		if !ok {
			return fmt.Errorf("legacy GEO value %q cannot be converted to a geo URI", value)
		}
		r.property.RawValue = "geo:" + latitude + "," + longitude
		r.valueType = ""
	case Version30:
		if latitude, longitude, ok := geoURICoordinates(value); ok {
			r.property.RawValue = latitude + ";" + longitude
			r.valueType = ""
		}
	case Version21:
		// vCard 2.1 is import-only; RenderView rejects it before rendering.
	}
	return nil
}

// inlineMediaForV3 spells a base64 data URI on PHOTO, LOGO, SOUND, or KEY as
// the RFC 2426 inline binary (ENCODING=b with the format in TYPE), which is
// the only inline form vCard 3 clients read.
func (r *propertyRender) inlineMediaForV3() {
	if !isInlineMediaName(r.property.Name) {
		return
	}
	mediaType, data, ok := decodeBase64DataURI(r.property.RawValue)
	if !ok {
		return
	}
	encoding, err := NewParameter("ENCODING", "b")
	if err != nil {
		return
	}
	r.property.RawValue = base64.StdEncoding.EncodeToString(data)
	r.property.Parameters = append(r.property.Parameters, encoding)
	r.mediaType = mediaType
	r.inlineBinary = true
}

// decodeBase64DataURI splits an RFC 2397 data URI whose payload is base64
// into its media type and decoded bytes.
func decodeBase64DataURI(value string) (string, []byte, bool) {
	const scheme = "data:"
	if len(value) < len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) {
		return "", nil, false
	}
	header, payload, found := strings.Cut(value[len(scheme):], ",")
	if !found {
		return "", nil, false
	}
	const marker = ";base64"
	if len(header) < len(marker) ||
		!strings.EqualFold(header[len(header)-len(marker):], marker) {
		return "", nil, false
	}
	mediaType := strings.TrimSpace(header[:len(header)-len(marker)])
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	data, err := decodeLegacyBinary(payload)
	if err != nil {
		return "", nil, false
	}
	return mediaType, data, true
}

// omittedFromV3View reports whether the vCard 3 compatibility view has no
// spelling for the normalized occurrence: a registered vCard 4 (or RFC 9554 /
// 9555) property, a BDAY or REV reset to free text, or a GEO value that is
// not a plain WGS-84 coordinate pair (normalizeGeo has already rewritten every
// convertible geo URI). The occurrence stays in PropertyTree and residue, so a
// vCard 4 view returns it unchanged. The canonical vCard 4 render never omits.
func (r *propertyRender) omittedFromV3View() bool {
	if r.target != Version30 {
		return false
	}
	name := strings.ToUpper(r.property.Name)
	switch {
	case isV4OnlyProperty(name):
		return true
	case name == "BDAY" || name == "REV":
		return r.valueType == valueTypeText
	case name == "GEO":
		_, _, ok := v3GeoCoordinates(strings.TrimSpace(r.property.RawValue))
		return !ok
	default:
		return false
	}
}

// emitTargetParameters re-emits the lifted media type, preference, and VALUE
// type in the target version's spelling, then drops what vCard 3 cannot carry.
func (r *propertyRender) emitTargetParameters() error {
	if err := r.applyMediaType(); err != nil {
		return err
	}
	if err := r.applyPreference(); err != nil {
		return err
	}
	if err := r.applyValueType(); err != nil {
		return err
	}
	if r.target == Version30 {
		r.dropUnspellableV3ParameterValues()
	}
	return nil
}

// applyMediaType spells the lifted media type for the target: the legacy
// TYPE token in vCard 3, MEDIATYPE for a vCard 3 media reference rendered
// in vCard 4 (a decoded inline binary carries its type in the data URI).
func (r *propertyRender) applyMediaType() error {
	if r.mediaType == "" {
		return nil
	}
	if r.target == Version30 && isInlineMediaName(r.property.Name) {
		r.property.Parameters = addTypeParameterValue(
			r.property.Parameters, v3MediaTypeToken(r.mediaType),
		)
		return nil
	}
	if !r.mediaProperty || r.mediaURI {
		return nil
	}
	parameter, err := NewParameter("MEDIATYPE", canonicalMediaType(r.mediaType))
	if err != nil {
		return err
	}
	r.property.Parameters = append(r.property.Parameters, parameter)
	return nil
}

// applyPreference spells a lifted preference as PREF=1 in vCard 4 and as the
// TYPE=pref token in vCard 3.
func (r *propertyRender) applyPreference() error {
	if !r.pref {
		return nil
	}
	if r.target == Version30 {
		r.property.Parameters = addTypeParameterValue(r.property.Parameters, "pref")
		return nil
	}
	if len(r.property.ParametersNamed("PREF")) > 0 {
		return nil
	}
	preference, err := NewParameter("PREF", "1")
	if err != nil {
		return err
	}
	r.property.Parameters = append(r.property.Parameters, preference)
	return nil
}

// applyValueType appends the VALUE parameter the target version needs, if any.
func (r *propertyRender) applyValueType() error {
	var valueType string
	if r.target == Version30 {
		valueType = r.v3ValueType()
	} else {
		valueType = r.v4ValueType()
	}
	if valueType == "" {
		return nil
	}
	parameter, err := NewParameter("VALUE", valueType)
	if err != nil {
		return err
	}
	r.property.Parameters = append(r.property.Parameters, parameter)
	return nil
}

func (r *propertyRender) v3ValueType() string {
	name := strings.ToUpper(r.property.Name)
	switch {
	case name == "UID" || r.inlineBinary:
		// UID is TEXT in vCard 3.0; URI-shaped identifiers stay text. Inline
		// binary is declared by ENCODING=b alone.
		return ""
	case isInlineMediaName(name):
		return r.v3MediaValueType(name)
	case r.mediaURI:
		return "uri"
	case name == "TEL" && r.valueType != valueTypeText &&
		(r.valueType == "uri" || IsURIValue(r.property.RawValue)):
		// A tel URI the v3 respelling could not unescape stays a URI. A
		// declared-text value is never one, however URI-shaped it looks.
		return "uri"
	default:
		return v3ValueTypeName(r.valueType)
	}
}

func (r *propertyRender) v3MediaValueType(name string) string {
	switch {
	case name == "KEY" && r.valueType == valueTypeText && !r.mediaURI:
		// KEY may be reset to text in both 3.0 and 4.0; without the
		// parameter 3.0 would read the value as inline binary.
		return valueTypeText
	case r.mediaURI || IsURIValue(r.property.RawValue):
		return "uri"
	default:
		return v3ValueTypeName(r.valueType)
	}
}

// v3ValueTypeName maps a VALUE type to the parameter the vCard 3 view emits.
// The default text type is left implicit; of the vCard 4 types vCard 3 lacks,
// timestamp degrades to date-time and the others to the default text.
func v3ValueTypeName(valueType string) string {
	switch valueType {
	case "timestamp":
		return "date-time"
	case valueTypeText, "date-and-or-time", "language-tag":
		return ""
	default:
		return valueType
	}
}

func (r *propertyRender) v4ValueType() string {
	name := strings.ToUpper(r.property.Name)
	switch {
	case isInlineMediaName(name):
		return r.v4MediaValueType(name)
	case r.mediaURI:
		// A decoded legacy binary on any other property is a URI, which is
		// not that property's default type.
		return "uri"
	case name == "UID" || name == "RELATED":
		if r.valueType == valueTypeText || !IsURIValue(r.property.RawValue) {
			return valueTypeText
		}
		return ""
	case r.valueType == "uri" && v4URIValueByDefault(name):
		return ""
	default:
		return r.valueType
	}
}

// v4MediaValueType handles PHOTO, LOGO, SOUND, and KEY, whose vCard 4 values
// are URIs by default. KEY is the one of them 4.0 lets reset to text;
// dropping that parameter would turn the key material into a URI.
func (r *propertyRender) v4MediaValueType(name string) string {
	if name == "KEY" && r.valueType == valueTypeText && !r.mediaURI {
		return valueTypeText
	}
	return ""
}

// dropUnspellableV3ParameterValues removes parameter values vCard 3 cannot
// carry: RFC 6868 lets a vCard 4 parameter hold a quote or a line break, and
// vCard 3 has no encoding for either. A parameter left without values is
// dropped; the property itself stays in the view.
func (r *propertyRender) dropUnspellableV3ParameterValues() {
	kept := r.property.Parameters[:0]
	for _, parameter := range r.property.Parameters {
		values := parameter.Values[:0]
		for _, value := range parameter.Values {
			if strings.ContainsAny(value.Decoded, "\"\r\n\x00") {
				continue
			}
			values = append(values, value)
		}
		if len(values) == 0 {
			continue
		}
		parameter.Values = values
		kept = append(kept, parameter)
	}
	r.property.Parameters = kept
}

func v4URIValueByDefault(name string) bool {
	switch strings.ToUpper(name) {
	case "SOURCE", "PHOTO", "TEL", "IMPP", "GEO", "LOGO", "MEMBER", "RELATED",
		"SOUND", "UID", "URL", "KEY", "FBURL", "CALADRURI", "CALURI",
		"BIRTHPLACE", "DEATHPLACE", "CONTACT-URI", "ORG-DIRECTORY":
		return true
	default:
		return false
	}
}

func addTypeParameterValue(parameters []Parameter, value string) []Parameter {
	for index := range parameters {
		if !strings.EqualFold(parameters[index].Name, parameterTypeName) {
			continue
		}
		for _, existing := range parameters[index].Values {
			if strings.EqualFold(existing.Decoded, value) {
				return parameters
			}
		}
		parameters[index].Values = append(parameters[index].Values, ParameterValue{Decoded: value})
		return parameters
	}
	parameter, _ := NewParameter(parameterTypeName, value)
	return append(parameters, parameter)
}

func invalidateParameterRawSyntax(parameter *Parameter) {
	for index := range parameter.Values {
		parameter.Values[index].Raw = ""
		parameter.Values[index].RawValid = false
		parameter.Values[index].Quoted = false
	}
}

// v3GeoCoordinates splits an RFC 2426 GEO value into its two float components.
// The decimal strings are returned as written, minus a leading "+" that RFC
// 5870 does not allow, so a conversion keeps the source precision.
func v3GeoCoordinates(value string) (string, string, bool) {
	latitude, longitude, found := strings.Cut(value, ";")
	if !found {
		return "", "", false
	}
	latitude, latitudeValid := geoCoordinate(latitude, 90)
	longitude, longitudeValid := geoCoordinate(longitude, 180)
	if !latitudeValid || !longitudeValid {
		return "", "", false
	}
	return latitude, longitude, true
}

// geoURICoordinates converts an RFC 5870 geo URI to the two RFC 2426 float
// components. A vCard 3 GEO value carries exactly a WGS-84 latitude and
// longitude, so a URI with an altitude, an uncertainty, a CRS other than the
// default wgs84, or any other geo parameter has no vCard 3 spelling and is
// reported as unconvertible rather than silently truncated.
func geoURICoordinates(value string) (string, string, bool) {
	const scheme = "geo:"
	if len(value) < len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) {
		return "", "", false
	}
	coordinates, parameters, _ := strings.Cut(value[len(scheme):], ";")
	latitude, longitude, found := strings.Cut(coordinates, ",")
	if !found || strings.Contains(longitude, ",") {
		return "", "", false
	}
	if parameters != "" {
		for parameter := range strings.SplitSeq(parameters, ";") {
			name, parameterValue, _ := strings.Cut(parameter, "=")
			if !strings.EqualFold(name, "crs") ||
				!strings.EqualFold(parameterValue, "wgs84") {
				return "", "", false
			}
		}
	}
	return v3GeoCoordinates(latitude + ";" + longitude)
}

// geoCoordinate validates one coordinate against the plain decimal grammar
// shared by RFC 2426 and RFC 5870 — neither admits an exponent — and returns
// its RFC 5870 spelling. Only RFC 2426 permits a leading "+".
func geoCoordinate(value string, limit float64) (string, bool) {
	digits := value
	if strings.HasPrefix(digits, "+") || strings.HasPrefix(digits, "-") {
		digits = digits[1:]
	}
	whole, fraction, hasFraction := strings.Cut(digits, ".")
	if !isDecimalDigits(whole) || (hasFraction && !isDecimalDigits(fraction)) {
		return "", false
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number < -limit || number > limit {
		return "", false
	}
	return strings.TrimPrefix(value, "+"), true
}

func isDecimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func v3StructuredValue(name, raw string) string {
	limit := 5
	switch strings.ToUpper(name) {
	case "N":
	case "ADR":
		limit = 7
	default:
		return raw
	}
	parts := splitRawStructuredValue(raw)
	if len(parts) <= limit {
		return raw
	}
	return strings.Join(parts[:limit], ";")
}

func splitRawStructuredValue(raw string) []string {
	parts := make([]string, 0, strings.Count(raw, ";")+1)
	start := 0
	for index := 0; index < len(raw); index++ {
		if raw[index] == '\\' && index+1 < len(raw) {
			index++
			continue
		}
		if raw[index] == ';' {
			parts = append(parts, raw[start:index])
			start = index + 1
		}
	}
	return append(parts, raw[start:])
}

// IsURIValue reports whether raw is an absolute URI: no whitespace or control
// characters and a scheme. Property values whose vCard type is URI (URL,
// SOCIALPROFILE, IMPP, ...) must satisfy this to be rendered as such.
func IsURIValue(raw string) bool {
	if strings.ContainsAny(raw, "\r\n\x00 ") {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme != ""
}

func isInlineMediaName(name string) bool {
	switch strings.ToUpper(name) {
	case "PHOTO", "LOGO", "SOUND", "KEY":
		return true
	default:
		return false
	}
}

func propertyIsBase64Encoded(property Property) bool {
	for _, parameter := range property.Parameters {
		if !strings.EqualFold(parameter.Name, "ENCODING") && !parameter.Bare {
			continue
		}
		for _, value := range parameter.Values {
			token := strings.TrimSpace(value.Decoded)
			if strings.EqualFold(token, "b") || strings.EqualFold(token, "base64") {
				return true
			}
		}
	}
	return false
}

func isLegacyEncodingToken(parameter Parameter) bool {
	if !parameter.Bare {
		return false
	}
	for _, value := range parameter.Values {
		token := strings.TrimSpace(value.Decoded)
		if strings.EqualFold(token, "b") || strings.EqualFold(token, "base64") ||
			strings.EqualFold(token, "quoted-printable") {
			return true
		}
	}
	return false
}

func decodeLegacyBinary(raw string) ([]byte, error) {
	raw = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		default:
			return r
		}
	}, raw)
	data, err := base64.StdEncoding.DecodeString(raw)
	if err == nil {
		return data, nil
	}
	data, err = base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode raw base64: %w", err)
	}
	return data, nil
}

func inferMediaType(property Property) string {
	for _, parameter := range property.Parameters {
		if !strings.EqualFold(parameter.Name, "MEDIATYPE") &&
			!strings.EqualFold(parameter.Name, parameterTypeName) {
			continue
		}
		for _, value := range parameter.Values {
			if mediaType := mediaTypeFromToken(value.Decoded); mediaType != "" {
				return mediaType
			}
		}
	}
	return "application/octet-stream"
}

// canonicalMediaType resolves a media type or a legacy TYPE token to a full
// media type, falling back to the token itself when it names nothing known.
func canonicalMediaType(token string) string {
	if mediaType := mediaTypeFromToken(token); mediaType != "" {
		return mediaType
	}
	return strings.TrimSpace(token)
}

// legacyMediaTokens is the fixed vocabulary of format tokens vCard 2.1 and 3
// (RFC 2426 §3.1.4, §3.6.6, §3.7.2) named for media properties, with the
// media type each spells in vCard 4. The table is deliberately closed: the
// host's mime tables must never decide canonical bytes, and a token outside
// it is preserved literally.
var legacyMediaTokens = []struct{ token, mediaType string }{
	{"JPEG", "image/jpeg"}, {"JPG", "image/jpeg"},
	{"GIF", "image/gif"},
	{"PNG", "image/png"},
	{"TIFF", "image/tiff"}, {"TIF", "image/tiff"},
	{"BMP", "image/bmp"}, {"DIB", "image/bmp"},
	{"CGM", "image/cgm"},
	{"WMF", "image/wmf"},
	{"PICT", "image/x-pict"},
	{"PS", "application/postscript"},
	{"PDF", "application/pdf"},
	{"MPEG", "video/mpeg"}, {"MPEG2", "video/mpeg"}, {"MPG", "video/mpeg"},
	{"AVI", "video/x-msvideo"},
	{"QTIME", "video/quicktime"}, {"MOV", "video/quicktime"},
	{"WAVE", "audio/x-wav"}, {"WAV", "audio/x-wav"},
	{"AIFF", "audio/x-aiff"}, {"AIF", "audio/x-aiff"},
	{"BASIC", "audio/basic"}, {"AU", "audio/basic"},
	{"MP3", "audio/mpeg"},
	{"X509", "application/pkix-cert"},
	{"PGP", "application/pgp-keys"},
}

func isMediaTypeToken(value string) bool {
	return mediaTypeFromToken(value) != ""
}

// mediaTypeFromToken maps a full media type or one of the legacy format
// tokens to a media type, or "" when it recognizes neither.
func mediaTypeFromToken(token string) string {
	candidate := strings.ToLower(strings.TrimSpace(token))
	if strings.Contains(candidate, "/") {
		return candidate
	}
	for _, entry := range legacyMediaTokens {
		if strings.EqualFold(entry.token, candidate) {
			return entry.mediaType
		}
	}
	return ""
}

// v3MediaTypeToken spells a media type as the legacy TYPE token: the first
// table entry for a known type, otherwise the upper-cased subtype.
func v3MediaTypeToken(mediaType string) string {
	parsed, _, err := mime.ParseMediaType(mediaType)
	if err == nil {
		mediaType = parsed
	}
	for _, entry := range legacyMediaTokens {
		if strings.EqualFold(entry.mediaType, mediaType) {
			return entry.token
		}
	}
	_, subtype, found := strings.Cut(mediaType, "/")
	if found {
		mediaType = subtype
	}
	mediaType = strings.TrimPrefix(strings.TrimSpace(mediaType), "x-")
	return strings.ToUpper(mediaType)
}

// vCard 3 has no representation for these registered vCard 4/RFC9554/RFC9555
// elements. The vCard 3 view omits only this compatibility set. Properties
// such as UID, CATEGORIES, PRODID, REV, IMPP, and SOURCE are valid vCard 3
// properties (or established vCard 3 extensions) and must remain visible.
// Omitted occurrences stay in PropertyTree and residue, so a later vCard 4
// view returns them.
var v4OnlyProperties = map[string]struct{}{
	"KIND": {}, "XML": {}, "GENDER": {}, "ANNIVERSARY": {}, "LANG": {},
	"MEMBER": {}, "RELATED": {}, "CLIENTPIDMAP": {}, "FBURL": {},
	"CALADRURI": {}, "CALURI": {}, "BIRTHPLACE": {}, "DEATHPLACE": {},
	"DEATHDATE": {}, "EXPERTISE": {}, "HOBBY": {}, "INTEREST": {},
	"ORG-DIRECTORY": {}, "CONTACT-URI": {}, "CREATED": {},
	"GRAMGENDER": {}, "LANGUAGE": {}, "PRONOUNS": {},
	"SOCIALPROFILE": {}, "JSPROP": {},
}

var v4OnlyParameters = map[string]struct{}{
	"PREF": {}, "ALTID": {}, "PID": {}, "MEDIATYPE": {}, "CALSCALE": {},
	"SORT-AS": {}, "GEO": {}, "TZ": {}, "INDEX": {}, "LEVEL": {},
	"CC": {}, "AUTHOR": {}, "AUTHOR-NAME": {}, "CREATED": {},
	"DERIVED": {}, "LABEL": {}, "PHONETIC": {}, "PROP-ID": {},
	"SCRIPT": {}, "SERVICE-TYPE": {}, "USERNAME": {}, "JSPTR": {},
	"JSCOMPS": {},
}

func isV4OnlyProperty(name string) bool {
	_, ok := v4OnlyProperties[strings.ToUpper(strings.TrimSpace(name))]
	return ok
}

func isV4OnlyParameter(name string) bool {
	_, ok := v4OnlyParameters[strings.ToUpper(strings.TrimSpace(name))]
	return ok
}
