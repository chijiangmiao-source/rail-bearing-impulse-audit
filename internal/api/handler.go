// Package api wires the pulse detector to an HTTP JSON API.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"trackside-pulse-api/internal/pulse"
)

// maxBodyBytes bounds accepted request bodies (20000 doubles need ~0.5 MiB).
const maxBodyBytes = 4 << 20

// AnalyzeRequest is the JSON request contract.
type AnalyzeRequest struct {
	SampleRate     float64       `json:"sample_rate"`
	Amplitudes     []float64     `json:"amplitudes"`
	ExcludedRanges []pulse.Range `json:"excluded_ranges,omitempty"`
	// IncludeMetrics switches on per-pulse duration_ms/rms_amplitude in the
	// response. Omitted or false keeps the legacy response shape.
	IncludeMetrics bool `json:"include_metrics,omitempty"`
}

// AnalyzeResponse is the full, deterministic response contract.
type AnalyzeResponse struct {
	SampleRate     float64       `json:"sample_rate"`
	Decidable      bool          `json:"decidable"`
	Baseline       float64       `json:"baseline"`
	Reason         string        `json:"reason,omitempty"`
	Pulses         []pulse.Pulse `json:"pulses"`
	ExcludedRanges []pulse.Range `json:"excluded_ranges,omitempty"`
}

// FieldError locates a rejected request at a field, or at one sample index.
type FieldError struct {
	Error      string `json:"error"`
	Field      string `json:"field,omitempty"`
	Index      *int   `json:"index,omitempty"`
	Constraint string `json:"constraint,omitempty"`
	Message    string `json:"message"`
}

func (e *FieldError) HTTPStatus() int { return http.StatusBadRequest }

// rawAnalyzeRequest keeps the fields as raw tokens so type errors can be
// attributed to the field or to an individual amplitude/range index.
// IncludeMetrics is a non-pointer RawMessage so an explicit null is
// distinguishable from an absent key: null is not a boolean and must be
// rejected, not treated as "field omitted".
type rawAnalyzeRequest struct {
	SampleRate     *json.RawMessage `json:"sample_rate"`
	Amplitudes     *json.RawMessage `json:"amplitudes"`
	ExcludedRanges *json.RawMessage `json:"excluded_ranges"`
	IncludeMetrics json.RawMessage  `json:"include_metrics"`
}

// NewRouter builds the HTTP router.
func NewRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	v1 := r.Group("/api/v1")
	{
		v1.POST("/pulses/analyze", analyzeHandler)
		v1.POST("/pulses/correlate", correlateHandler)
		v1.POST("/acquisition-audits", acquisitionAuditHandler)
	}
	return r
}

func analyzeHandler(c *gin.Context) {
	req, ferr := decodeAnalyzeRequest(c.Request)
	if ferr != nil {
		c.JSON(ferr.HTTPStatus(), ferr)
		return
	}

	// Metrics are opt-in: with the switch off the analysis is the legacy one
	// and the response stays field-for-field identical.
	var result pulse.Result
	if req.IncludeMetrics {
		result = pulse.AnalyzeWithMetrics(req.Amplitudes, req.SampleRate, req.ExcludedRanges...)
	} else {
		result = pulse.Analyze(req.Amplitudes, req.ExcludedRanges...)
	}
	c.JSON(http.StatusOK, AnalyzeResponse{
		SampleRate:     req.SampleRate,
		Decidable:      result.Decidable,
		Baseline:       result.Baseline,
		Reason:         result.Reason,
		Pulses:         result.Pulses,
		ExcludedRanges: req.ExcludedRanges,
	})
}

