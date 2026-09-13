package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func flatBody(t *testing.T, rate float64, n int, set map[int]float64) []byte {
	t.Helper()
	s := make([]float64, n)
	for i := range s {
		s[i] = 2
		if v, ok := set[i]; ok {
			s[i] = v
		}
	}
	body, err := json.Marshal(AnalyzeRequest{SampleRate: rate, Amplitudes: s})
	require.NoError(t, err)
	return body
}

// seamFixture builds the end-to-end sample: 130 samples with a high-amplitude
// wheel-seam block at [10,74] (value 30), a 4-sample bearing burst at
// [80,83] (value 13) and 61 ordinary samples at 2. With the seam kept the
// median is 21.5 and nothing crosses the threshold; with the seam excluded the
// baseline drops to 2 and the burst becomes a general pulse.
func seamFixture() []float64 {
	s := make([]float64, 130)
	for i := range s {
		s[i] = 2
	}
	for i := 10; i <= 74; i++ {
		s[i] = 30
	}
	for i := 80; i <= 83; i++ {
		s[i] = 13
	}
	return s
}

type rawRangeDTO struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

func post(t *testing.T, r http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pulses/analyze", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func postRaw(t *testing.T, r http.Handler, raw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pulses/analyze", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAnalyze_Success(t *testing.T) {
	r := NewRouter()
	w := post(t, r, flatBody(t, 16000, 64, map[int]float64{
		5: 13, 6: 25, 7: 13, 8: 13,
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp AnalyzeResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Decidable)
	assert.Equal(t, 2.0, resp.Baseline)
	require.Len(t, resp.Pulses, 1)
	p := resp.Pulses[0]
	assert.Equal(t, 5, p.Start)
	assert.Equal(t, 8, p.End)
	assert.Equal(t, 6, p.PeakIndex)
	assert.Equal(t, 25.0, p.Peak)
	assert.Equal(t, "severe", string(p.Severity))
	assert.Equal(t, 2.0, p.Baseline)
	assert.Equal(t, 16000.0, resp.SampleRate)
}

func TestAnalyze_UndecidableZeroBaseline(t *testing.T) {
	r := NewRouter()
	zeros, err := json.Marshal(AnalyzeRequest{SampleRate: 16000, Amplitudes: make([]float64, 64)})
	require.NoError(t, err)
	w := post(t, r, zeros)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["decidable"])
	assert.Equal(t, "baseline_zero", resp["reason"])
	assert.Equal(t, []any{}, resp["pulses"], "must never fabricate pulses")
}

func TestAnalyze_DeterministicBody(t *testing.T) {
	r := NewRouter()
	body := flatBody(t, 16000, 64, map[int]float64{
		5: 13, 6: 25, 7: 13, 8: 13,
		40: 13, 41: 13, 42: 13, 43: 13,
	})
	first := post(t, r, body).Body.String()
	for i := 0; i < 3; i++ {
		assert.Equal(t, first, post(t, r, body).Body.String())
	}
}

func TestValidation_SampleRateRange(t *testing.T) {
	r := NewRouter()
	zeros := func(rate float64) []byte {
		b, _ := json.Marshal(AnalyzeRequest{SampleRate: rate, Amplitudes: make([]float64, 64)})
		return b
	}

	for _, rate := range []float64{999, 48001, 0, -1} {
		w := post(t, r, zeros(rate))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		var fe FieldError
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
		assert.Equal(t, "sample_rate", fe.Field)
		assert.Equal(t, "range", fe.Constraint)
	}

	// Boundary values are accepted.
	for _, rate := range []float64{1000, 48000} {
		w := post(t, r, zeros(rate))
		assert.Equal(t, http.StatusOK, w.Code, "rate %v", rate)
	}
}

func TestValidation_SampleRateTypeAndRequired(t *testing.T) {
	r := NewRouter()

	w := postRaw(t, r, fmt.Sprintf(`{"sample_rate":"x","amplitudes":[%s]}`, strings.Repeat("0,", 63)+"0"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var fe FieldError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "sample_rate", fe.Field)
	assert.Equal(t, "type", fe.Constraint)

	w = postRaw(t, r, fmt.Sprintf(`{"amplitudes":[%s]}`, strings.Repeat("0,", 63)+"0"))
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "sample_rate", fe.Field)
	assert.Equal(t, "required", fe.Constraint)
}

func TestValidation_Length(t *testing.T) {
	r := NewRouter()

	for _, tc := range []struct {
		n          int
		constraint string
	}{
		{63, "min_length"},
		{20001, "max_length"},
	} {
		b, _ := json.Marshal(AnalyzeRequest{SampleRate: 16000, Amplitudes: make([]float64, tc.n)})
		w := post(t, r, b)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		var fe FieldError
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
		assert.Equal(t, "amplitudes", fe.Field)
		assert.Equal(t, tc.constraint, fe.Constraint)
		assert.Nil(t, fe.Index)
	}
}

func TestValidation_BadElementLocatesIndex(t *testing.T) {
	r := NewRouter()

	// A string at index 7.
	values := make([]string, 64)
	for i := range values {
		values[i] = "0"
	}
	values[7] = `"oops"`
	w := postRaw(t, r, fmt.Sprintf(`{"sample_rate":16000,"amplitudes":[%s]}`,
		strings.Join(values, ",")))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var fe FieldError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "amplitudes", fe.Field)
	require.NotNil(t, fe.Index)
	assert.Equal(t, 7, *fe.Index)
	assert.Equal(t, "type", fe.Constraint)
}

func TestValidation_NullElementLocatesIndex(t *testing.T) {
	r := NewRouter()
	values := make([]string, 64)
	for i := range values {
		values[i] = "0"
	}
	values[0] = "null"
	w := postRaw(t, r, fmt.Sprintf(`{"sample_rate":16000,"amplitudes":[%s]}`,
		strings.Join(values, ",")))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var fe FieldError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	require.NotNil(t, fe.Index)
	assert.Equal(t, 0, *fe.Index)
}

func TestValidation_NonFiniteTokensLocated(t *testing.T) {
	r := NewRouter()

	for _, tc := range []struct {
		name  string
		token string
		index int
	}{
		{"NaN", "NaN", 3},
		{"Infinity", "Infinity", 10},
		{"negative Infinity", "-Infinity", 63},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := make([]string, 64)
			for i := range values {
				values[i] = "0"
			}
			values[tc.index] = tc.token
			w := postRaw(t, r, fmt.Sprintf(`{"sample_rate":16000,"amplitudes":[%s]}`,
				strings.Join(values, ",")))
			assert.Equal(t, http.StatusBadRequest, w.Code)
			var fe FieldError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
			assert.Equal(t, "amplitudes", fe.Field)
			require.NotNil(t, fe.Index)
			assert.Equal(t, tc.index, *fe.Index)
			assert.Equal(t, "finite", fe.Constraint)
		})
	}

	// A NaN-looking word inside a JSON string must not be flagged; the
	// standard decoder then reports the usual type error.
	w := postRaw(t, r, fmt.Sprintf(`{"sample_rate":"NaN","amplitudes":[%s]}`,
		strings.Repeat("0,", 63)+"0"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var fe FieldError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "sample_rate", fe.Field)
	assert.Equal(t, "type", fe.Constraint)

	// NaN for sample_rate is located at the field.
	w = postRaw(t, r, fmt.Sprintf(`{"sample_rate":NaN,"amplitudes":[%s]}`,
		strings.Repeat("0,", 63)+"0"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "sample_rate", fe.Field)
	assert.Equal(t, "finite", fe.Constraint)
}

func TestValidation_AmplitudesNotArray(t *testing.T) {
	r := NewRouter()
	w := postRaw(t, r, `{"sample_rate":16000,"amplitudes":1}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var fe FieldError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "amplitudes", fe.Field)
	assert.Equal(t, "type", fe.Constraint)
}

func TestValidation_UnknownFieldNamed(t *testing.T) {
	r := NewRouter()
	w := postRaw(t, r, `{"sample_rate":16000,"amplitudes":[`+strings.Repeat("0,", 63)+`0],"bogus":1}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var fe FieldError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "bogus", fe.Field)
}

func TestValidation_FieldNamesAreCaseSensitive(t *testing.T) {
	r := NewRouter()
	zeros := strings.Repeat("0,", 63) + "0"

	// Contract names are case-sensitive: encoding/json would otherwise fold
	// a variant like "Sample_rate" onto the real field and the request would
	// be silently analyzed instead of rejected.
	for _, tc := range []struct {
		name  string
		raw   string
		field string
	}{
		{"capitalized sample_rate",
			`{"Sample_rate":16000,"amplitudes":[` + zeros + `]}`, "Sample_rate"},
		{"capitalized amplitudes",
			`{"sample_rate":16000,"Amplitudes":[` + zeros + `]}`, "Amplitudes"},
		{"uppercase amplitudes",
			`{"sample_rate":16000,"AMPLITUDES":[` + zeros + `]}`, "AMPLITUDES"},
		{"capitalized excluded_ranges",
			`{"sample_rate":16000,"amplitudes":[` + zeros + `],"Excluded_Ranges":[]}`, "Excluded_Ranges"},
		{"capitalized include_metrics",
			`{"sample_rate":16000,"amplitudes":[` + zeros + `],"Include_Metrics":true}`, "Include_Metrics"},
		{"uppercase include_metrics",
			`{"sample_rate":16000,"amplitudes":[` + zeros + `],"INCLUDE_METRICS":true}`, "INCLUDE_METRICS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postRaw(t, r, tc.raw)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var fe FieldError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
			assert.Equal(t, "validation_failed", fe.Error)
			assert.Equal(t, tc.field, fe.Field)
			assert.Equal(t, "unknown", fe.Constraint)
			assert.Nil(t, fe.Index)
			assert.Contains(t, fe.Message, `"`+tc.field+`"`)
			// The request is rejected outright: no analysis leaks through.
			assert.NotContains(t, w.Body.String(), "pulses")
			assert.NotContains(t, w.Body.String(), "duration_ms")
		})
	}
}

