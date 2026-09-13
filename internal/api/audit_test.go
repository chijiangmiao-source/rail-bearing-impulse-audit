package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const auditRoute = "/api/v1/acquisition-audits"

// auditRawBody posts a raw JSON body to the audit endpoint.
func auditPostRaw(t *testing.T, r http.Handler, raw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, auditRoute, strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// auditPost posts marshalled full_scale/samples and returns the recorder.
func auditPost(t *testing.T, r http.Handler, fullScale any, samples []float64) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"full_scale": fullScale, "samples": samples})
	require.NoError(t, err)
	return auditPostRaw(t, r, string(body))
}

func auditSamples(v float64, n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = v
	}
	return s
}

func TestAuditRoute_HealthyResponse(t *testing.T) {
	r := NewRouter()
	// 256 alternating ±1 samples at full scale 100: no clips, no run past
	// one sample, mean ratio 1%.
	samples := make([]float64, 256)
	for i := range samples {
		if i%2 == 0 {
			samples[i] = 1
		} else {
			samples[i] = -1
		}
	}
	w := auditPost(t, r, 100.0, samples)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "healthy", resp["status"])
	assert.Equal(t, float64(0), resp["clipping_ratio"])
	assert.Equal(t, float64(1), resp["longest_flatline"])
	assert.Equal(t, 0.01, resp["mean_abs_ratio"])
	assert.Equal(t, []any{}, resp["findings"], "healthy findings serialize as []")
}

func TestAuditRoute_RejectedBoundaryResponse(t *testing.T) {
	r := NewRouter()
	// Exactly 50 isolated full-scale samples among 1000: the clipping ratio
	// lands on 0.05 (>=5%), rejected, with no other metric firing (base
	// level 1% of full scale, no run past one sample).
	n := 1000
	samples := make([]float64, n)
	for i := range samples {
		if i%2 == 0 {
			samples[i] = 1
		} else {
			samples[i] = -1
		}
	}
	for i := 0; i < n; i += 20 {
		samples[i] = 100
	}
	w := auditPost(t, r, 100.0, samples)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "rejected", resp["status"])
	assert.Equal(t, 0.05, resp["clipping_ratio"])
	findings := resp["findings"].([]any)
	require.Len(t, findings, 1)
	f0 := findings[0].(map[string]any)
	assert.Equal(t, "clipping", f0["code"])
	assert.Equal(t, "rejected", f0["severity"])
}

func TestAuditRoute_DegradedFlatlineBoundaryResponse(t *testing.T) {
	r := NewRouter()
	// A run of exactly 16 identical quiet samples on an otherwise
	// alternating signal: degraded flatline at the equality boundary.
	samples := make([]float64, 256)
	for i := range samples {
		if i%2 == 0 {
			samples[i] = 1
		} else {
			samples[i] = -1
		}
	}
	for i := 0; i <= 15; i++ {
		samples[i] = 1
	}
	samples[16] = -1
	w := auditPost(t, r, 100.0, samples)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "degraded", resp["status"])
	assert.Equal(t, float64(16), resp["longest_flatline"])
}

func TestAuditRoute_FindingsFixedOrder(t *testing.T) {
	r := NewRouter()
	// Degraded on every metric at once: 5 isolated clips per 1000 (0.5%),
	// a 16-sample run at [101,116], base level ±10 (mean >= 10%).
	n := 1000
	samples := make([]float64, n)
	for i := range samples {
		if i%2 == 0 {
			samples[i] = 10
		} else {
			samples[i] = -10
		}
	}
	for _, i := range []int{0, 200, 400, 600, 800} {
		samples[i] = 100
	}
	for i := 101; i <= 116; i++ {
		samples[i] = 50
	}
	w := auditPost(t, r, 100.0, samples)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Status   string `json:"status"`
		Findings []struct {
			Code     string `json:"code"`
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "degraded", resp.Status)
	require.Len(t, resp.Findings, 3)
	assert.Equal(t, []string{"clipping", "flatline", "high_mean_level"},
		[]string{resp.Findings[0].Code, resp.Findings[1].Code, resp.Findings[2].Code})
}

func TestAuditRoute_DeterministicBody(t *testing.T) {
	r := NewRouter()
	samples := auditSamples(0, 256) // all zero -> rejected flatline
	first := auditPost(t, r, 100.0, samples).Body.String()
	for i := 0; i < 3; i++ {
		assert.Equal(t, first, auditPost(t, r, 100.0, samples).Body.String())
	}
}

