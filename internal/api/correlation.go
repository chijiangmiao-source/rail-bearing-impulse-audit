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

// CorrelateRequest is the JSON contract of the dual-channel review. Each
// rail keeps its own single-channel sampling configuration; the two
// pickers must sample at the same rate and the peak-time tolerance is an
// integer number of samples.
type CorrelateRequest struct {
	ToleranceSamples int                     `json:"tolerance_samples"`
	Left             CorrelateChannelRequest `json:"left"`
	Right            CorrelateChannelRequest `json:"right"`
}

// CorrelateChannelRequest is one side of a dual-channel review and has
// exactly the single-channel request contract.
type CorrelateChannelRequest struct {
	SampleRate     float64       `json:"sample_rate"`
	Amplitudes     []float64     `json:"amplitudes"`
	ExcludedRanges []pulse.Range `json:"excluded_ranges,omitempty"`
}

// CorrelateResponse preserves both channels' original single-channel
// analyses (echoed under left/right) and adds the paired evidence plus the
// still-unpaired pulse indices.
type CorrelateResponse struct {
	SampleRate       float64                  `json:"sample_rate"`
	ToleranceSamples int                      `json:"tolerance_samples"`
	Left             CorrelateChannelResponse `json:"left"`
	Right            CorrelateChannelResponse `json:"right"`
	Pairs            []pulse.Pair             `json:"pairs"`
	LeftUnpaired     []int                    `json:"left_unpaired"`
	RightUnpaired    []int                    `json:"right_unpaired"`
}

// CorrelateChannelResponse is one channel's original analysis result,
// unchanged in shape from POST /pulses/analyze.
type CorrelateChannelResponse struct {
	SampleRate     float64       `json:"sample_rate"`
	Decidable      bool          `json:"decidable"`
	Baseline       float64       `json:"baseline"`
	Reason         string        `json:"reason,omitempty"`
	Pulses         []pulse.Pulse `json:"pulses"`
	ExcludedRanges []pulse.Range `json:"excluded_ranges,omitempty"`
}

// rawCorrelateRequest keeps the fields as raw value tokens: a zero-length
// RawMessage means "key absent", while an explicit null decodes to the
// four bytes "null" and is therefore distinguishable as a type error.
type rawCorrelateRequest struct {
	ToleranceSamples json.RawMessage `json:"tolerance_samples"`
	Left             json.RawMessage `json:"left"`
	Right            json.RawMessage `json:"right"`
}

// rawChannelRequest is one dual-channel side. It shares the single-channel
// fields but deliberately not include_metrics: the correlate contract has
// no metrics switch and its responses never carry metric fields, so the
// key stays unknown here and is rejected as such.
type rawChannelRequest struct {
	SampleRate     *json.RawMessage `json:"sample_rate"`
	Amplitudes     *json.RawMessage `json:"amplitudes"`
	ExcludedRanges *json.RawMessage `json:"excluded_ranges"`
}

func correlateHandler(c *gin.Context) {
	req, ferr := decodeCorrelateRequest(c.Request)
	if ferr != nil {
		c.JSON(ferr.HTTPStatus(), ferr)
		return
	}

	result := pulse.Correlate(pulse.CorrelationRequest{
		SampleRate:       req.Left.SampleRate,
		ToleranceSamples: req.ToleranceSamples,
		Left: pulse.ChannelInput{
			Amplitudes:     req.Left.Amplitudes,
			ExcludedRanges: req.Left.ExcludedRanges,
		},
		Right: pulse.ChannelInput{
			Amplitudes:     req.Right.Amplitudes,
			ExcludedRanges: req.Right.ExcludedRanges,
		},
	})

	c.JSON(http.StatusOK, CorrelateResponse{
		SampleRate:       req.Left.SampleRate,
		ToleranceSamples: req.ToleranceSamples,
		Left:             channelResponse(req.Left, result.Left),
		Right:            channelResponse(req.Right, result.Right),
		Pairs:            result.Pairs,
		LeftUnpaired:     result.LeftUnpaired,
		RightUnpaired:    result.RightUnpaired,
	})
}

func channelResponse(req CorrelateChannelRequest, r pulse.Result) CorrelateChannelResponse {
	return CorrelateChannelResponse{
		SampleRate:     req.SampleRate,
		Decidable:      r.Decidable,
		Baseline:       r.Baseline,
		Reason:         r.Reason,
		Pulses:         r.Pulses,
		ExcludedRanges: req.ExcludedRanges,
	}
}

