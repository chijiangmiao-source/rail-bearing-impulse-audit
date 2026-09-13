// Command verify is the one-shot acceptance service. It talks to a running API
// instance over HTTP and checks the complete detection contract: thresholds,
// merging, length filtering, peak ties, severity boundaries, undecidable
// cases, field/sample-level validation and determinism.
//
// Exit code is 0 only when every scenario passes.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stretchr/testify/assert"

	"trackside-pulse-api/internal/pulse"
)

type pulseDTO struct {
	Start     int     `json:"start"`
	End       int     `json:"end"`
	PeakIndex int     `json:"peak_index"`
	Peak      float64 `json:"peak"`
	Severity  string  `json:"severity"`
	Baseline  float64 `json:"baseline"`
}

type rangeDTO struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type analyzeDTO struct {
	SampleRate     float64    `json:"sample_rate"`
	Decidable      bool       `json:"decidable"`
	Baseline       float64    `json:"baseline"`
	Reason         string     `json:"reason"`
	Pulses         []pulseDTO `json:"pulses"`
	ExcludedRanges []rangeDTO `json:"excluded_ranges"`
}

type errorDTO struct {
	Error      string `json:"error"`
	Field      string `json:"field"`
	Index      *int   `json:"index"`
	Constraint string `json:"constraint"`
	Message    string `json:"message"`
}

// failureRecorder adapts testify's TestingT and counts failures per scenario.
type failureRecorder struct {
	failed bool
	logs   []string
}

func (f *failureRecorder) Errorf(format string, args ...any) {
	f.failed = true
	f.logs = append(f.logs, "    "+fmt.Sprintf(format, args...))
}

type client struct {
	baseURL string
	http    *http.Client
}

