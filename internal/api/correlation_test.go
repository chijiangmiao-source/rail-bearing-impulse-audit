package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const correlatePath = "/api/v1/pulses/correlate"

func postCorrelate(t *testing.T, r http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, correlatePath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func postCorrelateRaw(t *testing.T, r http.Handler, raw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, correlatePath, strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type correlateChannelDTO struct {
	SampleRate     float64       `json:"sample_rate"`
	Decidable      bool          `json:"decidable"`
	Baseline       float64       `json:"baseline"`
	Reason         string        `json:"reason"`
	Pulses         []pulseLike   `json:"pulses"`
	ExcludedRanges []rawRangeDTO `json:"excluded_ranges"`
}

type pulseLike struct {
	Start     int     `json:"start"`
	End       int     `json:"end"`
	PeakIndex int     `json:"peak_index"`
	Peak      float64 `json:"peak"`
	Severity  string  `json:"severity"`
	Baseline  float64 `json:"baseline"`
}

type pairDTO struct {
	LeftPulseIndex  int `json:"left_pulse_index"`
	RightPulseIndex int `json:"right_pulse_index"`
	TimeDifference  int `json:"time_difference_samples"`
}

type correlateResponseDTO struct {
	SampleRate       float64             `json:"sample_rate"`
	ToleranceSamples int                 `json:"tolerance_samples"`
	Left             correlateChannelDTO `json:"left"`
	Right            correlateChannelDTO `json:"right"`
	Pairs            []pairDTO           `json:"pairs"`
	LeftUnpaired     []int               `json:"left_unpaired"`
	RightUnpaired    []int               `json:"right_unpaired"`
}

func mustCorrelate(t *testing.T, w *httptest.ResponseRecorder) (correlateResponseDTO, map[string]any) {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp correlateResponseDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	return resp, raw
}

// dualBursts is the deterministic two-pulse channel: a severe burst at
// [5,8] with peak 25 at index 6 and a general burst at [40,43] with peak
// 13 at index 40, over a baseline-2 background of 64 samples.
func dualBursts() []float64 {
	return fillAPI(64, 2, map[int]float64{
		5: 13, 6: 25, 7: 13, 8: 13,
		40: 13, 41: 13, 42: 13, 43: 13,
	})
}

func channelEnvelope(rate float64, samples []float64, ranges ...rawRangeDTO) map[string]any {
	m := map[string]any{"sample_rate": rate, "amplitudes": samples}
	if ranges != nil {
		m["excluded_ranges"] = ranges
	}
	return m
}

// --- four compatibility-locking scenarios -----------------------------------

func TestCorrelate_PrecisePairing(t *testing.T) {
	r := NewRouter()
	body, err := json.Marshal(map[string]any{
		"tolerance_samples": 5,
		"left":              channelEnvelope(16000, dualBursts()),
		"right":             channelEnvelope(16000, dualBursts()),
	})
	require.NoError(t, err)

	resp, raw := mustCorrelate(t, postCorrelate(t, r, body))
	assert.Equal(t, 16000.0, resp.SampleRate)
	assert.Equal(t, 5, resp.ToleranceSamples)

	require.True(t, resp.Left.Decidable)
	require.True(t, resp.Right.Decidable)
	require.Len(t, resp.Left.Pulses, 2)
	require.Len(t, resp.Right.Pulses, 2)
	assert.Equal(t, 6, resp.Left.Pulses[0].PeakIndex)
	assert.Equal(t, 25.0, resp.Left.Pulses[0].Peak)
	assert.Equal(t, "severe", resp.Left.Pulses[0].Severity)
	assert.Equal(t, 40, resp.Left.Pulses[1].PeakIndex)
	assert.Equal(t, "general", resp.Left.Pulses[1].Severity)

	assert.Equal(t, []pairDTO{
		{LeftPulseIndex: 0, RightPulseIndex: 0, TimeDifference: 0},
		{LeftPulseIndex: 1, RightPulseIndex: 1, TimeDifference: 0},
	}, resp.Pairs)
	assert.Empty(t, resp.LeftUnpaired)
	assert.Empty(t, resp.RightUnpaired)

	// Empty lists must serialize as [], never null.
	assert.Equal(t, []any{}, raw["left_unpaired"])
	assert.Equal(t, []any{}, raw["right_unpaired"])
}

func TestCorrelate_MultipleCandidatesPreemption(t *testing.T) {
	r := NewRouter()

	// Left peaks at 10 and 20; the single right pulse peaks at 15. Both
	// pairs differ by exactly the tolerance (5); the candidate ordered by
	// the smaller left index claims the right pulse and the second
	// candidate is skipped.
	left := fillAPI(64, 2, map[int]float64{
		8: 13, 9: 13, 10: 25, 11: 13,
		18: 13, 19: 13, 20: 25, 21: 13,
	})
	right := fillAPI(64, 2, map[int]float64{
		13: 13, 14: 13, 15: 25, 16: 13,
	})

	body, err := json.Marshal(map[string]any{
		"tolerance_samples": 5,
		"left":              channelEnvelope(16000, left),
		"right":             channelEnvelope(16000, right),
	})
	require.NoError(t, err)
	resp, _ := mustCorrelate(t, postCorrelate(t, r, body))

	require.Len(t, resp.Left.Pulses, 2)
	require.Len(t, resp.Right.Pulses, 1)
	assert.Equal(t, []pairDTO{
		{LeftPulseIndex: 0, RightPulseIndex: 0, TimeDifference: 5},
	}, resp.Pairs)
	assert.Equal(t, []int{1}, resp.LeftUnpaired, "the preempted left pulse stays unpaired")
	assert.Empty(t, resp.RightUnpaired)

	// A second right pulse placed at least 4 samples away from the first
	// (so they never merge), peaking at 4 (diff 8 vs the left peak 12) and
	// at 10 (diff 2): the time-difference ordering must beat the
	// pulse-index enumeration order.
	rightTwo := fillAPI(64, 2, map[int]float64{
		// earlier right pulse peaks at 4 (diff 8 vs left peak 12)
		1: 13, 2: 13, 3: 13, 4: 25,
		// later right pulse peaks at 10 (diff 2); 4-sample gap keeps them
		// from merging
		9: 13, 10: 25, 11: 13, 12: 13,
	})
	leftOne := fillAPI(64, 2, map[int]float64{
		10: 13, 11: 13, 12: 25, 13: 13,
	})
	body, err = json.Marshal(map[string]any{
		"tolerance_samples": 8,
		"left":              channelEnvelope(16000, leftOne),
		"right":             channelEnvelope(16000, rightTwo),
	})
	require.NoError(t, err)
	resp, _ = mustCorrelate(t, postCorrelate(t, r, body))
	assert.Equal(t, []pairDTO{
		{LeftPulseIndex: 0, RightPulseIndex: 1, TimeDifference: 2},
	}, resp.Pairs, "smallest time difference wins even though R0 was enumerated first")
	assert.Equal(t, []int{0}, resp.RightUnpaired)
}

func TestCorrelate_EvidenceAlwaysOrderedByLeftPeakIndex(t *testing.T) {
	r := NewRouter()

	// Greedy selection reaches the diff-1 pair (L1,R1) before the diff-5
	// pair (L0,R0); the response must still present evidence by ascending
	// left pulse index.
	left := fillAPI(64, 2, map[int]float64{
		8: 13, 9: 13, 10: 25, 11: 13,
		18: 13, 19: 13, 20: 25, 21: 13,
	})
	right := fillAPI(64, 2, map[int]float64{
		13: 13, 14: 13, 15: 25, 16: 13,
		21: 25, 22: 13, 23: 13, 24: 13,
	})
	body, err := json.Marshal(map[string]any{
		"tolerance_samples": 5,
		"left":              channelEnvelope(16000, left),
		"right":             channelEnvelope(16000, right),
	})
	require.NoError(t, err)
	resp, _ := mustCorrelate(t, postCorrelate(t, r, body))

	assert.Equal(t, []pairDTO{
		{LeftPulseIndex: 0, RightPulseIndex: 0, TimeDifference: 5},
		{LeftPulseIndex: 1, RightPulseIndex: 1, TimeDifference: 1},
	}, resp.Pairs)
	assert.Empty(t, resp.LeftUnpaired)
	assert.Empty(t, resp.RightUnpaired)
}

func TestCorrelate_OneSideUndecidable(t *testing.T) {
	r := NewRouter()
	seam := seamFixture()
	shield := []rawRangeDTO{{Start: 10, End: 74}}

	// Left shielded: the burst at [80,83] becomes a general pulse. Right
	// unshielded: undecidable (no_pulses) because the seam dominates the
	// median.
	body, err := json.Marshal(map[string]any{
		"tolerance_samples": 3,
		"left":              channelEnvelope(16000, seam, shield...),
		"right":             channelEnvelope(16000, seam),
	})
	require.NoError(t, err)
	resp, raw := mustCorrelate(t, postCorrelate(t, r, body))

	assert.True(t, resp.Left.Decidable)
	require.Len(t, resp.Left.Pulses, 1)
	assert.Equal(t, 80, resp.Left.Pulses[0].PeakIndex)
	assert.False(t, resp.Right.Decidable)
	assert.Equal(t, "no_pulses", resp.Right.Reason)
	assert.Empty(t, resp.Right.Pulses)

	assert.Empty(t, resp.Pairs, "an undecidable side forbids any evidence")
	assert.Equal(t, []int{0}, resp.LeftUnpaired, "unpaired indices stay in ascending order")
	assert.Empty(t, resp.RightUnpaired)

	// All four lists must be JSON arrays, even when empty.
	assert.Equal(t, []any{}, raw["pairs"])
	assert.Equal(t, []any{}, raw["right_unpaired"])
	assert.Equal(t, []any{float64(0)}, raw["left_unpaired"])
	assert.Equal(t, []any{}, raw["right"].(map[string]any)["pulses"])
}

func TestCorrelate_OriginalAnalyzeResponseUnchanged(t *testing.T) {
	r := NewRouter()
	samples := seamFixture()
	ranges := []rawRangeDTO{{Start: 10, End: 74}}

	// Standalone single-channel response.
	standaloneBody, err := json.Marshal(map[string]any{
		"sample_rate":     16000.0,
		"amplitudes":      samples,
		"excluded_ranges": ranges,
	})
	require.NoError(t, err)
	standalone := post(t, r, standaloneBody)
	require.Equal(t, http.StatusOK, standalone.Code, standalone.Body.String())
	var standaloneJSON map[string]any
	require.NoError(t, json.Unmarshal(standalone.Body.Bytes(), &standaloneJSON))

	// Same channel embedded as the correlation's right side.
	corrBody, err := json.Marshal(map[string]any{
		"tolerance_samples": 3,
		"left":              channelEnvelope(16000, samples, ranges...),
		"right":             channelEnvelope(16000, samples, ranges...),
	})
	require.NoError(t, err)
	w := postCorrelate(t, r, corrBody)
	var corrJSON map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &corrJSON))

	// Field-for-field identical: decidable, baseline, reason, pulses and
	// the verbatim excluded_ranges echo.
	assert.Equal(t, standaloneJSON, corrJSON["right"])
	assert.Equal(t, standaloneJSON, corrJSON["left"])

	// Legacy endpoints still answer byte-identically when called twice.
	assert.Equal(t, standalone.Body.String(), post(t, r, standaloneBody).Body.String())
}

