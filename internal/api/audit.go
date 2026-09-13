package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"

	"github.com/gin-gonic/gin"

	"trackside-pulse-api/internal/pulse"
)

// AuditResponse is the acquisition-quality report contract; it mirrors the
// domain report field-for-field so an audit never depends on pulse analysis.
type AuditResponse = pulse.AcquisitionAudit

// rawAuditRequest keeps the fields as raw value tokens: a zero-length
// RawMessage means "key absent" (a required error), while an explicit null
// decodes to the four bytes "null" and is therefore distinguishable as a
// type error — the same distinction the analysis contract draws for
// include_metrics.
type rawAuditRequest struct {
	FullScale json.RawMessage `json:"full_scale"`
	Samples   json.RawMessage `json:"samples"`
}

func acquisitionAuditHandler(c *gin.Context) {
	fullScale, samples, ferr := decodeAuditRequest(c.Request)
	if ferr != nil {
		c.JSON(ferr.HTTPStatus(), ferr)
		return
	}

	// Validation is complete before any report is built: a rejected request
	// never returns a partial audit.
	audit := pulse.AuditAcquisition(samples, fullScale)
	c.JSON(http.StatusOK, audit)
}

// decodeAuditRequest parses and validates an acquisition-audit body, locating
// every error at its field or sample index through the existing envelope.
func decodeAuditRequest(r *http.Request) (float64, []float64, *FieldError) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return 0, nil, &FieldError{Error: "body_too_large", Message: "request body exceeds 4 MiB"}
		}
		return 0, nil, &FieldError{Error: "invalid_body", Message: "cannot read request body"}
	}

	// Locate NaN/Infinity tokens first so the error points at full_scale or
	// at the offending sample index, ahead of the strict JSON decoder.
	if ferr := locateNonFinite(body, nonFiniteAudit); ferr != nil {
		return 0, nil, ferr
	}

	// Field names are a case-sensitive contract, same as on analysis.
	if key, found := firstNonContractKey(body, "full_scale", "samples"); found {
		return 0, nil, unknownFieldError(key, key)
	}

	var raw rawAuditRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return 0, nil, decodeError(err)
	}
	if tok := dec.Decode(&struct{}{}); !errors.Is(tok, io.EOF) {
		return 0, nil, &FieldError{Error: "invalid_json", Message: "extraneous content after request object"}
	}

	if len(raw.FullScale) == 0 {
		return 0, nil, &FieldError{
			Error: "validation_failed", Field: "full_scale",
			Constraint: "required", Message: "full_scale is required",
		}
	}
	fullScale, ferr := parseFullScale(raw.FullScale)
	if ferr != nil {
		return 0, nil, ferr
	}

	if len(raw.Samples) == 0 {
		return 0, nil, &FieldError{
			Error: "validation_failed", Field: "samples",
			Constraint: "required", Message: "samples is required",
		}
	}
	samples, ferr := parseAuditSamples(raw.Samples)
	if ferr != nil {
		return 0, nil, ferr
	}

	return fullScale, samples, nil
}

// parseFullScale converts the raw full-scale value: it must be a finite,
// strictly positive JSON number. Null and non-number tokens are type errors;
// NaN/Infinity are finite errors; zero and negatives are range errors.
func parseFullScale(token json.RawMessage) (float64, *FieldError) {
	if isNull(token) {
		return 0, &FieldError{
			Error: "validation_failed", Field: "full_scale",
			Constraint: "type", Message: "full_scale must be a finite number",
		}
	}
	var v float64
	if err := json.Unmarshal(token, &v); err != nil {
		return 0, &FieldError{
			Error: "validation_failed", Field: "full_scale",
			Constraint: "type", Message: "full_scale must be a finite JSON number",
		}
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, &FieldError{
			Error: "validation_failed", Field: "full_scale",
			Constraint: "finite", Message: "full_scale must be finite",
		}
	}
	if v <= 0 {
		return 0, &FieldError{
			Error: "validation_failed", Field: "full_scale",
			Constraint: "range", Message: "full_scale must be a positive number greater than 0",
		}
	}
	return v, nil
}

// parseAuditSamples parses the samples array with the audit length contract
// [AuditMinSamples, AuditMaxSamples] and finite per-element floats. Every
// error is located at the samples field or at one sample index.
func parseAuditSamples(token json.RawMessage) ([]float64, *FieldError) {
	if isNull(token) {
		return nil, &FieldError{
			Error: "validation_failed", Field: "samples",
			Constraint: "type", Message: "samples must be an array of finite numbers",
		}
	}

	var rawItems []json.RawMessage
	if err := json.Unmarshal(token, &rawItems); err != nil {
		return nil, &FieldError{
			Error: "validation_failed", Field: "samples",
			Constraint: "type", Message: "samples must be a JSON array",
		}
	}

	n := len(rawItems)
	if n < pulse.AuditMinSamples {
		return nil, &FieldError{
			Error: "validation_failed", Field: "samples",
			Constraint: "min_length",
			Message: fmt.Sprintf("samples must contain at least %d samples, got %d",
				pulse.AuditMinSamples, n),
		}
	}
	if n > pulse.AuditMaxSamples {
		return nil, &FieldError{
			Error: "validation_failed", Field: "samples",
			Constraint: "max_length",
			Message: fmt.Sprintf("samples must contain at most %d samples, got %d",
				pulse.AuditMaxSamples, n),
		}
	}

	values := make([]float64, n)
	for i, item := range rawItems {
		if isNull(item) {
			return nil, sampleTypeError(i)
		}
		if err := json.Unmarshal(item, &values[i]); err != nil {
			return nil, sampleTypeError(i)
		}
		if math.IsNaN(values[i]) || math.IsInf(values[i], 0) {
			return nil, &FieldError{
				Error: "validation_failed", Field: "samples",
				Index: intPtr(i), Constraint: "finite",
				Message: fmt.Sprintf("samples[%d] must be finite", i),
			}
		}
	}
	return values, nil
}

func sampleTypeError(i int) *FieldError {
	return &FieldError{
		Error: "validation_failed", Field: "samples",
		Index: intPtr(i), Constraint: "type",
		Message: fmt.Sprintf("samples[%d] must be a finite JSON number", i),
	}
}

// nonFiniteAudit maps a NaN/Infinity token in an audit request to its
// located field error: an element of samples carries the sample index, a
// full_scale value is a field-level finite error.
func nonFiniteAudit(stack []frame, pendingKey string) *FieldError {
	if len(stack) > 0 {
		top := stack[len(stack)-1]
		if top.kind == '[' && top.key == "samples" {
			return &FieldError{
				Error: "validation_failed", Field: "samples",
				Index: intPtr(top.idx), Constraint: "finite",
				Message: fmt.Sprintf("samples[%d] must be finite", top.idx),
			}
		}
		if top.kind == '{' && pendingKey == "full_scale" {
			return &FieldError{
				Error: "validation_failed", Field: "full_scale",
				Constraint: "finite", Message: "full_scale must be finite",
			}
		}
	}
	return &FieldError{Error: "invalid_json", Message: "invalid numeric literal"}
}