func main() {
	baseURL := flag.String("base-url", envOr("VERIFY_BASE_URL", "http://api:8080"),
		"API base URL (env VERIFY_BASE_URL)")
	mode := flag.String("mode", "accept", "accept: run acceptance suite; health: readiness probe")
	flag.Parse()

	c := &client{
		baseURL: strings.TrimRight(*baseURL, "/"),
		http:    &http.Client{Timeout: 10 * time.Second},
	}

	if *mode == "health" {
		if err := c.waitHealthy(3 * time.Second); err != nil {
			fmt.Printf("health: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("health: ok")
		return
	}

	if err := c.waitHealthy(60 * time.Second); err != nil {
		fmt.Printf("FATAL: API never became healthy: %v\n", err)
		os.Exit(1)
	}

	failed, total := 0, 0
	run := func(name string, fn func(a *assert.Assertions)) {
		total++
		rec := &failureRecorder{}
		fn(assert.New(rec))
		if rec.failed {
			failed++
			fmt.Printf("FAIL  %s\n", name)
			for _, l := range rec.logs {
				fmt.Println(l)
			}
		} else {
			fmt.Printf("PASS  %s\n", name)
		}
	}

	run("health endpoint", c.health)
	run("two pulses: severe and general, ascending order", c.scenarioBasic)
	run("even median is arithmetic mean of two middles", c.scenarioEvenMedian)
	run("odd median over 65 samples", c.scenarioOddMedian)
	run("candidates 3 samples apart merge; 4 apart stay separate", c.scenarioMerging)
	run("value equal to threshold is not a candidate", c.scenarioThresholdStrict)
	run("interval shorter than 4 samples is discarded", c.scenarioShortDiscard)
	run("peak ties resolve to earliest index", c.scenarioPeakTie)
	run("peak equal to 12x baseline is general (strict)", c.scenarioSeverityBoundary)
	run("zero baseline is undecidable and fabricates no pulses", c.scenarioZeroBaseline)
	run("zero baseline despite one spike still fabricates no pulses", c.scenarioZeroBaselineSpike)
	run("no retained intervals is undecidable", c.scenarioNoPulses)
	run("boundary sample rates and lengths are accepted", c.scenarioInputBoundaries)
	run("same input yields byte-identical responses", c.scenarioDeterminism)
	run("sample rate below minimum is located at sample_rate", c.scenarioRateLow)
	run("sample rate above maximum is located at sample_rate", c.scenarioRateHigh)
	run("wrong sample rate type is located at sample_rate", c.scenarioRateType)
	run("missing sample_rate is located at sample_rate", c.scenarioRateMissing)
	run("missing amplitudes is located at amplitudes", c.scenarioAmplitudesMissing)
	run("too few samples is located at amplitudes", c.scenarioTooFew)
	run("too many samples is located at amplitudes", c.scenarioTooMany)
	run("non-array amplitudes is located at amplitudes", c.scenarioNotArray)
	run("non-numeric element is located at sample index", c.scenarioElementType)
	run("null element is located at sample index", c.scenarioElementNull)
	run("NaN token is located at sample index", c.scenarioElementNaN)
	run("Infinity token is located at sample index", c.scenarioElementInfinity)
	run("unknown field is rejected and named", c.scenarioUnknownField)
	run("malformed JSON is rejected", c.scenarioMalformed)
	run("seam shielding recomputes baseline and severity from kept samples", c.scenarioExcludedSeam)
	run("end-to-end: seam shielding, candidate break and range echo in one sample", c.scenarioExcludedCombined)
	run("excluded interval breaks candidate merging even within 3 samples", c.scenarioExcludedBreak)
	run("adopted excluded_ranges are echoed verbatim and in order", c.scenarioExcludedEcho)
	run("omitted and empty excluded_ranges keep the legacy response byte-for-byte", c.scenarioExcludedLegacy)
	run("legal boundary excluded_ranges are accepted and echoed", c.scenarioExcludedBoundaries)
	run("inverted excluded range is located at its element index", c.scenarioExcludedInverted)
	run("out-of-bounds excluded range is located at its element index", c.scenarioExcludedOutOfRange)
	run("non-ascending excluded ranges are located at the element index", c.scenarioExcludedOrder)
	run("overlapping excluded ranges are located at the element index", c.scenarioExcludedOverlap)
	run("fewer than 64 kept samples is located at excluded_ranges", c.scenarioExcludedTooFew)
	run("non-object excluded range element is located at the index", c.scenarioExcludedElementType)
	run("null excluded-range endpoint is a located type error", c.scenarioExcludedNullEndpoint)
	run("non-contract excluded-range endpoint names are unknown fields", c.scenarioExcludedCaseSensitiveNames)
	run("first range dropping kept samples below 64 is the located offender", c.scenarioExcludedTooFewFirstOffender)

	fmt.Printf("\n%d/%d scenarios passed\n", total-failed, total)
	if failed > 0 {
		os.Exit(1)
	}
	fmt.Println("ACCEPTANCE OK")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// --- fixtures ---------------------------------------------------------------

func flatSamples(n int, v float64, set map[int]float64) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = v
		if x, ok := set[i]; ok {
			s[i] = x
		}
	}
	return s
}

func setRange(m map[int]float64, from, to int, v float64) {
	for i := from; i <= to; i++ {
		m[i] = v
	}
}

// --- scenarios --------------------------------------------------------------

func (c *client) health(a *assert.Assertions) {
	code, _ := c.get(a, "/healthz")
	a.Equal(http.StatusOK, code)
}

func (c *client) scenarioBasic(a *assert.Assertions) {
	// 64 samples all equal to 2 -> median 2, threshold >12, severe >24.
	set := map[int]float64{}
	setRange(set, 5, 8, 13) // 4-sample interval, peak 25 at index 6
	set[6] = 25
	setRange(set, 40, 43, 13) // second 4-sample interval, peak ties -> idx 40

	resp, body := c.analyzeValues(a, 16000, flatSamples(64, 2, set))
	a.True(resp.Decidable)
	a.Equal(2.0, resp.Baseline)
	a.Len(resp.Pulses, 2)
	if a.Len(resp.Pulses, 2) {
		p0 := resp.Pulses[0]
		a.Equal(5, p0.Start)
		a.Equal(8, p0.End)
		a.Equal(6, p0.PeakIndex)
		a.Equal(25.0, p0.Peak)
		a.Equal("severe", p0.Severity)
		a.Equal(2.0, p0.Baseline)
		p1 := resp.Pulses[1]
		a.Equal(40, p1.Start)
		a.Equal(43, p1.End)
		a.Equal(40, p1.PeakIndex)
		a.Equal(13.0, p1.Peak)
		a.Equal("general", p1.Severity)
	}
	a.NotEmpty(body)
}

func (c *client) scenarioEvenMedian(a *assert.Assertions) {
	// 32 twos then 32 eights; overwrite four eights at [32,35] with 40.
	// Sorted multiset stays 2x32, 8x28, 40x4 so the two middle values are
	// still 2 and 8 -> median (2+8)/2 = 5. Threshold >30, severe >60,
	// making the 40-run a general pulse.
	s := make([]float64, 64)
	for i := 0; i < 32; i++ {
		s[i] = 2
	}
	for i := 32; i < 64; i++ {
		s[i] = 8
	}
	for i := 32; i <= 35; i++ {
		s[i] = 40
	}
	resp, _ := c.analyzeValues(a, 16000, s)
	a.True(resp.Decidable)
	a.Equal(5.0, resp.Baseline)
	if a.Len(resp.Pulses, 1) {
		a.Equal(32, resp.Pulses[0].Start)
		a.Equal(35, resp.Pulses[0].End)
		a.Equal(40.0, resp.Pulses[0].Peak)
		a.Equal("general", resp.Pulses[0].Severity)
	}
}

func (c *client) scenarioOddMedian(a *assert.Assertions) {
	// 65 samples all equal to 4 -> median 4, threshold >24, severe >48.
	set := map[int]float64{}
	setRange(set, 10, 13, 50)
	resp, _ := c.analyzeValues(a, 48000, flatSamples(65, 4, set))
	a.True(resp.Decidable)
	a.Equal(4.0, resp.Baseline)
	if a.Len(resp.Pulses, 1) {
		a.Equal(10, resp.Pulses[0].Start)
		a.Equal(13, resp.Pulses[0].End)
		a.Equal(10, resp.Pulses[0].PeakIndex)
		a.Equal(50.0, resp.Pulses[0].Peak)
		a.Equal("severe", resp.Pulses[0].Severity)
	}
}

func (c *client) scenarioMerging(a *assert.Assertions) {
	set := map[int]float64{}
	setRange(set, 10, 11, 20) // run A (2 samples)
	setRange(set, 15, 16, 20) // run B, gap 3 -> merges with A: [10,16]
	setRange(set, 21, 24, 20) // run C, gap 4 -> stays separate: [21,24]
	resp, _ := c.analyzeValues(a, 16000, flatSamples(64, 2, set))
	a.True(resp.Decidable)
	if a.Len(resp.Pulses, 2) {
		a.Equal(10, resp.Pulses[0].Start)
		a.Equal(16, resp.Pulses[0].End)
		a.Equal(7, resp.Pulses[0].End-resp.Pulses[0].Start+1)
		a.Equal(10, resp.Pulses[0].PeakIndex)
		a.Equal(21, resp.Pulses[1].Start)
		a.Equal(24, resp.Pulses[1].End)
	}
}

func (c *client) scenarioThresholdStrict(a *assert.Assertions) {
	// Exactly 6x baseline: not strictly greater -> no candidate at all.
	set := map[int]float64{}
	setRange(set, 30, 33, 12)
	resp, _ := c.analyzeValues(a, 16000, flatSamples(64, 2, set))
	a.False(resp.Decidable)
	a.Equal("no_pulses", resp.Reason)
	a.Empty(resp.Pulses)
}

func (c *client) scenarioShortDiscard(a *assert.Assertions) {
	set := map[int]float64{}
	setRange(set, 5, 7, 30) // 3 samples, isolated -> discarded
	resp, _ := c.analyzeValues(a, 16000, flatSamples(64, 2, set))
	a.False(resp.Decidable)
	a.Equal("no_pulses", resp.Reason)
	a.Empty(resp.Pulses)
}

func (c *client) scenarioPeakTie(a *assert.Assertions) {
	set := map[int]float64{}
	setRange(set, 8, 12, 20) // five equal peaks
	resp, _ := c.analyzeValues(a, 16000, flatSamples(64, 2, set))
	if a.Len(resp.Pulses, 1) {
		a.Equal(8, resp.Pulses[0].PeakIndex)
		a.Equal(20.0, resp.Pulses[0].Peak)
	}
}

func (c *client) scenarioSeverityBoundary(a *assert.Assertions) {
	// Peak equal to 12x baseline (24) is general, not severe.
	set := map[int]float64{}
	setRange(set, 2, 5, 24)
	resp, _ := c.analyzeValues(a, 16000, flatSamples(64, 2, set))
	if a.Len(resp.Pulses, 1) {
		a.Equal("general", resp.Pulses[0].Severity)
	}
}

func (c *client) scenarioZeroBaseline(a *assert.Assertions) {
	resp, _ := c.analyzeValues(a, 16000, make([]float64, 64))
	a.False(resp.Decidable)
	a.Equal(0.0, resp.Baseline)
	a.Equal("baseline_zero", resp.Reason)
	a.Empty(resp.Pulses)
}

func (c *client) scenarioZeroBaselineSpike(a *assert.Assertions) {
	// 63 zeros + one spike: even median is still 0; the spike must not be
	// reported as a pulse just because threshold is 0.
	s := make([]float64, 64)
	s[10] = 99
	resp, _ := c.analyzeValues(a, 16000, s)
	a.False(resp.Decidable)
	a.Equal("baseline_zero", resp.Reason)
	a.Empty(resp.Pulses)
}

func (c *client) scenarioNoPulses(a *assert.Assertions) {
	// Baseline 2 but nothing exceeds 12.
	resp, _ := c.analyzeValues(a, 16000, flatSamples(64, 2, nil))
	a.False(resp.Decidable)
	a.Equal(2.0, resp.Baseline)
	a.Equal("no_pulses", resp.Reason)
	a.Empty(resp.Pulses)
}

func (c *client) scenarioInputBoundaries(a *assert.Assertions) {
	set := map[int]float64{}
	setRange(set, 0, 3, 30)

	// Lowest sample rate and length.
	resp, _ := c.analyzeValues(a, 1000, flatSamples(64, 2, set))
	a.True(resp.Decidable)
	if a.Len(resp.Pulses, 1) {
		a.Equal(0, resp.Pulses[0].Start)
	}

	// Highest sample rate.
	resp, _ = c.analyzeValues(a, 48000, flatSamples(64, 2, set))
	a.True(resp.Decidable)

	// Largest allowed sample count.
	big := flatSamples(20000, 2, set)
	resp, _ = c.analyzeValues(a, 16000, big)
	a.True(resp.Decidable)
	if a.Len(resp.Pulses, 1) {
		a.Equal(3, resp.Pulses[0].End)
	}
}

func (c *client) scenarioDeterminism(a *assert.Assertions) {
	set := map[int]float64{}
	setRange(set, 5, 8, 13)
	set[6] = 25
	setRange(set, 40, 43, 13)
	samples := flatSamples(64, 2, set)

	_, first := c.analyzeValues(a, 16000, samples)
	for i := 0; i < 3; i++ {
		_, again := c.analyzeValues(a, 16000, samples)
		a.Equal(string(first), string(again), "response %d differs", i+2)
	}
}

// --- validation scenarios ---------------------------------------------------

func (c *client) scenarioRateLow(a *assert.Assertions) {
	code, e := c.analyzeError(a, 999, make([]float64, 64))
	a.Equal(http.StatusBadRequest, code)
	a.Equal("sample_rate", e.Field)
	a.Equal("range", e.Constraint)
}

func (c *client) scenarioRateHigh(a *assert.Assertions) {
	code, e := c.analyzeError(a, 48001, make([]float64, 64))
	a.Equal(http.StatusBadRequest, code)
	a.Equal("sample_rate", e.Field)
	a.Equal("range", e.Constraint)
}

func (c *client) scenarioRateType(a *assert.Assertions) {
	code, body := c.postRaw(a, fmt.Sprintf(
		`{"sample_rate":"16000","amplitudes":[%s]}`, zerosCSV(64)))
	a.Equal(http.StatusBadRequest, code)
	e := decodeError(a, body)
	a.Equal("sample_rate", e.Field)
	a.Equal("type", e.Constraint)
}

func (c *client) scenarioRateMissing(a *assert.Assertions) {
	code, body := c.postRaw(a, fmt.Sprintf(
		`{"amplitudes":[%s]}`, zerosCSV(64)))
	a.Equal(http.StatusBadRequest, code)
	e := decodeError(a, body)
	a.Equal("sample_rate", e.Field)
	a.Equal("required", e.Constraint)
}

func (c *client) scenarioAmplitudesMissing(a *assert.Assertions) {
	code, body := c.postRaw(a, `{"sample_rate":16000}`)
	a.Equal(http.StatusBadRequest, code)
	e := decodeError(a, body)
	a.Equal("amplitudes", e.Field)
	a.Equal("required", e.Constraint)
}

func (c *client) scenarioTooFew(a *assert.Assertions) {
	code, e := c.analyzeError(a, 16000, make([]float64, pulse.MinSamples-1))
	a.Equal(http.StatusBadRequest, code)
	a.Equal("amplitudes", e.Field)
	a.Equal("min_length", e.Constraint)
}

func (c *client) scenarioTooMany(a *assert.Assertions) {
	code, e := c.analyzeError(a, 16000, make([]float64, pulse.MaxSamples+1))
	a.Equal(http.StatusBadRequest, code)
	a.Equal("amplitudes", e.Field)
	a.Equal("max_length", e.Constraint)
}

func (c *client) scenarioNotArray(a *assert.Assertions) {
	code, body := c.postRaw(a, `{"sample_rate":16000,"amplitudes":2}`)
	a.Equal(http.StatusBadRequest, code)
	e := decodeError(a, body)
	a.Equal("amplitudes", e.Field)
	a.Equal("type", e.Constraint)
}

func (c *client) scenarioElementType(a *assert.Assertions) {
	values := make([]string, 64)
	for i := range values {
		values[i] = "0"
	}
	values[2] = `"x"`
	code, body := c.postRaw(a, fmt.Sprintf(
		`{"sample_rate":16000,"amplitudes":[%s]}`, strings.Join(values, ",")))
	a.Equal(http.StatusBadRequest, code)
	e := decodeError(a, body)
	a.Equal("amplitudes", e.Field)
	if a.NotNil(e.Index) {
		a.Equal(2, *e.Index)
	}
	a.Equal("type", e.Constraint)
}

func (c *client) scenarioElementNull(a *assert.Assertions) {
	values := make([]string, 64)
	for i := range values {
		values[i] = "0"
	}
	values[0] = "null"
	code, body := c.postRaw(a, fmt.Sprintf(
		`{"sample_rate":16000,"amplitudes":[%s]}`, strings.Join(values, ",")))
	a.Equal(http.StatusBadRequest, code)
	e := decodeError(a, body)
	a.Equal("amplitudes", e.Field)
	if a.NotNil(e.Index) {
		a.Equal(0, *e.Index)
	}
	a.Equal("type", e.Constraint)
}

func (c *client) scenarioElementNaN(a *assert.Assertions) {
	values := make([]string, 64)
	for i := range values {
		values[i] = "0"
	}
	values[1] = "NaN"
	code, body := c.postRaw(a, fmt.Sprintf(
		`{"sample_rate":16000,"amplitudes":[%s]}`, strings.Join(values, ",")))
	a.Equal(http.StatusBadRequest, code)
	e := decodeError(a, body)
	a.Equal("amplitudes", e.Field)
	if a.NotNil(e.Index) {
		a.Equal(1, *e.Index)
	}
	// NaN is not a JSON number; the server locates it at the offending
	// sample index and reports it as a non-finite value.
	a.Equal("finite", e.Constraint)
}

func (c *client) scenarioElementInfinity(a *assert.Assertions) {
	values := make([]string, 64)
	for i := range values {
		values[i] = "0"
	}
	values[0] = "-Infinity"
	code, body := c.postRaw(a, fmt.Sprintf(
		`{"sample_rate":16000,"amplitudes":[%s]}`, strings.Join(values, ",")))
	a.Equal(http.StatusBadRequest, code)
	e := decodeError(a, body)
	a.Equal("amplitudes", e.Field)
	if a.NotNil(e.Index) {
		a.Equal(0, *e.Index)
	}
	a.Equal("finite", e.Constraint)
}

func (c *client) scenarioUnknownField(a *assert.Assertions) {
	code, body := c.postRaw(a, fmt.Sprintf(
		`{"sample_rate":16000,"amplitudes":[%s],"extra":1}`, zerosCSV(64)))
	a.Equal(http.StatusBadRequest, code)
	e := decodeError(a, body)
	a.Equal("extra", e.Field)
}

func (c *client) scenarioMalformed(a *assert.Assertions) {
	code, body := c.postRaw(a, `{"sample_rate":16000,"amplitudes":`)
	a.Equal(http.StatusBadRequest, code)
	e := decodeError(a, body)
	a.Equal("invalid_json", e.Error)
}

// --- excluded_ranges scenarios ----------------------------------------------

// seamSamples builds the wheel-seam end-to-end fixture: 130 samples with a
// 65-sample high-amplitude seam block at [10,74] (30), a 4-sample bearing
// burst at [80,83] (13) and 61 ordinary samples (2).
func seamSamples() []float64 {
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

func (c *client) scenarioExcludedSeam(a *assert.Assertions) {
	s := seamSamples()

	// With the seam kept it dominates the even median: (13+30)/2 = 21.5 and
	// the 13-burst stays below threshold -> undecidable.
	full, _ := c.analyzeValues(a, 16000, s)
	a.False(full.Decidable)
	a.Equal(21.5, full.Baseline)
	a.Equal("no_pulses", full.Reason)
	a.Empty(full.Pulses)

	// Shielding the seam recomputes baseline from the 65 kept samples
	// (61 twos + 4 thirteens): median 2, threshold 12, so the burst is now a
	// general pulse, and severity is also decided from kept samples.
	resp, body := c.analyzeRanges(a, 16000, s, []rangeDTO{{Start: 10, End: 74}})
	a.True(resp.Decidable, "body: %s", body)
	a.Equal(2.0, resp.Baseline)
	if a.Len(resp.Pulses, 1) {
		p := resp.Pulses[0]
		a.Equal(80, p.Start)
		a.Equal(83, p.End)
		a.Equal(80, p.PeakIndex)
		a.Equal(13.0, p.Peak)
		a.Equal("general", p.Severity)
		a.Equal(2.0, p.Baseline)
	}
}

func (c *client) scenarioExcludedCombined(a *assert.Assertions) {
	// One signal, three requests, proving shielding, disconnection and echo.
	//
	// 130 samples: a 65-sample seam at 30 on [10,74], a 4-sample bearing
	// burst at 13 on [80,83], two 2-sample 20-runs at [100,101] and
	// [103,104] (a single-sample gap at 102), ordinary value 2 elsewhere.
	s := seamSamples()
	for _, i := range []int{100, 101, 103, 104} {
		s[i] = 20
	}

	// 1) Nothing excluded: sorted multiset is 57 twos, 4 thirteens, 4
	//    twenties and 65 thirties, so the even median is (20+30)/2 = 25;
	//    threshold 150 leaves neither the 13-burst nor the 20-runs qualified.
	full, body := c.analyzeValues(a, 16000, s)
	a.Equal(25.0, full.Baseline, "body: %s", body)
	a.False(full.Decidable)
	a.Equal("no_pulses", full.Reason)
	a.Empty(full.Pulses)

	// 2) Shield only the seam: baseline drops to 2. The 13-burst is detected
	//    and the two 20-runs merge across the one-sample gap (<= 3).
	seamOnly, body := c.analyzeRanges(a, 16000, s, []rangeDTO{{Start: 10, End: 74}})
	a.Equal(2.0, seamOnly.Baseline, "body: %s", body)
	if a.Len(seamOnly.Pulses, 2) {
		a.Equal(80, seamOnly.Pulses[0].Start)
		a.Equal(83, seamOnly.Pulses[0].End)
		a.Equal("general", seamOnly.Pulses[0].Severity)
		a.Equal(100, seamOnly.Pulses[1].Start)
		a.Equal(104, seamOnly.Pulses[1].End, "the 1-sample gap must normally merge")
	}

	// 3) Also exclude the single gap sample [102,102]: it becomes an
	//    uncrossable boundary, so the two runs stay 2-sample candidates and
	//    are filtered. Only the bearing burst survives. Exactly 64 samples
	//    remain, and both adopted ranges are echoed verbatim.
	adopted := []rangeDTO{{Start: 10, End: 74}, {Start: 102, End: 102}}
	resp, body := c.analyzeRanges(a, 16000, s, adopted)
	a.True(resp.Decidable, "body: %s", body)
	a.Equal(2.0, resp.Baseline)
	if a.Len(resp.Pulses, 1, "split candidates must be filtered: %s", body) {
		a.Equal(80, resp.Pulses[0].Start)
		a.Equal(83, resp.Pulses[0].End)
		a.Equal(13.0, resp.Pulses[0].Peak)
		a.Equal("general", resp.Pulses[0].Severity)
	}
	a.Equal(adopted, resp.ExcludedRanges, "adopted ranges echoed for point review")
}

func (c *client) scenarioExcludedBreak(a *assert.Assertions) {
	// 68 samples: two 2-sample bursts with a 2-sample gap.
	s := make([]float64, 68)
	for i := range s {
		s[i] = 2
	}
	for _, i := range []int{10, 11, 14, 15} {
		s[i] = 20
	}

	// Gap of 2 (<= 3) normally merges them into one retained interval.
	plain, _ := c.analyzeValues(a, 16000, s)
	if a.Len(plain.Pulses, 1) {
		a.Equal(10, plain.Pulses[0].Start)
		a.Equal(15, plain.Pulses[0].End)
	}

	// Excluding the two gap samples makes the seam an uncrossable boundary:
	// the pieces stay separate 2-sample candidates and both are filtered.
	resp, body := c.analyzeRanges(a, 16000, s, []rangeDTO{{Start: 12, End: 13}})
	a.False(resp.Decidable, "body: %s", body)
	a.Equal("no_pulses", resp.Reason)
	a.Empty(resp.Pulses)
	// Echo is present even on an undecidable response, verbatim.
	if a.Len(resp.ExcludedRanges, 1) {
		a.Equal(rangeDTO{Start: 12, End: 13}, resp.ExcludedRanges[0])
	}
}

func (c *client) scenarioExcludedEcho(a *assert.Assertions) {
	// Quiet 200-sample signal with two disjoint excluded blocks; the response
	// must echo the adopted ranges verbatim and in request order.
	s := make([]float64, 200)
	for i := range s {
		s[i] = 2
	}
	want := []rangeDTO{{Start: 0, End: 9}, {Start: 100, End: 105}}
	resp, body := c.analyzeRanges(a, 16000, s, want)
	a.Equal(2.0, resp.Baseline, "body: %s", body)
	if a.Len(resp.ExcludedRanges, 2) {
		a.Equal(want[0], resp.ExcludedRanges[0])
		a.Equal(want[1], resp.ExcludedRanges[1])
	}
}

func (c *client) scenarioExcludedLegacy(a *assert.Assertions) {
	zeros := zerosCSV(64)
	bodies := make([]string, 3)
	for i, tail := range []string{
		``,
		`,"excluded_ranges":[]`,
		`,"excluded_ranges":null`,
	} {
		code, body := c.postRaw(a, fmt.Sprintf(
			`{"sample_rate":16000,"amplitudes":[%s]%s}`, zeros, tail))
		a.Equal(http.StatusOK, code, "body: %s", body)
		bodies[i] = string(body)
		a.NotContains(string(body), "excluded_ranges",
			"legacy responses must not carry the optional field")
	}
	a.Equal(bodies[0], bodies[1], "empty array must be byte-identical to omitted")
	a.Equal(bodies[0], bodies[2], "null must be byte-identical to omitted")
}

func (c *client) scenarioExcludedBoundaries(a *assert.Assertions) {
	// 67 samples; point exclusions at both edges and one adjacent leave
	// exactly 64 kept samples. Field contract locked end to end.
	s := make([]float64, 67)
	for i := range s {
		s[i] = 2
	}
	for _, i := range []int{2, 3, 4, 5} {
		s[i] = 20
	}
	want := []rangeDTO{{Start: 0, End: 0}, {Start: 1, End: 1}, {Start: 66, End: 66}}
	resp, body := c.analyzeRanges(a, 16000, s, want)
	a.True(resp.Decidable, "body: %s", body)
	if a.Len(resp.Pulses, 1) {
		a.Equal(2, resp.Pulses[0].Start)
		a.Equal(5, resp.Pulses[0].End)
	}
	if a.Len(resp.ExcludedRanges, 3) {
		a.Equal(want, resp.ExcludedRanges)
	}
}

func (c *client) scenarioExcludedInverted(a *assert.Assertions) {
	e := c.postRangesError(a, 64, `[{"start":10,"end":9}]`)
	a.Equal("excluded_ranges", e.Field)
	if a.NotNil(e.Index) {
		a.Equal(0, *e.Index)
	}
	a.Equal("order", e.Constraint)
}

func (c *client) scenarioExcludedOutOfRange(a *assert.Assertions) {
	// Endpoint at len(amplitudes) is outside the closed [0, n-1] domain.
	e := c.postRangesError(a, 64, `[{"start":0,"end":64}]`)
	a.Equal("excluded_ranges", e.Field)
	if a.NotNil(e.Index) {
		a.Equal(0, *e.Index)
	}
	a.Equal("range", e.Constraint)
}

func (c *client) scenarioExcludedOrder(a *assert.Assertions) {
	e := c.postRangesError(a, 64, `[{"start":10,"end":11},{"start":9,"end":12}]`)
	a.Equal("excluded_ranges", e.Field)
	if a.NotNil(e.Index) {
		a.Equal(1, *e.Index)
	}
	a.Equal("order", e.Constraint)
}

func (c *client) scenarioExcludedOverlap(a *assert.Assertions) {
	e := c.postRangesError(a, 64, `[{"start":10,"end":20},{"start":20,"end":21}]`)
	a.Equal("excluded_ranges", e.Field)
	if a.NotNil(e.Index) {
		a.Equal(1, *e.Index)
	}
	a.Equal("overlap", e.Constraint)
}

func (c *client) scenarioExcludedTooFew(a *assert.Assertions) {
	// 64 samples minus one excluded leaves 63.
	e := c.postRangesError(a, 64, `[{"start":63,"end":63}]`)
	a.Equal("excluded_ranges", e.Field)
	if a.NotNil(e.Index) {
		a.Equal(0, *e.Index)
	}
	a.Equal("min_length", e.Constraint)
}

func (c *client) scenarioExcludedElementType(a *assert.Assertions) {
	// A bare number, a null element, and a non-integer endpoint are each
	// located at their element index with constraint "type".
	for _, tc := range []struct {
		name   string
		ranges string
		index  int
	}{
		{"number element", `[5]`, 0},
		{"null element", `[{"start":0,"end":0},null]`, 1},
		{"fractional endpoint", `[{"start":1.5,"end":3}]`, 0},
		{"string endpoint", `[{"start":"2","end":3}]`, 0},
	} {
		e := c.postRangesError(a, 64, tc.ranges)
		if !a.Equal("excluded_ranges", e.Field, tc.name) {
			continue
		}
		if a.NotNil(e.Index, tc.name) {
			a.Equal(tc.index, *e.Index, tc.name)
		}
		a.Equal("type", e.Constraint, tc.name)
	}

	// A non-array excluded_ranges is a field-level type error without index.
	raw := fmt.Sprintf(`{"sample_rate":16000,"amplitudes":[%s],"excluded_ranges":{"start":1,"end":2}}`,
		zerosCSV(64))
	code, body := c.postRaw(a, raw)
	a.Equal(http.StatusBadRequest, code)
	e := decodeError(a, body)
	a.Equal("excluded_ranges", e.Field)
	a.Nil(e.Index)
	a.Equal("type", e.Constraint)
}

func (c *client) scenarioExcludedNullEndpoint(a *assert.Assertions) {
	// An explicit null endpoint is present but not an integer: a type error
	// located at the element, never a missing-field (required) error.
	for _, tc := range []struct {
		name   string
		ranges string
	}{
		{"null start", `[{"start":null,"end":3}]`},
		{"null end", `[{"start":1,"end":null}]`},
	} {
		e := c.postRangesError(a, 64, tc.ranges)
		if !a.Equal("excluded_ranges", e.Field, tc.name) {
			continue
		}
		if a.NotNil(e.Index, tc.name) {
			a.Equal(0, *e.Index, tc.name)
		}
		a.Equal("type", e.Constraint, tc.name)
	}
}

func (c *client) scenarioExcludedCaseSensitiveNames(a *assert.Assertions) {
	// Endpoint names are a case-sensitive contract: "Start"/"End" are unknown
	// fields of the element and must never be silently accepted.
	for _, tc := range []struct {
		name   string
		ranges string
		key    string
	}{
		{"uppercase names", `[{"Start":1,"End":2}]`, "Start"},
		{"mixed case", `[{"start":1,"End":2}]`, "End"},
	} {
		e := c.postRangesError(a, 64, tc.ranges)
		if !a.Equal("excluded_ranges", e.Field, tc.name) {
			continue
		}
		if a.NotNil(e.Index, tc.name) {
			a.Equal(0, *e.Index, tc.name)
		}
		a.Equal("unknown", e.Constraint, tc.name)
		a.Contains(e.Message, `"`+tc.key+`"`, tc.name)
	}
}

func (c *client) scenarioExcludedTooFewFirstOffender(a *assert.Assertions) {
	// 64 samples: the point exclusion [0,0] already leaves 63 kept samples,
	// so it — not the later big exclusion — is the located offender.
	e := c.postRangesError(a, 64, `[{"start":0,"end":0},{"start":32,"end":63}]`)
	a.Equal("excluded_ranges", e.Field)
	if a.NotNil(e.Index) {
		a.Equal(0, *e.Index)
	}
	a.Equal("min_length", e.Constraint)
}

func (c *client) postRangesError(a *assert.Assertions, n int, rangesJSON string) errorDTO {
	raw := fmt.Sprintf(`{"sample_rate":16000,"amplitudes":[%s],"excluded_ranges":%s}`,
		zerosCSV(n), rangesJSON)
	code, body := c.postRaw(a, raw)
	a.Equal(http.StatusBadRequest, code, "body: %s", body)
	return decodeError(a, body)
}

// --- HTTP helpers -----------------------------------------------------------

func (c *client) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(c.baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("service at %s not healthy after %s", c.baseURL, timeout)
}

func (c *client) get(a *assert.Assertions, path string) (int, []byte) {
	resp, err := http.Get(c.baseURL + path)
	if !a.NoError(err) {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func (c *client) postRaw(a *assert.Assertions, raw string) (int, []byte) {
	resp, err := c.http.Post(c.baseURL+"/api/v1/pulses/analyze",
		"application/json", bytes.NewBufferString(raw))
	if !a.NoError(err) {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func (c *client) analyzeValues(a *assert.Assertions, rate float64, samples []float64) (analyzeDTO, []byte) {
	payload, err := json.Marshal(struct {
		SampleRate float64   `json:"sample_rate"`
		Amplitudes []float64 `json:"amplitudes"`
	}{rate, samples})
	if !a.NoError(err) {
		return analyzeDTO{}, nil
	}
	code, body := c.postRaw(a, string(payload))
	a.Equal(http.StatusOK, code, "body: %s", body)

	var resp analyzeDTO
	if !a.NoError(json.Unmarshal(body, &resp)) {
		return analyzeDTO{}, body
	}
	a.Equal(rate, resp.SampleRate)
	a.NotNil(resp.Pulses)
	return resp, body
}

// analyzeRanges posts a request carrying excluded_ranges and expects 200.
func (c *client) analyzeRanges(a *assert.Assertions, rate float64, samples []float64, ranges []rangeDTO) (analyzeDTO, []byte) {
	payload, err := json.Marshal(struct {
		SampleRate     float64    `json:"sample_rate"`
		Amplitudes     []float64  `json:"amplitudes"`
		ExcludedRanges []rangeDTO `json:"excluded_ranges"`
	}{rate, samples, ranges})
	if !a.NoError(err) {
		return analyzeDTO{}, nil
	}
	code, body := c.postRaw(a, string(payload))
	a.Equal(http.StatusOK, code, "body: %s", body)

	var resp analyzeDTO
	if !a.NoError(json.Unmarshal(body, &resp)) {
		return analyzeDTO{}, body
	}
	a.Equal(rate, resp.SampleRate)
	a.NotNil(resp.Pulses)
	return resp, body
}

// analyzeError posts a well-formed request and expects a 400 envelope.
func (c *client) analyzeError(a *assert.Assertions, rate float64, samples []float64) (int, errorDTO) {
	payload, err := json.Marshal(struct {
		SampleRate float64   `json:"sample_rate"`
		Amplitudes []float64 `json:"amplitudes"`
	}{rate, samples})
	if !a.NoError(err) {
		return 0, errorDTO{}
	}
	code, body := c.postRaw(a, string(payload))
	return code, decodeError(a, body)
}

func decodeError(a *assert.Assertions, body []byte) errorDTO {
	var e errorDTO
	if !a.NoError(json.Unmarshal(body, &e), "body: %s", body) {
		return errorDTO{}
	}
	return e
}

func zerosCSV(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "0"
	}
	return strings.Join(parts, ",")
}