func TestCorrelate_DeterministicBody(t *testing.T) {
	r := NewRouter()
	body, err := json.Marshal(map[string]any{
		"tolerance_samples": 5,
		"left":              channelEnvelope(16000, dualBursts()),
		"right":             channelEnvelope(16000, dualBursts()),
	})
	require.NoError(t, err)
	first := postCorrelate(t, r, body).Body.String()
	for i := 0; i < 3; i++ {
		assert.Equal(t, first, postCorrelate(t, r, body).Body.String())
	}
}

// --- validation contract -----------------------------------------------------

func TestValidation_CorrelateTolerance(t *testing.T) {
	r := NewRouter()
	channel := fmt.Sprintf(`{"sample_rate":16000,"amplitudes":[%s]}`, strings.Repeat("0,", 63)+"0")

	for _, tc := range []struct {
		name       string
		token      string
		status     int
		constraint string
	}{
		{"missing", "", http.StatusBadRequest, "required"},
		{"null", "null", http.StatusBadRequest, "type"},
		{"string", `"3"`, http.StatusBadRequest, "type"},
		{"boolean", "true", http.StatusBadRequest, "type"},
		{"fractional", "1.5", http.StatusBadRequest, "type"},
		{"negative", "-1", http.StatusBadRequest, "range"},
		{"above max", "101", http.StatusBadRequest, "range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw string
			if tc.token == "" {
				raw = fmt.Sprintf(`{"left":%s,"right":%s}`, channel, channel)
			} else {
				raw = fmt.Sprintf(`{"tolerance_samples":%s,"left":%s,"right":%s}`,
					tc.token, channel, channel)
			}
			w := postCorrelateRaw(t, r, raw)
			assert.Equal(t, tc.status, w.Code, w.Body.String())
			var fe FieldError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
			assert.Equal(t, "validation_failed", fe.Error)
			assert.Equal(t, "tolerance_samples", fe.Field)
			assert.Nil(t, fe.Index)
			assert.Equal(t, tc.constraint, fe.Constraint)
		})
	}

	// NaN/Infinity are located before the JSON decoder rejects the body.
	for _, token := range []string{"NaN", "Infinity", "-Infinity"} {
		t.Run(token, func(t *testing.T) {
			raw := fmt.Sprintf(`{"tolerance_samples":%s,"left":%s,"right":%s}`,
				token, channel, channel)
			w := postCorrelateRaw(t, r, raw)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			var fe FieldError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
			assert.Equal(t, "tolerance_samples", fe.Field)
			assert.Equal(t, "type", fe.Constraint)
		})
	}

	// The boundaries zero and one hundred are both accepted.
	for _, tol := range []int{0, 100} {
		body, err := json.Marshal(map[string]any{
			"tolerance_samples": tol,
			"left":              channelEnvelope(16000, dualBursts()),
			"right":             channelEnvelope(16000, dualBursts()),
		})
		require.NoError(t, err)
		w := postCorrelate(t, r, body)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
}