func TestAuditRoute_LengthBoundariesAccepted(t *testing.T) {
	r := NewRouter()
	for _, n := range []int{256, 20000} {
		samples := make([]float64, n)
		for i := range samples {
			if i%2 == 0 {
				samples[i] = 1
			} else {
				samples[i] = -1
			}
		}
		w := auditPost(t, r, 100.0, samples)
		assert.Equal(t, http.StatusOK, w.Code, "length %d: %s", n, w.Body.String())
	}
}

func TestAuditRoute_MaximalFullScaleAndSamplesAreRejectedCompletely(t *testing.T) {
	// Both full_scale and every sample at the float64 ceiling are legal
	// (finite, positive full_scale, valid length). The report must come
	// back whole with mean ratio exactly 1 and status rejected; an
	// overflow to +Inf made encoding/json fail and the response incomplete.
	r := NewRouter()
	samples := auditSamples(math.MaxFloat64, 256)
	w := auditPost(t, r, math.MaxFloat64, samples)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "rejected", resp["status"])
	assert.Equal(t, float64(1), resp["clipping_ratio"])
	assert.Equal(t, float64(1), resp["mean_abs_ratio"])
	assert.Equal(t, float64(256), resp["longest_flatline"])
	findings := resp["findings"].([]any)
	assert.NotEmpty(t, findings)
	for _, f := range findings {
		assert.Contains(t, []string{"clipping", "flatline", "high_mean_level"},
			f.(map[string]any)["code"])
	}
}

// --- validation: every error keeps the existing envelope and is located -----

func TestAuditValidation_FullScale(t *testing.T) {
	r := NewRouter()

	for _, tc := range []struct {
		name       string
		token      string
		constraint string
	}{
		{"missing", "", "required"},
		{"null", "null", "type"},
		{"string", `"100"`, "type"},
		{"boolean", "true", "type"},
		{"object", "{}", "type"},
		{"array", "[]", "type"},
		{"zero", "0", "range"},
		{"negative", "-1", "range"},
		{"NaN", "NaN", "finite"},
		{"Infinity", "Infinity", "finite"},
		{"negative Infinity", "-Infinity", "finite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw string
			if tc.token == "" {
				raw = fmt.Sprintf(`{"samples":[%s]}`, strings.Repeat("0,", 255)+"0")
			} else {
				raw = fmt.Sprintf(`{"full_scale":%s,"samples":[%s]}`,
					tc.token, strings.Repeat("0,", 255)+"0")
			}
			w := auditPostRaw(t, r, raw)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var fe FieldError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
			assert.Equal(t, "validation_failed", fe.Error)
			assert.Equal(t, "full_scale", fe.Field)
			assert.Nil(t, fe.Index)
			assert.Equal(t, tc.constraint, fe.Constraint)
			assert.NotContains(t, w.Body.String(), "findings")
		})
	}

	// A tiny positive boundary value is accepted.
	w := auditPost(t, r, 1e-308, auditSamples(0, 256))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestAuditValidation_Samples(t *testing.T) {
	r := NewRouter()
	zeros := strings.Repeat("0,", 255) + "0"

	for _, tc := range []struct {
		name       string
		raw        string
		field      string
		index      *int
		constraint string
	}{
		{
			name:       "samples missing",
			raw:        `{"full_scale":100}`,
			field:      "samples",
			constraint: "required",
		},
		{
			name:       "samples null",
			raw:        `{"full_scale":100,"samples":null}`,
			field:      "samples",
			constraint: "type",
		},
		{
			name:       "samples scalar",
			raw:        `{"full_scale":100,"samples":2}`,
			field:      "samples",
			constraint: "type",
		},
		{
			name:       "samples object",
			raw:        `{"full_scale":100,"samples":{}}`,
			field:      "samples",
			constraint: "type",
		},
		{
			name:       "one below minimum length",
			raw:        fmt.Sprintf(`{"full_scale":100,"samples":[%s]}`, strings.Repeat("0,", 254)+"0"),
			field:      "samples",
			constraint: "min_length",
		},
		{
			name:       "one above maximum length",
			raw:        fmt.Sprintf(`{"full_scale":100,"samples":[%s]}`, strings.Repeat("0,", 20000)+"0"),
			field:      "samples",
			constraint: "max_length",
		},
		{
			name: "string element locates its index",
			raw: func() string {
				vals := make([]string, 256)
				for i := range vals {
					vals[i] = "0"
				}
				vals[42] = `"x"`
				return fmt.Sprintf(`{"full_scale":100,"samples":[%s]}`, strings.Join(vals, ","))
			}(),
			field:      "samples",
			index:      intPtrVal(42),
			constraint: "type",
		},
		{
			name: "null element locates its index",
			raw: func() string {
				vals := make([]string, 256)
				for i := range vals {
					vals[i] = "0"
				}
				vals[0] = "null"
				return fmt.Sprintf(`{"full_scale":100,"samples":[%s]}`, strings.Join(vals, ","))
			}(),
			field:      "samples",
			index:      intPtrVal(0),
			constraint: "type",
		},
		{
			name:       "NaN element locates its index as finite",
			raw:        fmt.Sprintf(`{"full_scale":100,"samples":[%s]}`, "NaN,"+zeros),
			field:      "samples",
			index:      intPtrVal(0),
			constraint: "finite",
		},
		{
			name: "Infinity element locates its index as finite",
			raw: func() string {
				vals := make([]string, 256)
				for i := range vals {
					vals[i] = "0"
				}
				vals[255] = "-Infinity"
				return fmt.Sprintf(`{"full_scale":100,"samples":[%s]}`, strings.Join(vals, ","))
			}(),
			field:      "samples",
			index:      intPtrVal(255),
			constraint: "finite",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := auditPostRaw(t, r, tc.raw)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var fe FieldError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
			assert.Equal(t, "validation_failed", fe.Error)
			assert.Equal(t, tc.field, fe.Field)
			assert.Equal(t, tc.constraint, fe.Constraint)
			if tc.index == nil {
				assert.Nil(t, fe.Index, "field-level error carries no index: %s", fe.Message)
			} else {
				require.NotNil(t, fe.Index)
				assert.Equal(t, *tc.index, *fe.Index)
			}
			// A rejected request never returns a partial report.
			assert.NotContains(t, w.Body.String(), "clipping_ratio")
			assert.NotContains(t, w.Body.String(), "longest_flatline")
			assert.NotContains(t, w.Body.String(), "findings")
		})
	}
}