// decodeAnalyzeRequest parses and validates the body, locating every error.
func decodeAnalyzeRequest(r *http.Request) (*AnalyzeRequest, *FieldError) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &FieldError{Error: "body_too_large", Message: "request body exceeds 4 MiB"}
		}
		return nil, &FieldError{Error: "invalid_body", Message: "cannot read request body"}
	}

	// NaN/Infinity are not JSON numbers, so the standard decoder rejects the
	// whole document without saying which value offended. Locate such tokens
	// first so the error points at the field or the sample index.
	if ferr := locateNonFinite(body, nonFiniteAnalyze); ferr != nil {
		return nil, ferr
	}

	// Field names are a case-sensitive contract: encoding/json matches
	// struct tags case-insensitively, so without this walk a spelling like
	// "Sample_rate" would be silently accepted as sample_rate.
	if key, found := firstNonContractKey(body,
		"sample_rate", "amplitudes", "excluded_ranges", "include_metrics"); found {
		return nil, unknownFieldError(key, key)
	}

	var raw rawAnalyzeRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, decodeError(err)
	}
	// Reject trailing non-whitespace tokens after the object.
	if tok := dec.Decode(&struct{}{}); !errors.Is(tok, io.EOF) {
		return nil, &FieldError{Error: "invalid_json", Message: "extraneous content after request object"}
	}

	if raw.SampleRate == nil {
		return nil, &FieldError{
			Error: "validation_failed", Field: "sample_rate",
			Constraint: "required", Message: "sample_rate is required",
		}
	}
	sampleRate, ferr := parseSampleRate(*raw.SampleRate)
	if ferr != nil {
		return nil, ferr
	}

	if raw.Amplitudes == nil {
		return nil, &FieldError{
			Error: "validation_failed", Field: "amplitudes",
			Constraint: "required", Message: "amplitudes is required",
		}
	}
	amplitudes, ferr := parseAmplitudes(*raw.Amplitudes)
	if ferr != nil {
		return nil, ferr
	}

	var ranges []pulse.Range
	if raw.ExcludedRanges != nil {
		ranges, ferr = parseExcludedRanges(*raw.ExcludedRanges, len(amplitudes))
		if ferr != nil {
			return nil, ferr
		}
	}

	includeMetrics := false
	if len(raw.IncludeMetrics) != 0 {
		includeMetrics, ferr = parseIncludeMetrics(raw.IncludeMetrics)
		if ferr != nil {
			return nil, ferr
		}
	}

	return &AnalyzeRequest{
		SampleRate:     sampleRate,
		Amplitudes:     amplitudes,
		ExcludedRanges: ranges,
		IncludeMetrics: includeMetrics,
	}, nil
}

func parseSampleRate(token json.RawMessage) (float64, *FieldError) {
	if isNull(token) {
		return 0, &FieldError{
			Error: "validation_failed", Field: "sample_rate",
			Constraint: "type", Message: "sample_rate must be a finite number",
		}
	}
	var v float64
	if err := json.Unmarshal(token, &v); err != nil {
		return 0, &FieldError{
			Error: "validation_failed", Field: "sample_rate",
			Constraint: "type", Message: "sample_rate must be a finite JSON number",
		}
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, &FieldError{
			Error: "validation_failed", Field: "sample_rate",
			Constraint: "finite", Message: "sample_rate must be finite",
		}
	}
	if v < pulse.MinSampleRate || v > pulse.MaxSampleRate {
		return 0, &FieldError{
			Error: "validation_failed", Field: "sample_rate",
			Constraint: "range",
			Message: fmt.Sprintf("sample_rate must be within [%.0f, %.0f] Hz",
				pulse.MinSampleRate, pulse.MaxSampleRate),
		}
	}
	return v, nil
}

func parseAmplitudes(token json.RawMessage) ([]float64, *FieldError) {
	if isNull(token) {
		return nil, &FieldError{
			Error: "validation_failed", Field: "amplitudes",
			Constraint: "type", Message: "amplitudes must be an array of finite numbers",
		}
	}

	var rawItems []json.RawMessage
	if err := json.Unmarshal(token, &rawItems); err != nil {
		return nil, &FieldError{
			Error: "validation_failed", Field: "amplitudes",
			Constraint: "type", Message: "amplitudes must be a JSON array",
		}
	}

	n := len(rawItems)
	if n < pulse.MinSamples {
		return nil, &FieldError{
			Error: "validation_failed", Field: "amplitudes",
			Constraint: "min_length",
			Message: fmt.Sprintf("amplitudes must contain at least %d samples, got %d",
				pulse.MinSamples, n),
		}
	}
	if n > pulse.MaxSamples {
		return nil, &FieldError{
			Error: "validation_failed", Field: "amplitudes",
			Constraint: "max_length",
			Message: fmt.Sprintf("amplitudes must contain at most %d samples, got %d",
				pulse.MaxSamples, n),
		}
	}

	values := make([]float64, n)
	for i, item := range rawItems {
		if isNull(item) {
			return nil, amplitudeTypeError(i)
		}
		if err := json.Unmarshal(item, &values[i]); err != nil {
			return nil, amplitudeTypeError(i)
		}
		if math.IsNaN(values[i]) || math.IsInf(values[i], 0) {
			return nil, &FieldError{
				Error: "validation_failed", Field: "amplitudes",
				Index: intPtr(i), Constraint: "finite",
				Message: fmt.Sprintf("amplitudes[%d] must be finite", i),
			}
		}
	}
	return values, nil
}