func TestValidation_MalformedJSON(t *testing.T) {
	r := NewRouter()
	w := postRaw(t, r, `{"sample_rate":`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var fe FieldError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "invalid_json", fe.Error)
}

func TestHealthz(t *testing.T) {
	r := NewRouter()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- excluded_ranges contract ------------------------------------------------

func TestAnalyze_ExcludedRangesEchoAndResult(t *testing.T) {
	r := NewRouter()
	body, err := json.Marshal(map[string]any{
		"sample_rate":     16000.0,
		"amplitudes":      seamFixture(),
		"excluded_ranges": []map[string]int{{"start": 10, "end": 74}},
	})
	require.NoError(t, err)
	w := post(t, r, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		AnalyzeResponse
		ExcludedRanges []rawRangeDTO `json:"excluded_ranges"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Decidable)
	assert.Equal(t, 2.0, resp.Baseline)
	require.Len(t, resp.Pulses, 1)
	p := resp.Pulses[0]
	assert.Equal(t, 80, p.Start)
	assert.Equal(t, 83, p.End)
	assert.Equal(t, 13.0, p.Peak)
	assert.Equal(t, "general", string(p.Severity))

	// The adopted ranges are echoed verbatim for point-by-point review.
	require.Len(t, resp.ExcludedRanges, 1)
	assert.Equal(t, rawRangeDTO{Start: 10, End: 74}, resp.ExcludedRanges[0])
}

func TestAnalyze_ExcludedRangesMultipleEchoedInOrder(t *testing.T) {
	r := NewRouter()
	// A 200-sample quiet signal; exclude two disjoint blocks; the response
	// must echo exactly what was adopted, in request order.
	samples := make([]float64, 200)
	for i := range samples {
		samples[i] = 2
	}
	for i := 100; i <= 105; i++ {
		samples[i] = 30
	}
	body, err := json.Marshal(map[string]any{
		"sample_rate": 16000.0,
		"amplitudes":  samples,
		"excluded_ranges": []map[string]int{
			{"start": 0, "end": 9},
			{"start": 100, "end": 105},
		},
	})
	require.NoError(t, err)
	w := post(t, r, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		ExcludedRanges []rawRangeDTO `json:"excluded_ranges"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.ExcludedRanges, 2)
	assert.Equal(t, rawRangeDTO{0, 9}, resp.ExcludedRanges[0])
	assert.Equal(t, rawRangeDTO{100, 105}, resp.ExcludedRanges[1])
}

func TestAnalyze_ExcludedRangesBreakCandidatesAcrossGap(t *testing.T) {
	r := NewRouter()
	// Two 2-sample bursts separated by two samples ([12,13]): gap <= 3 would
	// normally merge them into a retained 6-sample interval. Excluding the
	// gap samples forces separate 2-sample candidates, both filtered.
	// 68 samples keep 66 valid samples after the 2-sample exclusion.
	s := make([]float64, 68)
	for i := range s {
		s[i] = 2
	}
	for _, i := range []int{10, 11, 14, 15} {
		s[i] = 20
	}
	body, err := json.Marshal(map[string]any{
		"sample_rate":     16000.0,
		"amplitudes":      s,
		"excluded_ranges": []map[string]int{{"start": 12, "end": 13}},
	})
	require.NoError(t, err)

	// Without the exclusion the bursts merge and survive.
	plainBody, _ := json.Marshal(map[string]any{"sample_rate": 16000.0, "amplitudes": s})
	plain := post(t, r, plainBody)
	require.Equal(t, http.StatusOK, plain.Code)
	var plainResp AnalyzeResponse
	require.NoError(t, json.Unmarshal(plain.Body.Bytes(), &plainResp))
	require.Len(t, plainResp.Pulses, 1)

	// With the exclusion both pieces are filtered.
	w := post(t, r, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp AnalyzeResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.Decidable)
	assert.Equal(t, "no_pulses", resp.Reason)
	assert.Empty(t, resp.Pulses)
}

func TestAnalyze_ExcludedRangesOmittedOrEmptyKeepsContract(t *testing.T) {
	r := NewRouter()
	set := map[int]float64{5: 13, 6: 25, 7: 13, 8: 13}

	omitted, err := json.Marshal(map[string]any{
		"sample_rate": 16000.0, "amplitudes": fillAPI(64, 2, set),
	})
	require.NoError(t, err)
	empty, err := json.Marshal(map[string]any{
		"sample_rate": 16000.0, "amplitudes": fillAPI(64, 2, set),
		"excluded_ranges": []any{},
	})
	require.NoError(t, err)

	omittedBody := post(t, r, omitted).Body.String()
	emptyBody := post(t, r, empty).Body.String()

	// An explicit null means "not provided" for this optional field.
	nullBodyReq, _ := json.Marshal(map[string]any{
		"sample_rate": 16000.0, "amplitudes": fillAPI(64, 2, set),
		"excluded_ranges": nil,
	})
	assert.Equal(t, omittedBody, post(t, r, nullBodyReq).Body.String())

	// Field-for-field identical responses, and the field itself is absent
	// (omitempty) in both so existing clients see the legacy contract.
	assert.Equal(t, omittedBody, emptyBody)
	assert.NotContains(t, omittedBody, "excluded_ranges")
	assert.NotContains(t, emptyBody, "excluded_ranges")

	var resp AnalyzeResponse
	require.NoError(t, json.Unmarshal([]byte(omittedBody), &resp))
	assert.True(t, resp.Decidable)
	assert.Equal(t, 2.0, resp.Baseline)
	require.Len(t, resp.Pulses, 1)
	assert.Equal(t, 25.0, resp.Pulses[0].Peak)
	assert.Nil(t, resp.ExcludedRanges)
}

func fillAPI(n int, base float64, set map[int]float64) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = base
		if v, ok := set[i]; ok {
			s[i] = v
		}
	}
	return s
}