func TestValidation_CorrelateSides(t *testing.T) {
	r := NewRouter()
	goodSide := func(rate float64) string {
		return fmt.Sprintf(`{"sample_rate":%v,"amplitudes":[%s]}`, rate, strings.Repeat("0,", 63)+"0")
	}

	for _, tc := range []struct {
		name       string
		body       string
		field      string
		index      *int
		constraint string
	}{
		{
			name:       "left missing",
			body:       fmt.Sprintf(`{"tolerance_samples":1,"right":%s}`, goodSide(16000)),
			field:      "left",
			constraint: "required",
		},
		{
			name:       "left null",
			body:       fmt.Sprintf(`{"tolerance_samples":1,"left":null,"right":%s}`, goodSide(16000)),
			field:      "left",
			constraint: "type",
		},
		{
			name:       "left scalar",
			body:       fmt.Sprintf(`{"tolerance_samples":1,"left":123,"right":%s}`, goodSide(16000)),
			field:      "left",
			constraint: "type",
		},
		{
			name:       "right missing",
			body:       fmt.Sprintf(`{"tolerance_samples":1,"left":%s}`, goodSide(16000)),
			field:      "right",
			constraint: "required",
		},
		{
			name:       "right null",
			body:       fmt.Sprintf(`{"tolerance_samples":1,"left":%s,"right":null}`, goodSide(16000)),
			field:      "right",
			constraint: "type",
		},
		{
			name: "left empty object",
			body: fmt.Sprintf(`{"tolerance_samples":1,"left":{},"right":%s}`,
				goodSide(16000)),
			field:      "left.sample_rate",
			constraint: "required",
		},
		{
			name: "left sample rate too low",
			body: fmt.Sprintf(`{"tolerance_samples":1,"left":%s,"right":%s}`,
				goodSide(999), goodSide(16000)),
			field:      "left.sample_rate",
			constraint: "range",
		},
		{
			name: "right sample rate wrong type",
			body: fmt.Sprintf(`{"tolerance_samples":1,"left":%s,`+
				`"right":{"sample_rate":"16000","amplitudes":[%s]}}`,
				goodSide(16000), strings.Repeat("0,", 63)+"0"),
			field:      "right.sample_rate",
			constraint: "type",
		},
		{
			name: "left amplitudes too short",
			body: fmt.Sprintf(`{"tolerance_samples":1,`+
				`"left":{"sample_rate":16000,"amplitudes":[%s]},"right":%s}`,
				strings.Repeat("0,", 62)+"0", goodSide(16000)),
			field:      "left.amplitudes",
			constraint: "min_length",
		},
		{
			name: "right non-array amplitudes",
			body: fmt.Sprintf(`{"tolerance_samples":1,"left":%s,`+
				`"right":{"sample_rate":16000,"amplitudes":2}}`, goodSide(16000)),
			field:      "right.amplitudes",
			constraint: "type",
		},
		{
			name: "right bad amplitude element keeps sample index",
			body: func() string {
				values := make([]string, 64)
				for i := range values {
					values[i] = "0"
				}
				values[7] = `"oops"`
				return fmt.Sprintf(`{"tolerance_samples":1,"left":%s,`+
					`"right":{"sample_rate":16000,"amplitudes":[%s]}}`,
					goodSide(16000), strings.Join(values, ","))
			}(),
			field:      "right.amplitudes",
			index:      intPtrVal(7),
			constraint: "type",
		},
		{
			name: "left inverted excluded range keeps element index",
			body: fmt.Sprintf(`{"tolerance_samples":1,`+
				`"left":{"sample_rate":16000,"amplitudes":[%s],"excluded_ranges":[{"start":10,"end":9}]},`+
				`"right":%s}`, strings.Repeat("0,", 63)+"0", goodSide(16000)),
			field:      "left.excluded_ranges",
			index:      intPtrVal(0),
			constraint: "order",
		},
		{
			name: "right overlap keeps element index",
			body: fmt.Sprintf(`{"tolerance_samples":1,"left":%s,`+
				`"right":{"sample_rate":16000,"amplitudes":[%s],`+
				`"excluded_ranges":[{"start":10,"end":20},{"start":20,"end":21}]}}`,
				goodSide(16000), strings.Repeat("0,", 63)+"0"),
			field:      "right.excluded_ranges",
			index:      intPtrVal(1),
			constraint: "overlap",
		},
		{
			name: "unknown field inside left",
			body: fmt.Sprintf(`{"tolerance_samples":1,`+
				`"left":{"sample_rate":16000,"amplitudes":[%s],"bogus":1},"right":%s}`,
				strings.Repeat("0,", 63)+"0", goodSide(16000)),
			field:      "left.bogus",
			constraint: "unknown",
		},
		{
			name:       "sample rates differ",
			body:       fmt.Sprintf(`{"tolerance_samples":1,"left":%s,"right":%s}`, goodSide(16000), goodSide(8000)),
			field:      "right.sample_rate",
			constraint: "sample_rate_mismatch",
		},
		{
			name:       "array side is a side type error",
			body:       fmt.Sprintf(`{"tolerance_samples":1,"left":[],"right":%s}`, goodSide(16000)),
			field:      "left",
			constraint: "type",
		},
		{
			name:       "array side containing NaN is a side type error",
			body:       fmt.Sprintf(`{"tolerance_samples":1,"left":[NaN],"right":%s}`, goodSide(16000)),
			field:      "left",
			constraint: "type",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postCorrelateRaw(t, r, tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var fe FieldError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
			assert.Equal(t, "validation_failed", fe.Error)
			assert.Equal(t, tc.field, fe.Field)
			assert.Equal(t, tc.constraint, fe.Constraint)
			if tc.index == nil {
				assert.Nil(t, fe.Index)
			} else {
				require.NotNil(t, fe.Index)
				assert.Equal(t, *tc.index, *fe.Index)
			}
			assert.NotEmpty(t, fe.Message)
		})
	}
}