func amplitudeTypeError(i int) *FieldError {
	return &FieldError{
		Error: "validation_failed", Field: "amplitudes",
		Index: intPtr(i), Constraint: "type",
		Message: fmt.Sprintf("amplitudes[%d] must be a finite JSON number", i),
	}
}

// parseIncludeMetrics converts the optional metrics switch. Only a JSON
// boolean is accepted: an explicit null, like any other non-boolean token,
// is a field-level type error (unmarshaling null into a bool would
// otherwise silently succeed). An absent key never reaches here.
func parseIncludeMetrics(token json.RawMessage) (bool, *FieldError) {
	if isNull(token) {
		return false, includeMetricsTypeError()
	}
	var v bool
	if err := json.Unmarshal(token, &v); err != nil {
		return false, includeMetricsTypeError()
	}
	return v, nil
}

func includeMetricsTypeError() *FieldError {
	return &FieldError{
		Error: "validation_failed", Field: "include_metrics",
		Constraint: "type", Message: "include_metrics must be a JSON boolean",
	}
}

// parseExcludedRanges parses and validates excluded_ranges against an
// amplitudes slice of length n. Every error is located at the offending
// element index; structural errors (a non-array value) are field-level.
// A JSON null never reaches here: it leaves the outer pointer nil and is
// treated as "field omitted".
func parseExcludedRanges(token json.RawMessage, n int) ([]pulse.Range, *FieldError) {
	var rawItems []json.RawMessage
	if err := json.Unmarshal(token, &rawItems); err != nil {
		return nil, &FieldError{
			Error: "validation_failed", Field: "excluded_ranges",
			Constraint: "type", Message: "excluded_ranges must be a JSON array",
		}
	}

	ranges := make([]pulse.Range, len(rawItems))
	for i, item := range rawItems {
		r, ferr := parseRangeElement(item, i)
		if ferr != nil {
			return nil, ferr
		}
		ranges[i] = r
	}

	if verr := pulse.ValidateRanges(ranges, n); verr != nil {
		return nil, &FieldError{
			Error: "validation_failed", Field: "excluded_ranges",
			Index: intPtr(verr.Index), Constraint: verr.Constraint,
			Message: verr.Message,
		}
	}
	return ranges, nil
}

// parseRangeElement decodes one excluded_ranges element into a Range. Keys
// are read in document order and matched case-sensitively: a non-contract
// name (such as "Start") is an unknown field of the element, an absent
// endpoint is a required error, and an explicit null endpoint is a type
// error — null is not an integer.
func parseRangeElement(item json.RawMessage, i int) (pulse.Range, *FieldError) {
	if isNull(item) {
		return pulse.Range{}, excludedTypeError(i, "excluded_ranges[%d] must be an object, got null", i)
	}

	notObject := func() *FieldError {
		return excludedTypeError(i, "excluded_ranges[%d] must be a {start, end} object", i)
	}

	dec := json.NewDecoder(bytes.NewReader(item))
	tok, err := dec.Token()
	if err != nil {
		return pulse.Range{}, notObject()
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return pulse.Range{}, notObject()
	}

	var start, end json.RawMessage
	haveStart, haveEnd := false, false
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return pulse.Range{}, notObject()
		}
		key, _ := kt.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return pulse.Range{}, notObject()
		}
		switch key {
		case "start":
			start, haveStart = raw, true
		case "end":
			end, haveEnd = raw, true
		default:
			return pulse.Range{}, &FieldError{
				Error: "validation_failed", Field: "excluded_ranges",
				Index: intPtr(i), Constraint: "unknown",
				Message: fmt.Sprintf("excluded_ranges[%d] has unknown field %q", i, key),
			}
		}
	}
	if _, err := dec.Token(); err != nil { // consume the closing '}'
		return pulse.Range{}, notObject()
	}

	if !haveStart {
		return pulse.Range{}, excludedRequiredError(i, "start")
	}
	if !haveEnd {
		return pulse.Range{}, excludedRequiredError(i, "end")
	}

	s, ferr := parseRangeEndpoint(i, "start", start)
	if ferr != nil {
		return pulse.Range{}, ferr
	}
	e, ferr := parseRangeEndpoint(i, "end", end)
	if ferr != nil {
		return pulse.Range{}, ferr
	}
	return pulse.Range{Start: s, End: e}, nil
}