func TestAnalyze_ExcludedRangesBoundaryInputs(t *testing.T) {
	r := NewRouter()
	// Legal boundaries locked by the processor: point ranges at both edges
	// and adjacent ranges leaving exactly 64 kept samples out of 67.
	samples := make([]float64, 67)
	for i := range samples {
		samples[i] = 2
	}
	for _, i := range []int{2, 3, 4, 5} {
		samples[i] = 20
	}
	body, err := json.Marshal(map[string]any{
		"sample_rate": 16000.0,
		"amplitudes":  samples,
		"excluded_ranges": []map[string]int{
			{"start": 0, "end": 0},
			{"start": 1, "end": 1},
			{"start": 66, "end": 66},
		},
	})
	require.NoError(t, err)
	w := post(t, r, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp AnalyzeResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Decidable)
	require.Len(t, resp.Pulses, 1)
	assert.Equal(t, 2, resp.Pulses[0].Start)
	assert.Equal(t, 5, resp.Pulses[0].End)
}

func TestValidation_ExcludedRanges(t *testing.T) {
	r := NewRouter()

	postRanges := func(t *testing.T, rangesJSON string, n int) *httptest.ResponseRecorder {
		t.Helper()
		raw := fmt.Sprintf(`{"sample_rate":16000,"amplitudes":[%s],"excluded_ranges":%s}`,
			strings.Repeat("0,", n-1)+"0", rangesJSON)
		return postRaw(t, r, raw)
	}

	for _, tc := range []struct {
		name       string
		n          int
		ranges     string
		index      *int
		constraint string
		field      string
	}{
		{name: "inverted", n: 64, ranges: `[{"start":10,"end":9}]`,
			index: intPtrVal(0), constraint: "order"},
		{name: "end past last sample", n: 64, ranges: `[{"start":0,"end":64}]`,
			index: intPtrVal(0), constraint: "range"},
		{name: "negative start", n: 64, ranges: `[{"start":-1,"end":3}]`,
			index: intPtrVal(0), constraint: "range"},
		{name: "starts not ascending", n: 64, ranges: `[{"start":10,"end":11},{"start":9,"end":12}]`,
			index: intPtrVal(1), constraint: "order"},
		{name: "overlap", n: 64, ranges: `[{"start":10,"end":20},{"start":20,"end":21}]`,
			index: intPtrVal(1), constraint: "overlap"},
		{name: "overlap at second offender", n: 64,
			ranges: `[{"start":0,"end":0},{"start":5,"end":5},{"start":5,"end":6}]`,
			index:  intPtrVal(2), constraint: "overlap"},
		{name: "fewer than 64 remain", n: 64, ranges: `[{"start":63,"end":63}]`,
			index: intPtrVal(0), constraint: "min_length"},
		{name: "first range already drops below 64 kept", n: 64,
			ranges: `[{"start":0,"end":0},{"start":32,"end":63}]`,
			index:  intPtrVal(0), constraint: "min_length"},
		{name: "later range tips below 64 kept", n: 65,
			ranges: `[{"start":0,"end":0},{"start":32,"end":63}]`,
			index:  intPtrVal(1), constraint: "min_length"},
		{name: "element not object", n: 64, ranges: `[5]`,
			index: intPtrVal(0), constraint: "type"},
		{name: "bare NaN element", n: 64, ranges: `[1,{"start":0,"end":0},NaN]`,
			index: intPtrVal(2), constraint: "type"},
		{name: "null element", n: 64, ranges: `[null]`,
			index: intPtrVal(0), constraint: "type"},
		{name: "missing start", n: 64, ranges: `[{"end":3}]`,
			index: intPtrVal(0), constraint: "required"},
		{name: "missing end", n: 64, ranges: `[{"start":3}]`,
			index: intPtrVal(0), constraint: "required"},
		{name: "null start is a type error", n: 64, ranges: `[{"start":null,"end":3}]`,
			index: intPtrVal(0), constraint: "type"},
		{name: "null end is a type error", n: 64, ranges: `[{"start":1,"end":null}]`,
			index: intPtrVal(0), constraint: "type"},
		{name: "non integer start", n: 64, ranges: `[{"start":1.5,"end":3}]`,
			index: intPtrVal(0), constraint: "type"},
		{name: "string start", n: 64, ranges: `[{"start":"3","end":4}]`,
			index: intPtrVal(0), constraint: "type"},
		{name: "unknown element field", n: 64, ranges: `[{"start":1,"end":2,"x":1}]`,
			index: intPtrVal(0), constraint: "unknown"},
		{name: "uppercase endpoint names are unknown", n: 64,
			ranges: `[{"Start":1,"End":2}]`,
			index:  intPtrVal(0), constraint: "unknown"},
		{name: "mixed-case endpoint is unknown", n: 64,
			ranges: `[{"start":1,"End":2}]`,
			index:  intPtrVal(0), constraint: "unknown"},
		{name: "not an array", n: 64, ranges: `{"start":1,"end":2}`,
			index: nil, constraint: "type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postRanges(t, tc.ranges, tc.n)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var fe FieldError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
			assert.Equal(t, "validation_failed", fe.Error)
			assert.Equal(t, "excluded_ranges", fe.Field)
			assert.Equal(t, tc.constraint, fe.Constraint)
			if tc.index == nil {
				assert.Nil(t, fe.Index, "field-level error must carry no index: %s", fe.Message)
			} else {
				require.NotNil(t, fe.Index)
				assert.Equal(t, *tc.index, *fe.Index)
			}
			assert.NotEmpty(t, fe.Message)
		})
	}
}