// decodeCorrelateRequest parses and validates the body. Every error keeps
// the existing single-channel envelope, with field names prefixed by
// "left."/"right." (nested sample/range indices are preserved); a
// rejected request never produces a partial response.
func decodeCorrelateRequest(r *http.Request) (*CorrelateRequest, *FieldError) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &FieldError{Error: "body_too_large", Message: "request body exceeds 4 MiB"}
		}
		return nil, &FieldError{Error: "invalid_body", Message: "cannot read request body"}
	}

	// Locate NaN/Infinity tokens first, prefixed by their side, so the
	// error points at the field or the sample/range index.
	if ferr := locateNonFinite(body, nonFiniteCorrelate); ferr != nil {
		return nil, ferr
	}

	// The top-level names are a case-sensitive contract too: a variant like
	// "Tolerance_Samples" or "Left" is an unknown field, never folded onto
	// the contract name by encoding/json's case-insensitive matching.
	if key, found := firstNonContractKey(body, "tolerance_samples", "left", "right"); found {
		return nil, unknownFieldError(key, key)
	}

	var raw rawCorrelateRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, decodeError(err)
	}
	if tok := dec.Decode(&struct{}{}); !errors.Is(tok, io.EOF) {
		return nil, &FieldError{Error: "invalid_json", Message: "extraneous content after request object"}
	}

	if len(raw.ToleranceSamples) == 0 {
		return nil, &FieldError{
			Error: "validation_failed", Field: "tolerance_samples",
			Constraint: "required", Message: "tolerance_samples is required",
		}
	}
	tolerance, ferr := parseTolerance(raw.ToleranceSamples)
	if ferr != nil {
		return nil, ferr
	}

	left, ferr := decodeCorrelateChannel("left", raw.Left)
	if ferr != nil {
		return nil, ferr
	}
	right, ferr := decodeCorrelateChannel("right", raw.Right)
	if ferr != nil {
		return nil, ferr
	}

	if left.SampleRate != right.SampleRate {
		return nil, &FieldError{
			Error: "validation_failed", Field: "right.sample_rate",
			Constraint: "sample_rate_mismatch",
			Message: fmt.Sprintf(
				"right.sample_rate %g must equal left.sample_rate %g",
				right.SampleRate, left.SampleRate),
		}
	}

	return &CorrelateRequest{
		ToleranceSamples: tolerance,
		Left:             *left,
		Right:            *right,
	}, nil
}

// decodeCorrelateChannel validates one side exactly as the single-channel
// decoder does, prefixing every field name with the side.
func decodeCorrelateChannel(side string, token json.RawMessage) (*CorrelateChannelRequest, *FieldError) {
	prefix := func(field string) string { return side + "." + field }

	if len(token) == 0 {
		return nil, &FieldError{
			Error: "validation_failed", Field: side,
			Constraint: "required", Message: side + " is required",
		}
	}
	if isNull(token) {
		return nil, sideObjectTypeError(side)
	}

	// Side field names follow the same case-sensitive contract; a variant
	// like "Sample_rate" is an unknown field located with the side prefix.
	if key, found := firstNonContractKey(token,
		"sample_rate", "amplitudes", "excluded_ranges"); found {
		return nil, unknownFieldError(side+"."+key, key)
	}

	var raw rawChannelRequest
	dec := json.NewDecoder(bytes.NewReader(token))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, channelDecodeError(side, err)
	}
	if tok := dec.Decode(&struct{}{}); !errors.Is(tok, io.EOF) {
		return nil, &FieldError{
			Error: "validation_failed", Field: side,
			Constraint: "type", Message: side + " must be a single JSON object",
		}
	}

	if raw.SampleRate == nil {
		return nil, &FieldError{
			Error: "validation_failed", Field: prefix("sample_rate"),
			Constraint: "required", Message: "sample_rate is required",
		}
	}
	sampleRate, ferr := parseSampleRate(*raw.SampleRate)
	if ferr != nil {
		return nil, prefixFieldError(ferr, side)
	}

	if raw.Amplitudes == nil {
		return nil, &FieldError{
			Error: "validation_failed", Field: prefix("amplitudes"),
			Constraint: "required", Message: "amplitudes is required",
		}
	}
	amplitudes, ferr := parseAmplitudes(*raw.Amplitudes)
	if ferr != nil {
		return nil, prefixFieldError(ferr, side)
	}

	var ranges []pulse.Range
	if raw.ExcludedRanges != nil {
		ranges, ferr = parseExcludedRanges(*raw.ExcludedRanges, len(amplitudes))
		if ferr != nil {
			// Range errors already carry the offending element's index;
			// only the field name becomes side-prefixed.
			return nil, prefixFieldError(ferr, side)
		}
	}

	return &CorrelateChannelRequest{
		SampleRate:     sampleRate,
		Amplitudes:     amplitudes,
		ExcludedRanges: ranges,
	}, nil
}

