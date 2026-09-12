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
	"strings"

	"github.com/gin-gonic/gin"

	"trackside-pulse-api/internal/pulse"
)

// maxBodyBytes bounds accepted request bodies (20000 doubles need ~0.5 MiB).
const maxBodyBytes = 4 << 20

// AnalyzeRequest is the JSON request contract.
type AnalyzeRequest struct {
	SampleRate float64   `json:"sample_rate"`
	Amplitudes []float64 `json:"amplitudes"`
}

// AnalyzeResponse is the full, deterministic response contract.
type AnalyzeResponse struct {
	SampleRate float64       `json:"sample_rate"`
	Decidable  bool          `json:"decidable"`
	Baseline   float64       `json:"baseline"`
	Reason     string        `json:"reason,omitempty"`
	Pulses     []pulse.Pulse `json:"pulses"`
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

// rawAnalyzeRequest keeps the two fields as raw tokens so type errors can be
// attributed to the field or to an individual amplitude index.
type rawAnalyzeRequest struct {
	SampleRate *json.RawMessage `json:"sample_rate"`
	Amplitudes *json.RawMessage `json:"amplitudes"`
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
	}
	return r
}

func analyzeHandler(c *gin.Context) {
	req, ferr := decodeAnalyzeRequest(c.Request)
	if ferr != nil {
		c.JSON(ferr.HTTPStatus(), ferr)
		return
	}

	result := pulse.Analyze(req.Amplitudes)
	c.JSON(http.StatusOK, AnalyzeResponse{
		SampleRate: req.SampleRate,
		Decidable:  result.Decidable,
		Baseline:   result.Baseline,
		Reason:     result.Reason,
		Pulses:     result.Pulses,
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
	if ferr := locateNonFinite(body); ferr != nil {
		return nil, ferr
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

	return &AnalyzeRequest{SampleRate: sampleRate, Amplitudes: amplitudes}, nil
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

// decodeError translates a JSON decode error into a located FieldError.
func decodeError(err error) *FieldError {
	msg := err.Error()
	if strings.HasPrefix(msg, "json: unknown field ") {
		name := strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
		return &FieldError{
			Error: "validation_failed", Field: name,
			Constraint: "unknown", Message: fmt.Sprintf("unknown field %q", name),
		}
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

// locateNonFinite finds the first NaN/Infinity/-Infinity literal outside of a
// JSON string and returns an error located at the field or sample index.
// Valid JSON has no such literals, so a nil result means nothing to report.
func locateNonFinite(body []byte) *FieldError {
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
			return nonFiniteError(stack, pendingKey)
		case c == 'I' && i+8 <= n && string(body[i:i+8]) == "Infinity":
			return nonFiniteError(stack, pendingKey)
		case c == '-' && i+9 <= n && string(body[i:i+9]) == "-Infinity":
			return nonFiniteError(stack, pendingKey)
		default:
			// Skip ordinary numbers/keywords/unknown chars; structural
			// validity is checked by encoding/json afterwards.
			i++
		}
	}
	return nil
}

// nonFiniteError maps the current walk position to a located field error.
func nonFiniteError(stack []frame, pendingKey string) *FieldError {
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
