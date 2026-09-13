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
	"math"
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
	// Optional metrics, present only when the request switched them on.
	DurationMS   *float64 `json:"duration_ms"`
	RMSAmplitude *float64 `json:"rms_amplitude"`
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

type pairDTO struct {
	LeftPulseIndex  int `json:"left_pulse_index"`
	RightPulseIndex int `json:"right_pulse_index"`
	TimeDifference  int `json:"time_difference_samples"`
}

type correlateDTO struct {
	SampleRate       float64    `json:"sample_rate"`
	ToleranceSamples int        `json:"tolerance_samples"`
	Left             analyzeDTO `json:"left"`
	Right            analyzeDTO `json:"right"`
	Pairs            []pairDTO  `json:"pairs"`
	LeftUnpaired     []int      `json:"left_unpaired"`
	RightUnpaired    []int      `json:"right_unpaired"`
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

	// --- optional per-pulse metrics (include_metrics) ---
	run("metrics: normal pulse carries exact duration and rms", c.scenarioMetricsPrecise)
	run("metrics: huge finite amplitudes stay finite", c.scenarioMetricsHugeFinite)
	run("metrics: non-boolean switch is located at include_metrics", c.scenarioMetricsSwitchType)
	run("metrics: switch off and dual-channel keep the legacy contract", c.scenarioMetricsCompatibility)

	// --- dual-channel correlation ---
	run("correlation: two coincident pulses pair with zero difference", c.scenarioCorrelatePrecise)
	run("correlation: tolerance boundary is inclusive and zero is exact", c.scenarioCorrelateToleranceBoundary)
	run("correlation: unequal pulse counts leave the extra pulse unpaired", c.scenarioCorrelateUnequalCounts)
	run("correlation: candidate chosen by smallest time difference", c.scenarioCorrelateTimeDifferenceWins)
	run("correlation: an occupied pulse preempts later candidates", c.scenarioCorrelatePreemption)
	run("correlation: evidence is ordered by ascending left peak index", c.scenarioCorrelateEvidenceOrder)
	run("correlation: one undecidable side forbids all evidence", c.scenarioCorrelateOneSideUndecidable)
	run("correlation: one zero-baseline side forbids all evidence", c.scenarioCorrelateOneSideBaselineZero)
	run("correlation: per-side excluded ranges shield each analysis", c.scenarioCorrelateExcludedRanges)
	run("correlation: embedded channels match the analyze response fields", c.scenarioCorrelatePreservesAnalyze)
	run("correlation: same request yields byte-identical responses", c.scenarioCorrelateDeterminism)
	run("correlation: missing tolerance is located at tolerance_samples", c.scenarioCorrelateToleranceMissing)
	run("correlation: non-integer tolerance is located at tolerance_samples", c.scenarioCorrelateToleranceType)
	run("correlation: tolerance out of [0,100] is located at tolerance_samples", c.scenarioCorrelateToleranceRange)
	run("correlation: NaN tolerance is located at tolerance_samples", c.scenarioCorrelateToleranceNaN)
	run("correlation: missing left side is located at left", c.scenarioCorrelateSideMissing)
	run("correlation: null right side is located at right", c.scenarioCorrelateSideNull)
	run("correlation: scalar side is a type error at the side", c.scenarioCorrelateSideScalar)
	run("correlation: per-side errors keep left/right prefixes and indices", c.scenarioCorrelatePerSideErrors)
	run("correlation: differing sample rates are located at right.sample_rate", c.scenarioCorrelateRateMismatch)
	run("correlation: invalid request returns no partial association", c.scenarioCorrelateNoPartialOnError)
	run("correlation: unknown top-level field is rejected and named", c.scenarioCorrelateUnknownField)

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

// --- include_metrics scenarios -------------------------------------------------

func (c *client) scenarioMetricsPrecise(a *assert.Assertions) {
	// 4-sample pulse [5,8] with values 13, 25, 13, 13 at 16 kHz:
	// duration = 4/16000 s = 0.25 ms; RMS covers exactly the closed
	// interval: sqrt((13² + 25² + 13² + 13²)/4) = sqrt(283).
	set := map[int]float64{}
	setRange(set, 5, 8, 13)
	set[6] = 25
	resp, body := c.analyzeMetrics(a, 16000, flatSamples(64, 2, set))
	a.True(resp.Decidable, "body: %s", body)
	a.Equal(2.0, resp.Baseline)
	if a.Len(resp.Pulses, 1) {
		p := resp.Pulses[0]
		a.Equal(5, p.Start)
		a.Equal(8, p.End)
		a.Equal(6, p.PeakIndex)
		a.Equal(25.0, p.Peak)
		if a.NotNil(p.DurationMS, "body: %s", body) {
			a.Equal(0.25, *p.DurationMS)
		}
		if a.NotNil(p.RMSAmplitude) {
			a.InDelta(math.Sqrt(283), *p.RMSAmplitude, 1e-12)
		}
	}
}

func (c *client) scenarioMetricsHugeFinite(a *assert.Assertions) {
	// Four samples at 1e308: a naive sum of squares overflows to +Inf; the
	// scaled sum-of-squares must return exactly 1e308, a finite value.
	set := map[int]float64{}
	setRange(set, 5, 8, 1e308)
	resp, body := c.analyzeMetrics(a, 16000, flatSamples(64, 2, set))
	a.True(resp.Decidable, "body: %s", body)
	if a.Len(resp.Pulses, 1) {
		p := resp.Pulses[0]
		a.Equal(1e308, p.Peak)
		a.Equal("severe", p.Severity)
		if a.NotNil(p.RMSAmplitude, "body: %s", body) {
			a.False(math.IsInf(*p.RMSAmplitude, 0), "rms must stay finite")
			a.False(math.IsNaN(*p.RMSAmplitude), "rms must not be NaN")
			a.Equal(1e308, *p.RMSAmplitude)
		}
		if a.NotNil(p.DurationMS) {
			a.Equal(0.25, *p.DurationMS)
		}
	}
}

func (c *client) scenarioMetricsSwitchType(a *assert.Assertions) {
	// The switch accepts only a JSON boolean; every other token is a type
	// error located at include_metrics with no sample index.
	for _, token := range []string{`"true"`, "1", "0", "1.5", "[]", "{}", "NaN", "Infinity"} {
		raw := fmt.Sprintf(`{"sample_rate":16000,"amplitudes":[%s],"include_metrics":%s}`,
			zerosCSV(64), token)
		code, body := c.postRaw(a, raw)
		a.Equal(http.StatusBadRequest, code, "token %s body: %s", token, body)
		e := decodeError(a, body)
		a.Equal("validation_failed", e.Error, token)
		a.Equal("include_metrics", e.Field, token)
		a.Equal("type", e.Constraint, token)
		a.Nil(e.Index, token)
	}
}

func (c *client) scenarioMetricsCompatibility(a *assert.Assertions) {
	set := map[int]float64{}
	setRange(set, 5, 8, 13)
	set[6] = 25
	samplesJSON, err := json.Marshal(flatSamples(64, 2, set))
	if !a.NoError(err) {
		return
	}

	// Omitted, false and null switches are byte-identical to one another
	// and carry no metric fields: existing clients see the legacy contract.
	baseline := ""
	for i, tail := range []string{``, `,"include_metrics":false`, `,"include_metrics":null`} {
		code, body := c.postRaw(a, fmt.Sprintf(
			`{"sample_rate":16000,"amplitudes":%s%s}`, samplesJSON, tail))
		a.Equal(http.StatusOK, code, "body: %s", body)
		a.NotContains(string(body), "duration_ms", "tail %s", tail)
		a.NotContains(string(body), "rms_amplitude", "tail %s", tail)
		if i == 0 {
			baseline = string(body)
		} else {
			a.Equal(baseline, string(body), "tail %s must not change the response", tail)
		}
	}

	// The dual-channel review never carries metric fields either, in
	// neither side's embedded analysis.
	resp, body := c.correlateOK(a, 5,
		channel(16000, dualBurstSamples()), channel(16000, dualBurstSamples()))
	a.True(resp.Left.Decidable, "body: %s", body)
	a.NotEmpty(resp.Pairs)
	a.NotContains(string(body), "duration_ms")
	a.NotContains(string(body), "rms_amplitude")
}

// --- dual-channel correlation scenarios --------------------------------------

// channelReq is one side of a correlation request, identical in shape to
// the single-channel analyze contract.
type channelReq struct {
	SampleRate     float64    `json:"sample_rate"`
	Amplitudes     []float64  `json:"amplitudes"`
	ExcludedRanges []rangeDTO `json:"excluded_ranges,omitempty"`
}

func channel(rate float64, samples []float64, ranges ...rangeDTO) channelReq {
	return channelReq{SampleRate: rate, Amplitudes: samples, ExcludedRanges: ranges}
}

// dualBurstSamples is the two-pulse fixture: severe burst [5,8] peak 25 at
// index 6 and general burst [40,43] peak 13 at index 40 on baseline 2.
func dualBurstSamples() []float64 {
	set := map[int]float64{}
	setRange(set, 5, 8, 13)
	set[6] = 25
	setRange(set, 40, 43, 13)
	return flatSamples(64, 2, set)
}

func (c *client) scenarioCorrelatePrecise(a *assert.Assertions) {
	resp, body := c.correlateOK(a, 5, channel(16000, dualBurstSamples()), channel(16000, dualBurstSamples()))
	a.True(resp.Left.Decidable, "body: %s", body)
	a.True(resp.Right.Decidable)
	if a.Len(resp.Pairs, 2) {
		a.Equal(pairDTO{0, 0, 0}, resp.Pairs[0])
		a.Equal(pairDTO{1, 1, 0}, resp.Pairs[1])
	}
	a.Equal([]int{}, resp.LeftUnpaired)
	a.Equal([]int{}, resp.RightUnpaired)
}

func (c *client) scenarioCorrelateToleranceBoundary(a *assert.Assertions) {
	// Left peak at 6, right peak one sample later at 7.
	leftSet := map[int]float64{}
	setRange(leftSet, 5, 8, 13)
	leftSet[6] = 25
	rightSet := map[int]float64{}
	setRange(rightSet, 5, 8, 13)
	rightSet[7] = 25
	left, right := flatSamples(64, 2, leftSet), flatSamples(64, 2, rightSet)

	exact, body := c.correlateOK(a, 0, channel(16000, left), channel(16000, right))
	a.Empty(exact.Pairs, "zero tolerance must not bridge different peak times: %s", body)
	a.Equal([]int{0}, exact.LeftUnpaired)
	a.Equal([]int{0}, exact.RightUnpaired)

	within, _ := c.correlateOK(a, 1, channel(16000, left), channel(16000, right))
	if a.Len(within.Pairs, 1) {
		a.Equal(pairDTO{0, 0, 1}, within.Pairs[0])
	}
	a.Equal([]int{}, within.LeftUnpaired)
	a.Equal([]int{}, within.RightUnpaired)
}

func (c *client) scenarioCorrelateUnequalCounts(a *assert.Assertions) {
	// Left keeps both bursts; the right side only records the later one.
	leftSet := map[int]float64{}
	setRange(leftSet, 5, 8, 13)
	setRange(leftSet, 40, 43, 13)
	rightSet := map[int]float64{}
	setRange(rightSet, 40, 43, 13)
	left := flatSamples(64, 2, leftSet)
	right := flatSamples(64, 2, rightSet)

	resp, body := c.correlateOK(a, 3, channel(16000, left), channel(16000, right))
	if a.Len(resp.Pairs, 1, "body: %s", body) {
		a.Equal(pairDTO{1, 0, 0}, resp.Pairs[0],
			"the right pulse pairs with the coincident later left pulse")
	}
	a.Equal([]int{0}, resp.LeftUnpaired)
	a.Equal([]int{}, resp.RightUnpaired)
}

func (c *client) scenarioCorrelateTimeDifferenceWins(a *assert.Assertions) {
	// One left peak at 12; right peaks at 4 (difference 8) and 10
	// (difference 2). Both candidates fall inside tolerance 8, but the
	// smaller time difference must win even though R0 is enumerated first.
	leftSet := map[int]float64{}
	setRange(leftSet, 10, 13, 13)
	leftSet[12] = 25
	rightSet := map[int]float64{}
	setRange(rightSet, 1, 4, 13) // first right pulse, peak at 4
	rightSet[4] = 25
	setRange(rightSet, 9, 12, 13) // second right pulse, peak at 10
	rightSet[10] = 25
	left := flatSamples(64, 2, leftSet)
	right := flatSamples(64, 2, rightSet)

	resp, body := c.correlateOK(a, 8, channel(16000, left), channel(16000, right))
	if a.Len(resp.Pairs, 1, "body: %s", body) {
		a.Equal(pairDTO{0, 1, 2}, resp.Pairs[0])
	}
	a.Equal([]int{}, resp.LeftUnpaired)
	a.Equal([]int{0}, resp.RightUnpaired, "the difference-8 candidate is left unpaired")
}

func (c *client) scenarioCorrelatePreemption(a *assert.Assertions) {
	// Left peaks at 10 and 20; the single right pulse peaks at 15. Both
	// candidates differ by exactly the tolerance 5; the smaller left index
	// claims the right pulse and the later candidate is skipped.
	leftSet := map[int]float64{}
	setRange(leftSet, 8, 11, 13)
	leftSet[10] = 25
	setRange(leftSet, 18, 21, 13)
	leftSet[20] = 25
	rightSet := map[int]float64{}
	setRange(rightSet, 13, 16, 13)
	rightSet[15] = 25
	left := flatSamples(64, 2, leftSet)
	right := flatSamples(64, 2, rightSet)

	resp, body := c.correlateOK(a, 5, channel(16000, left), channel(16000, right))
	if a.Len(resp.Pairs, 1, "body: %s", body) {
		a.Equal(pairDTO{0, 0, 5}, resp.Pairs[0])
	}
	a.Equal([]int{1}, resp.LeftUnpaired, "the preempted left pulse stays unpaired")
	a.Equal([]int{}, resp.RightUnpaired)
}

func (c *client) scenarioCorrelateEvidenceOrder(a *assert.Assertions) {
	// Greedy selection reaches the difference-1 pair before the
	// difference-5 pair; the response order is fixed by left peak index.
	leftSet := map[int]float64{}
	setRange(leftSet, 8, 11, 13)
	leftSet[10] = 25
	setRange(leftSet, 18, 21, 13)
	leftSet[20] = 25
	rightSet := map[int]float64{}
	setRange(rightSet, 13, 16, 13) // peak at 15
	rightSet[15] = 25
	setRange(rightSet, 21, 24, 13) // peak at 21
	rightSet[21] = 25
	left := flatSamples(64, 2, leftSet)
	right := flatSamples(64, 2, rightSet)

	resp, body := c.correlateOK(a, 5, channel(16000, left), channel(16000, right))
	if a.Len(resp.Pairs, 2, "body: %s", body) {
		a.Equal(pairDTO{0, 0, 5}, resp.Pairs[0])
		a.Equal(pairDTO{1, 1, 1}, resp.Pairs[1],
			"evidence is ordered by ascending left peak index, not selection order")
	}
	a.Equal([]int{}, resp.LeftUnpaired)
	a.Equal([]int{}, resp.RightUnpaired)
}

func (c *client) scenarioCorrelateOneSideUndecidable(a *assert.Assertions) {
	s := seamSamples()
	shield := []rangeDTO{{Start: 10, End: 74}}

	// Left shielded (decidable, one pulse), right unshielded (undecidable).
	resp, body := c.correlateOK(a, 3,
		channel(16000, s, shield...), channel(16000, s))
	a.True(resp.Left.Decidable, "body: %s", body)
	a.False(resp.Right.Decidable)
	a.Equal("no_pulses", resp.Right.Reason)
	a.Empty(resp.Pairs, "an undecidable side forbids any evidence")
	a.Equal([]int{0}, resp.LeftUnpaired)
	a.Equal([]int{}, resp.RightUnpaired)
	if a.Len(resp.Left.ExcludedRanges, 1) {
		a.Equal(shield[0], resp.Left.ExcludedRanges[0])
	}
}

func (c *client) scenarioCorrelateOneSideBaselineZero(a *assert.Assertions) {
	leftSet := map[int]float64{}
	setRange(leftSet, 5, 8, 13)
	left := flatSamples(64, 2, leftSet)
	zeros := make([]float64, 64)

	resp, body := c.correlateOK(a, 3, channel(16000, left), channel(16000, zeros))
	a.True(resp.Left.Decidable, "body: %s", body)
	a.False(resp.Right.Decidable)
	a.Equal("baseline_zero", resp.Right.Reason)
	a.Empty(resp.Pairs)
	a.Equal([]int{0}, resp.LeftUnpaired)
	a.Equal([]int{}, resp.RightUnpaired)
}

func (c *client) scenarioCorrelateExcludedRanges(a *assert.Assertions) {
	s := seamSamples()
	shield := []rangeDTO{{Start: 10, End: 74}}

	// Unshielded sides are undecidable because the seam dominates the
	// median; shielding both sides reveals the burst at [80,83].
	full, _ := c.correlateOK(a, 3, channel(16000, s), channel(16000, s))
	a.False(full.Left.Decidable)
	a.False(full.Right.Decidable)
	a.Empty(full.Pairs)

	resp, body := c.correlateOK(a, 3,
		channel(16000, s, shield...), channel(16000, s, shield...))
	a.True(resp.Left.Decidable, "body: %s", body)
	a.True(resp.Right.Decidable)
	if a.Len(resp.Pairs, 1) {
		a.Equal(pairDTO{0, 0, 0}, resp.Pairs[0])
	}
	a.Equal([]int{}, resp.LeftUnpaired)
	a.Equal([]int{}, resp.RightUnpaired)
	a.Equal(shield, resp.Left.ExcludedRanges)
	a.Equal(shield, resp.Right.ExcludedRanges)
}

func (c *client) scenarioCorrelatePreservesAnalyze(a *assert.Assertions) {
	s := seamSamples()
	shield := []rangeDTO{{Start: 10, End: 74}}

	// The same channel posted standalone and embedded in a correlation
	// must yield field-for-field identical analysis.
	standalone, standaloneBody := c.analyzeRanges(a, 16000, s, shield)
	resp, body := c.correlateOK(a, 3,
		channel(16000, s, shield...), channel(16000, s, shield...))
	a.Equal(standalone, resp.Right,
		"embedded channel differs from /analyze:\n%s\n%s", standaloneBody, body)
	a.Equal(standalone, resp.Left)

	// The original single-channel endpoint is untouched.
	again, _ := c.analyzeRanges(a, 16000, s, shield)
	a.Equal(standalone, again)
}

func (c *client) scenarioCorrelateDeterminism(a *assert.Assertions) {
	payload, err := json.Marshal(struct {
		ToleranceSamples int        `json:"tolerance_samples"`
		Left             channelReq `json:"left"`
		Right            channelReq `json:"right"`
	}{5, channel(16000, dualBurstSamples()), channel(16000, dualBurstSamples())})
	if !a.NoError(err) {
		return
	}
	code, first := c.postCorrelateRaw(a, string(payload))
	a.Equal(http.StatusOK, code, "body: %s", first)
	for i := 0; i < 3; i++ {
		_, again := c.postCorrelateRaw(a, string(payload))
		a.Equal(string(first), string(again), "response %d differs", i+2)
	}
}

func (c *client) scenarioCorrelateToleranceMissing(a *assert.Assertions) {
	raw := fmt.Sprintf(`{"left":%s,"right":%s}`, zerosSide(64), zerosSide(64))
	code, body := c.postCorrelateRaw(a, raw)
	a.Equal(http.StatusBadRequest, code, "body: %s", body)
	e := decodeError(a, body)
	a.Equal("tolerance_samples", e.Field)
	a.Equal("required", e.Constraint)
}

func (c *client) scenarioCorrelateToleranceType(a *assert.Assertions) {
	for _, token := range []string{"null", `"3"`, "1.5", "true"} {
		raw := fmt.Sprintf(`{"tolerance_samples":%s,"left":%s,"right":%s}`,
			token, zerosSide(64), zerosSide(64))
		code, body := c.postCorrelateRaw(a, raw)
		a.Equal(http.StatusBadRequest, code, "token %s body: %s", token, body)
		e := decodeError(a, body)
		a.Equal("tolerance_samples", e.Field, token)
		a.Equal("type", e.Constraint, token)
	}
}

func (c *client) scenarioCorrelateToleranceRange(a *assert.Assertions) {
	for _, token := range []string{"-1", "101"} {
		raw := fmt.Sprintf(`{"tolerance_samples":%s,"left":%s,"right":%s}`,
			token, zerosSide(64), zerosSide(64))
		code, body := c.postCorrelateRaw(a, raw)
		a.Equal(http.StatusBadRequest, code, "body: %s", body)
		e := decodeError(a, body)
		a.Equal("tolerance_samples", e.Field)
		a.Equal("range", e.Constraint, token)
	}

	// Both boundaries are accepted.
	for _, tol := range []int{0, 100} {
		payload, _ := json.Marshal(struct {
			ToleranceSamples int        `json:"tolerance_samples"`
			Left             channelReq `json:"left"`
			Right            channelReq `json:"right"`
		}{tol, channel(16000, dualBurstSamples()), channel(16000, dualBurstSamples())})
		code, body := c.postCorrelateRaw(a, string(payload))
		a.Equal(http.StatusOK, code, "tolerance %d body: %s", tol, body)
	}
}

func (c *client) scenarioCorrelateToleranceNaN(a *assert.Assertions) {
	raw := fmt.Sprintf(`{"tolerance_samples":NaN,"left":%s,"right":%s}`,
		zerosSide(64), zerosSide(64))
	code, body := c.postCorrelateRaw(a, raw)
	a.Equal(http.StatusBadRequest, code, "body: %s", body)
	e := decodeError(a, body)
	a.Equal("tolerance_samples", e.Field)
	a.Equal("type", e.Constraint)
}

func (c *client) scenarioCorrelateSideMissing(a *assert.Assertions) {
	raw := fmt.Sprintf(`{"tolerance_samples":1,"right":%s}`, zerosSide(64))
	code, body := c.postCorrelateRaw(a, raw)
	a.Equal(http.StatusBadRequest, code, "body: %s", body)
	e := decodeError(a, body)
	a.Equal("left", e.Field)
	a.Equal("required", e.Constraint)
}

func (c *client) scenarioCorrelateSideNull(a *assert.Assertions) {
	raw := fmt.Sprintf(`{"tolerance_samples":1,"left":%s,"right":null}`, zerosSide(64))
	code, body := c.postCorrelateRaw(a, raw)
	a.Equal(http.StatusBadRequest, code, "body: %s", body)
	e := decodeError(a, body)
	a.Equal("right", e.Field)
	a.Equal("type", e.Constraint)
}

func (c *client) scenarioCorrelateSideScalar(a *assert.Assertions) {
	raw := fmt.Sprintf(`{"tolerance_samples":1,"left":123,"right":%s}`, zerosSide(64))
	code, body := c.postCorrelateRaw(a, raw)
	a.Equal(http.StatusBadRequest, code, "body: %s", body)
	e := decodeError(a, body)
	a.Equal("left", e.Field)
	a.Equal("type", e.Constraint)
}

func (c *client) scenarioCorrelatePerSideErrors(a *assert.Assertions) {
	good := zerosSide(64)
	for _, tc := range []struct {
		name       string
		raw        string
		field      string
		index      *int
		constraint string
	}{
		{
			name:       "left sample rate out of range",
			raw:        fmt.Sprintf(`{"tolerance_samples":1,"left":{"sample_rate":999,"amplitudes":[%s]},"right":%s}`, zerosCSV(64), good),
			field:      "left.sample_rate",
			constraint: "range",
		},
		{
			name:       "right sample rate wrong type",
			raw:        fmt.Sprintf(`{"tolerance_samples":1,"left":%s,"right":{"sample_rate":"16000","amplitudes":[%s]}}`, good, zerosCSV(64)),
			field:      "right.sample_rate",
			constraint: "type",
		},
		{
			name:       "left amplitudes too short",
			raw:        fmt.Sprintf(`{"tolerance_samples":1,"left":{"sample_rate":16000,"amplitudes":[%s]},"right":%s}`, zerosCSV(63), good),
			field:      "left.amplitudes",
			constraint: "min_length",
		},
		{
			name: "right bad amplitude keeps sample index",
			raw: func() string {
				values := make([]string, 64)
				for i := range values {
					values[i] = "0"
				}
				values[7] = `"x"`
				return fmt.Sprintf(`{"tolerance_samples":1,"left":%s,"right":{"sample_rate":16000,"amplitudes":[%s]}}`,
					good, strings.Join(values, ","))
			}(),
			field:      "right.amplitudes",
			index:      intPtrVerify(7),
			constraint: "type",
		},
		{
			name:       "left inverted excluded range keeps element index",
			raw:        fmt.Sprintf(`{"tolerance_samples":1,"left":{"sample_rate":16000,"amplitudes":[%s],"excluded_ranges":[{"start":10,"end":9}]},"right":%s}`, zerosCSV(64), good),
			field:      "left.excluded_ranges",
			index:      intPtrVerify(0),
			constraint: "order",
		},
		{
			name: "NaN element keeps side prefix and sample index",
			raw: func() string {
				values := make([]string, 64)
				for i := range values {
					values[i] = "0"
				}
				values[3] = "NaN"
				return fmt.Sprintf(`{"tolerance_samples":1,"left":{"sample_rate":16000,"amplitudes":[%s]},"right":%s}`,
					strings.Join(values, ","), good)
			}(),
			field:      "left.amplitudes",
			index:      intPtrVerify(3),
			constraint: "finite",
		},
	} {
		code, body := c.postCorrelateRaw(a, tc.raw)
		if !a.Equal(http.StatusBadRequest, code, "%s body: %s", tc.name, body) {
			continue
		}
		e := decodeError(a, body)
		a.Equal(tc.field, e.Field, tc.name)
		a.Equal(tc.constraint, e.Constraint, tc.name)
		if tc.index != nil {
			if a.NotNil(e.Index, tc.name) {
				a.Equal(*tc.index, *e.Index, tc.name)
			}
		}
	}
}

func (c *client) scenarioCorrelateRateMismatch(a *assert.Assertions) {
	raw := fmt.Sprintf(`{"tolerance_samples":1,"left":%s,"right":%s}`,
		sideWithRate(16000, 64), sideWithRate(8000, 64))
	code, body := c.postCorrelateRaw(a, raw)
	a.Equal(http.StatusBadRequest, code, "body: %s", body)
	e := decodeError(a, body)
	a.Equal("right.sample_rate", e.Field)
	a.Equal("sample_rate_mismatch", e.Constraint)
}

func (c *client) scenarioCorrelateNoPartialOnError(a *assert.Assertions) {
	// An invalid request must return only the error envelope: no
	// association fields may leak.
	raw := fmt.Sprintf(`{"tolerance_samples":-5,"left":%s,"right":%s}`,
		zerosSide(64), zerosSide(64))
	code, body := c.postCorrelateRaw(a, raw)
	a.Equal(http.StatusBadRequest, code, "body: %s", body)
	var envelope map[string]any
	if !a.NoError(json.Unmarshal(body, &envelope)) {
		return
	}
	a.NotContains(envelope, "pairs")
	a.NotContains(envelope, "left")
	a.NotContains(envelope, "right")
	a.NotContains(envelope, "left_unpaired")
	a.Equal("tolerance_samples", envelope["field"])
}

func (c *client) scenarioCorrelateUnknownField(a *assert.Assertions) {
	raw := fmt.Sprintf(`{"tolerance_samples":1,"left":%s,"right":%s,"extra":1}`,
		zerosSide(64), zerosSide(64))
	code, body := c.postCorrelateRaw(a, raw)
	a.Equal(http.StatusBadRequest, code, "body: %s", body)
	e := decodeError(a, body)
	a.Equal("extra", e.Field)
	a.Equal("unknown", e.Constraint)
}

func zerosSide(n int) string {
	return fmt.Sprintf(`{"sample_rate":16000,"amplitudes":[%s]}`, zerosCSV(n))
}

func sideWithRate(rate float64, n int) string {
	return fmt.Sprintf(`{"sample_rate":%v,"amplitudes":[%s]}`, rate, zerosCSV(n))
}

func intPtrVerify(i int) *int { return &i }

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
	return c.postPath(a, "/api/v1/pulses/analyze", raw)
}