// parseRangeEndpoint converts one raw endpoint value to an int. An explicit
// null, a non-number and a non-integer number are all type errors located at
// the element: an endpoint must be an integer JSON number.
func parseRangeEndpoint(i int, name string, raw json.RawMessage) (int, *FieldError) {
	if isNull(raw) {
		return 0, excludedTypeError(i,
			"excluded_ranges[%d]."+name+" must be an integer JSON number", i)
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, excludedTypeError(i,
			"excluded_ranges[%d]."+name+" must be an integer JSON number", i)
	}
	if !isJSONInteger(v) {
		return 0, excludedTypeError(i,
			"excluded_ranges[%d] start and end must be integer sample indices", i)
	}
	return int(v), nil
}

func excludedTypeError(i int, format string, args ...any) *FieldError {
	return &FieldError{
		Error: "validation_failed", Field: "excluded_ranges",
		Index: intPtr(i), Constraint: "type",
		Message: fmt.Sprintf(format, args...),
	}
}

func excludedRequiredError(i int, endpoint string) *FieldError {
	return &FieldError{
		Error: "validation_failed", Field: "excluded_ranges",
		Index: intPtr(i), Constraint: "required",
		Message: fmt.Sprintf("excluded_ranges[%d].%s is required", i, endpoint),
	}
}

// isJSONInteger reports whether a decoded finite JSON number is integral.
func isJSONInteger(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Trunc(v) == v
}

// firstNonContractKey walks the top-level object of body in document order
// and returns the first key that is not exactly one of the allowed contract
// names. encoding/json matches struct tags case-insensitively, so without
// this walk a case variant such as "Sample_rate" would be silently accepted
// even with DisallowUnknownFields. A non-object or malformed body yields no
// key: the struct decode that follows reports it instead.
func firstNonContractKey(body []byte, allowed ...string) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, _ := kt.(string)
		if !slices.Contains(allowed, key) {
			return key, true
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return "", false
		}
	}
	return "", false
}

// unknownFieldError is the contract rejection of a non-contract field name;
// field is the located (possibly side-prefixed) name, name the bare key.
func unknownFieldError(field, name string) *FieldError {
	return &FieldError{
		Error: "validation_failed", Field: field,
		Constraint: "unknown", Message: fmt.Sprintf("unknown field %q", name),
	}
}

// decodeError translates a JSON decode error into a located FieldError.
func decodeError(err error) *FieldError {
	msg := err.Error()
	if strings.HasPrefix(msg, "json: unknown field ") {
		name := strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
		return unknownFieldError(name, name)
	}
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		return &FieldError{
			Error:      "invalid_json",
			Field:      ute.Field,
			Constraint: "type",
			Message:    msg,
		}
	}
	return &FieldError{Error: "invalid_json", Message: msg}
}

func isNull(token json.RawMessage) bool {
	return string(bytes.TrimSpace(token)) == "null"
}

func intPtr(i int) *int { return &i }

// frame is one level of the lightweight JSON walk used to locate NaN/Infinity.
type frame struct {
	kind byte // '{' or '['
	// key is the object key this container is the value of.
	key string
	// idx counts elements while kind == '['.
	idx int
}

// nonFiniteLocator maps a NaN/Infinity token found by the JSON walk to a
// located field error. It receives the container stack and the pending
// object key the token is the value of.
type nonFiniteLocator func(stack []frame, pendingKey string) *FieldError