func TestAuditValidation_UnknownAndMalformed(t *testing.T) {
	r := NewRouter()
	zeros := strings.Repeat("0,", 255) + "0"

	// Unknown top-level fields are named, including case variants of the
	// contract names.
	for _, tc := range []struct {
		name  string
		raw   string
		field string
	}{
		{"extra", fmt.Sprintf(`{"full_scale":100,"samples":[%s],"extra":1}`, zeros), "extra"},
		{"capitalized full_scale", fmt.Sprintf(`{"Full_Scale":100,"samples":[%s]}`, zeros), "Full_Scale"},
		{"capitalized samples", fmt.Sprintf(`{"full_scale":100,"Samples":[%s]}`, zeros), "Samples"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := auditPostRaw(t, r, tc.raw)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var fe FieldError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
			assert.Equal(t, "validation_failed", fe.Error)
			assert.Equal(t, tc.field, fe.Field)
			assert.Equal(t, "unknown", fe.Constraint)
		})
	}

	// Malformed JSON and trailing content reuse the document envelope.
	w := auditPostRaw(t, r, `{"full_scale":100,"samples":`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	var fe FieldError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "invalid_json", fe.Error)

	w = auditPostRaw(t, r, fmt.Sprintf(`{"full_scale":100,"samples":[%s}]}{}`, zeros))
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "invalid_json", fe.Error)
}

func TestAuditValidation_ExistingEndpointsUnchanged(t *testing.T) {
	// The new route must not answer the legacy paths, and the legacy
	// analyzer still validates its own contract (64-sample minimum).
	r := NewRouter()
	req := httptest.NewRequest(http.MethodPost, auditRoute,
		strings.NewReader(fmt.Sprintf(`{"full_scale":100,"samples":[%s]}`,
			strings.Repeat("0,", 63)+"0")))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
	var fe FieldError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fe))
	assert.Equal(t, "samples", fe.Field)
	assert.Equal(t, "min_length", fe.Constraint)

	// analyze still applies its own 64-sample floor, not 256.
	legacy := postRaw(t, r, fmt.Sprintf(`{"sample_rate":16000,"amplitudes":[%s]}`,
		strings.Repeat("0,", 63)+"0"))
	assert.Equal(t, http.StatusOK, legacy.Code, legacy.Body.String())
}