func TestValidation_CorrelateNonFiniteLocatedBySide(t *testing.T) {
	r := NewRouter()
	zeros := strings.Repeat("0,", 63) + "0"

	for _, tc := range []struct {
		name       string
		body       string
		field      string
		index      *int
		constraint string
	}{
		{
			name: "NaN in left amplitudes at index 3",
			body: func() string {
				values := make([]string, 64)
				for i := range values {
					values[i] = "0"
				}
				values[3] = "NaN"
				return fmt.Sprintf(`{"tolerance_samples":1,`+
					`"left":{"sample_rate":16000,"amplitudes":[%s]},`+
					`"right":{"sample_rate":16000,"amplitudes":[%s]}}`,
					strings.Join(values, ","), zeros)
			}(),
			field:      "left.amplitudes",
			index:      intPtrVal(3),
			constraint: "finite",
		},
		{
			name: "-Infinity in right sample_rate",
			body: fmt.Sprintf(`{"tolerance_samples":1,`+
				`"left":{"sample_rate":16000,"amplitudes":[%s]},`+
				`"right":{"sample_rate":-Infinity,"amplitudes":[%s]}}`,
				zeros, zeros),
			field:      "right.sample_rate",
			constraint: "finite",
		},
		{
			name: "NaN in left excluded range start keeps element index",
			body: fmt.Sprintf(`{"tolerance_samples":1,`+
				`"left":{"sample_rate":16000,"amplitudes":[%s],"excluded_ranges":[{"start":NaN,"end":3}]},`+
				`"right":{"sample_rate":16000,"amplitudes":[%s]}}`,
				zeros, zeros),
			field:      "left.excluded_ranges",
			index:      intPtrVal(0),
			constraint: "type",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postCorrelateRaw(t, r, tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var fe FieldError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
			assert.Equal(t, tc.field, fe.Field)
			assert.Equal(t, tc.constraint, fe.Constraint)
			if tc.index != nil {
				require.NotNil(t, fe.Index)
				assert.Equal(t, *tc.index, *fe.Index)
			}
		})
	}
}

func TestValidation_CorrelateRejectsPartialAndUnknownTopFields(t *testing.T) {
	r := NewRouter()

	// An invalid side must surface the error envelope only: no analysis
	// fields may leak into a 400 body.
	zeros := strings.Repeat("0,", 63) + "0"
	raw := fmt.Sprintf(`{"tolerance_samples":-5,`+
		`"left":{"sample_rate":16000,"amplitudes":[%s]},`+
		`"right":{"sample_rate":16000,"amplitudes":[%s]}}`, zeros, zeros)
	w := postCorrelateRaw(t, r, raw)
	require.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.NotContains(t, body, "pairs")
	assert.NotContains(t, body, "left")
	assert.NotContains(t, body, "right")

	// Unknown top-level fields are rejected exactly as on the legacy route.
	w = postCorrelateRaw(t, r, fmt.Sprintf(
		`{"tolerance_samples":1,"left":{"sample_rate":16000,"amplitudes":[%s]},`+
			`"right":{"sample_rate":16000,"amplitudes":[%s]},"extra":1}`,
		zeros, zeros))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var fe FieldError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "extra", fe.Field)
	assert.Equal(t, "unknown", fe.Constraint)
}