// locateNonFinite finds the first NaN/Infinity/-Infinity literal outside of a
// JSON string and asks locate to report it. Valid JSON has no such literals,
// so a nil result means nothing to report.
func locateNonFinite(body []byte, locate nonFiniteLocator) *FieldError {
	var stack []frame
	pendingKey := ""

	i, n := 0, len(body)
	for i < n {
		c := body[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"':
			s, end, ok := scanString(body, i)
			if !ok {
				return nil // malformed string; let the standard decoder report it
			}
			// A string directly inside an object and followed by ':' is a key.
			if len(stack) > 0 && stack[len(stack)-1].kind == '{' &&
				nextToken(body, end) == ':' {
				pendingKey = s
			}
			i = end
		case c == '{' || c == '[':
			stack = append(stack, frame{kind: c, key: pendingKey})
			pendingKey = ""
			i++
		case c == '}' || c == ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			pendingKey = ""
			i++
		case c == ',':
			if len(stack) > 0 {
				top := &stack[len(stack)-1]
				if top.kind == '[' {
					top.idx++
				}
			}
			i++
		case c == ':':
			i++
		case c == 'N' && i+3 <= n && string(body[i:i+3]) == "NaN":
			return locate(stack, pendingKey)
		case c == 'I' && i+8 <= n && string(body[i:i+8]) == "Infinity":
			return locate(stack, pendingKey)
		case c == '-' && i+9 <= n && string(body[i:i+9]) == "-Infinity":
			return locate(stack, pendingKey)
		default:
			// Skip ordinary numbers/keywords/unknown chars; structural
			// validity is checked by encoding/json afterwards.
			i++
		}
	}
	return nil
}

// nonFiniteAnalyze maps a NaN/Infinity token in a single-channel analyze
// request to its located field error.
func nonFiniteAnalyze(stack []frame, pendingKey string) *FieldError {
	if len(stack) > 0 {
		top := stack[len(stack)-1]
		if top.kind == '[' && top.key == "amplitudes" {
			return &FieldError{
				Error: "validation_failed", Field: "amplitudes",
				Index: intPtr(top.idx), Constraint: "finite",
				Message: fmt.Sprintf("amplitudes[%d] must be finite", top.idx),
			}
		}
		if top.kind == '{' && pendingKey == "sample_rate" {
			return &FieldError{
				Error: "validation_failed", Field: "sample_rate",
				Constraint: "finite", Message: "sample_rate must be finite",
			}
		}
		// A non-finite literal where only a boolean is legal.
		if top.kind == '{' && pendingKey == "include_metrics" {
			return &FieldError{
				Error: "validation_failed", Field: "include_metrics",
				Constraint: "type", Message: "include_metrics must be a JSON boolean",
			}
		}
		// A start/end value inside an excluded_ranges element object.
		if top.kind == '{' && (pendingKey == "start" || pendingKey == "end") &&
			len(stack) >= 2 {
			if parent := stack[len(stack)-2]; parent.kind == '[' && parent.key == "excluded_ranges" {
				return &FieldError{
					Error: "validation_failed", Field: "excluded_ranges",
					Index: intPtr(parent.idx), Constraint: "type",
					Message: fmt.Sprintf(
						"excluded_ranges[%d].%s must be an integer JSON number",
						parent.idx, pendingKey),
				}
			}
		}
		// A bare non-finite literal where an element object was expected.
		if top.kind == '[' && top.key == "excluded_ranges" {
			return &FieldError{
				Error: "validation_failed", Field: "excluded_ranges",
				Index: intPtr(top.idx), Constraint: "type",
				Message: fmt.Sprintf(
					"excluded_ranges[%d] must be a {start, end} object", top.idx),
			}
		}
	}
	return &FieldError{Error: "invalid_json", Message: "invalid numeric literal"}
}

// scanString returns the decoded string and the index just past the quote.
func scanString(body []byte, from int) (string, int, bool) {
	// from points at the opening quote.
	i := from + 1
	for i < len(body) {
		switch body[i] {
		case '\\':
			i += 2
			continue
		case '"':
			var s string
			if err := json.Unmarshal(body[from:i+1], &s); err != nil {
				return "", i, false
			}
			return s, i + 1, true
		}
		i++
	}
	return "", i, false
}

// nextToken returns the first non-whitespace byte at or after from.
func nextToken(body []byte, from int) byte {
	for from < len(body) {
		switch body[from] {
		case ' ', '\t', '\n', '\r':
			from++
			continue
		}
		return body[from]
	}
	return 0
}
