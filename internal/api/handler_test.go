package api

import (
	"bytes"
	"encoding/json"
	"fmt"
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