func TestValidation_ExcludedRangesNaNLocated(t *testing.T) {
	r := NewRouter()
	raw := fmt.Sprintf(`{"sample_rate":16000,"amplitudes":[%s],"excluded_ranges":[{"start":NaN,"end":3}]}`,
		strings.Repeat("0,", 63)+"0")
	w := postRaw(t, r, raw)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var fe FieldError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "excluded_ranges", fe.Field)
	require.NotNil(t, fe.Index)
	assert.Equal(t, 0, *fe.Index)
	assert.Equal(t, "type", fe.Constraint)
}

func intPtrVal(i int) *int { return &i }

// --- include_metrics contract -------------------------------------------------

func TestAnalyze_IncludeMetricsReturnsMetrics(t *testing.T) {
	r := NewRouter()
	// 4-sample pulse [5,8] (13, 25, 13, 13) at 16 kHz: duration 0.25 ms,
	// RMS sqrt((13² + 25² + 13² + 13²)/4) = sqrt(283).
	body, err := json.Marshal(map[string]any{
		"sample_rate":     16000.0,
		"amplitudes":      fillAPI(64, 2, map[int]float64{5: 13, 6: 25, 7: 13, 8: 13}),
		"include_metrics": true,
	})
	require.NoError(t, err)
	w := post(t, r, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp AnalyzeResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Decidable)
	require.Len(t, resp.Pulses, 1)
	p := resp.Pulses[0]
	assert.Equal(t, 5, p.Start)
	assert.Equal(t, 8, p.End)
	require.NotNil(t, p.DurationMS, "metrics must be present when requested")
	require.NotNil(t, p.RMSAmplitude)
	assert.Equal(t, 0.25, *p.DurationMS)
	assert.InDelta(t, math.Sqrt(283), *p.RMSAmplitude, 1e-12)

	// The raw body carries both metric keys inside the pulse object.
	assert.Contains(t, w.Body.String(), "duration_ms")
	assert.Contains(t, w.Body.String(), "rms_amplitude")
}