// parseTolerance converts the raw tolerance into an integer within
// [MinToleranceSamples, MaxToleranceSamples]. Null, a non-number, a
// non-integer number and an out-of-range integer are all rejected with
// field-level errors; zero is explicitly legal.
func parseTolerance(token json.RawMessage) (int, *FieldError) {
	if isNull(token) {
		return 0, toleranceTypeError()
	}
	var v float64
	if err := json.Unmarshal(token, &v); err != nil {
		return 0, toleranceTypeError()
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || !isJSONInteger(v) {
		return 0, toleranceTypeError()
	}
	if v < pulse.MinToleranceSamples || v > pulse.MaxToleranceSamples {
		return 0, &FieldError{
			Error: "validation_failed", Field: "tolerance_samples",
			Constraint: "range",
			Message: fmt.Sprintf("tolerance_samples must be an integer within [%d, %d]",
				pulse.MinToleranceSamples, pulse.MaxToleranceSamples),
		}
	}
	return int(v), nil
}

func toleranceTypeError() *FieldError {
	return &FieldError{
		Error: "validation_failed", Field: "tolerance_samples",
		Constraint: "type",
		Message: fmt.Sprintf("tolerance_samples must be an integer JSON number within [%d, %d]",
			pulse.MinToleranceSamples, pulse.MaxToleranceSamples),
	}
}

// prefixFieldError rewrites a single-channel error into a side-prefixed
// one. The element/sample index and every other attribute are preserved;
// a document-level error without a field stays as is.
func prefixFieldError(e *FieldError, side string) *FieldError {
	if e.Field == "" {
		return e
	}
	cp := *e
	cp.Field = side + "." + e.Field
	return &cp
}

// channelDecodeError maps a structural decode error inside one side
// object. A non-object side value (a scalar, an array, null handled
// earlier) is a type error at the side; an unknown field keeps its name
// prefixed by the side.
func channelDecodeError(side string, err error) *FieldError {
	msg := err.Error()
	const unknownPrefix = "json: unknown field "
	if strings.HasPrefix(msg, unknownPrefix) {
		name := strings.Trim(strings.TrimPrefix(msg, unknownPrefix), `"`)
		return unknownFieldError(side+"."+name, name)
	}
	return sideObjectTypeError(side)
}

func sideObjectTypeError(side string) *FieldError {
	return &FieldError{
		Error: "validation_failed", Field: side,
		Constraint: "type",
		Message:    side + " must be a {sample_rate, amplitudes, excluded_ranges} object",
	}
}

// nonFiniteCorrelate maps a NaN/Infinity token in a dual-channel request
// to its side-prefixed field or sample/range index.
func nonFiniteCorrelate(stack []frame, pendingKey string) *FieldError {
	side, localStack, localKey := correlateContext(stack, pendingKey)
	if side == "" {
		switch pendingKey {
		case "tolerance_samples":
			return toleranceTypeError()
		case "left", "right":
			return sideObjectTypeError(pendingKey)
		default:
			return &FieldError{Error: "invalid_json", Message: "invalid numeric literal"}
		}
	}

	// Reuse the single-channel mapping on the stack rooted at the side
	// object, then translate the reported field to the dotted name.
	ferr := nonFiniteAnalyze(localStack, localKey)
	if ferr == nil {
		return &FieldError{Error: "invalid_json", Message: "invalid numeric literal"}
	}
	// A non-finite token inside a structurally wrong side (e.g. an array)
	// is a side-level type error, not a document-level JSON error.
	if ferr.Error == "invalid_json" {
		return sideObjectTypeError(side)
	}
	switch ferr.Field {
	case "sample_rate", "amplitudes", "excluded_ranges":
		ferr.Field = side + "." + ferr.Field
	default:
		ferr.Field = side
	}
	return ferr
}

// correlateContext finds the nearest enclosing container keyed "left" or
// "right" and returns the side together with a virtual stack and pending
// key rooted at the side object, shaped as the single-channel locator
// expects.
func correlateContext(stack []frame, pendingKey string) (string, []frame, string) {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].key != "left" && stack[i].key != "right" {
			continue
		}
		side := stack[i].key
		local := make([]frame, 0, len(stack)-i)
		local = append(local, frame{kind: '{'})
		local = append(local, stack[i+1:]...)
		return side, local, pendingKey
	}
	return "", stack, pendingKey
}