func (c *client) postCorrelateRaw(a *assert.Assertions, raw string) (int, []byte) {
	return c.postPath(a, "/api/v1/pulses/correlate", raw)
}

func (c *client) postPath(a *assert.Assertions, path, raw string) (int, []byte) {
	resp, err := c.http.Post(c.baseURL+path,
		"application/json", bytes.NewBufferString(raw))
	if !a.NoError(err) {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// correlateOK posts a dual-channel review and expects 200.
func (c *client) correlateOK(a *assert.Assertions, tolerance int, left, right channelReq) (correlateDTO, []byte) {
	payload, err := json.Marshal(struct {
		ToleranceSamples int        `json:"tolerance_samples"`
		Left             channelReq `json:"left"`
		Right            channelReq `json:"right"`
	}{tolerance, left, right})
	if !a.NoError(err) {
		return correlateDTO{}, nil
	}
	code, body := c.postCorrelateRaw(a, string(payload))
	a.Equal(http.StatusOK, code, "body: %s", body)

	var resp correlateDTO
	if !a.NoError(json.Unmarshal(body, &resp)) {
		return correlateDTO{}, body
	}
	a.Equal(left.SampleRate, resp.SampleRate)
	a.NotNil(resp.Pairs)
	a.NotNil(resp.LeftUnpaired)
	a.NotNil(resp.RightUnpaired)
	a.NotNil(resp.Left.Pulses)
	a.NotNil(resp.Right.Pulses)
	return resp, body
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

// analyzeMetrics posts a request with include_metrics switched on and
// expects 200.
func (c *client) analyzeMetrics(a *assert.Assertions, rate float64, samples []float64) (analyzeDTO, []byte) {
	payload, err := json.Marshal(struct {
		SampleRate     float64   `json:"sample_rate"`
		Amplitudes     []float64 `json:"amplitudes"`
		IncludeMetrics bool      `json:"include_metrics"`
	}{rate, samples, true})
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