func TestAnalyze_IncludeMetricsUndecidableStaysEmpty(t *testing.T) {
	r := NewRouter()
	// Zero baseline with the switch on: the undecidable envelope is
	// unchanged and no metrics are fabricated.
	body, err := json.Marshal(map[string]any{
		"sample_rate":     16000.0,
		"amplitudes":      make([]float64, 64),
		"include_metrics": true,
	})
	require.NoError(t, err)
	w := post(t, r, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["decidable"])
	assert.Equal(t, "baseline_zero", resp["reason"])
	assert.Equal(t, []any{}, resp["pulses"])
	assert.NotContains(t, w.Body.String(), "duration_ms")
	assert.NotContains(t, w.Body.String(), "rms_amplitude")
}

func TestAnalyze_IncludeMetricsOffKeepsLegacyBody(t *testing.T) {
	r := NewRouter()
	samples := fillAPI(64, 2, map[int]float64{5: 13, 6: 25, 7: 13, 8: 13})
	mk := func(extra map[string]any) []byte {
		m := map[string]any{"sample_rate": 16000.0, "amplitudes": samples}
		for k, v := range extra {
			m[k] = v
		}
		b, err := json.Marshal(m)
		require.NoError(t, err)
		return b
	}

	omitted := post(t, r, mk(nil)).Body.String()
	for _, extra := range []map[string]any{
		{"include_metrics": false},
	} {
		body := post(t, r, mk(extra)).Body.String()
		assert.Equal(t, omitted, body, "switch off must keep the legacy body: %v", extra)
	}
	assert.NotContains(t, omitted, "duration_ms")
	assert.NotContains(t, omitted, "rms_amplitude")
}

func TestValidation_IncludeMetricsType(t *testing.T) {
	r := NewRouter()
	zeros := strings.Repeat("0,", 63) + "0"

	// Only a JSON boolean is legal; every other token (an explicit null, the
	// non-finite literals, numbers, strings, containers) is a type error
	// located at include_metrics.
	for _, token := range []string{"null", `"true"`, "1", "0", "1.5", "[]", "{}", "NaN", "Infinity"} {
		t.Run(token, func(t *testing.T) {
			w := postRaw(t, r, fmt.Sprintf(
				`{"sample_rate":16000,"amplitudes":[%s],"include_metrics":%s}`, zeros, token))
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var fe FieldError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
			assert.Equal(t, "validation_failed", fe.Error)
			assert.Equal(t, "include_metrics", fe.Field)
			assert.Equal(t, "type", fe.Constraint)
			assert.Nil(t, fe.Index)
			assert.NotEmpty(t, fe.Message)
		})
	}
}
