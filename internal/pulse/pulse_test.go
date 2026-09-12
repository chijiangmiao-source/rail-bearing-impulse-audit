package pulse

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fill(n, base float64, set map[int]float64) []float64 {
	s := make([]float64, int(n))
	for i := range s {
		s[i] = base
		if v, ok := set[i]; ok {
			s[i] = v
		}
	}
	return s
}

func setRange(m map[int]float64, from, to int, v float64) {
	for i := from; i <= to; i++ {
		m[i] = v
	}
}

func TestMedianAbs_OddAndEven(t *testing.T) {
	assert.Equal(t, 3.0, medianAbs([]float64{1, -3, 5}))
	// Even: arithmetic mean of the two middle absolute values.
	assert.Equal(t, 2.5, medianAbs([]float64{1, 2, 3, 10}))
	// Negation must not change the baseline.
	assert.Equal(t, medianAbs([]float64{1, -2, 3, 10}),
		medianAbs([]float64{-1, 2, -3, -10}))
}

func TestAnalyze_BasicTwoPulses(t *testing.T) {
	set := map[int]float64{}
	setRange(set, 5, 8, 13)
	set[6] = 25
	setRange(set, 40, 43, 13)

	r := Analyze(fill(64, 2, set))
	require.True(t, r.Decidable)
	assert.Equal(t, 2.0, r.Baseline)
	require.Len(t, r.Pulses, 2)

	p0 := r.Pulses[0]
	assert.Equal(t, Pulse{
		Start: 5, End: 8, PeakIndex: 6, Peak: 25,
		Severity: SeveritySevere, Baseline: 2,
	}, p0)

	p1 := r.Pulses[1]
	assert.Equal(t, 40, p1.Start)
	assert.Equal(t, 43, p1.End)
	assert.Equal(t, 40, p1.PeakIndex) // tied peak: earliest
	assert.Equal(t, 13.0, p1.Peak)
	assert.Equal(t, SeverityGeneral, p1.Severity)
}

func TestAnalyze_MergeGapBoundary(t *testing.T) {
	set := map[int]float64{}
	setRange(set, 10, 11, 20) // run A
	setRange(set, 15, 16, 20) // gap of 3 -> merge
	setRange(set, 21, 24, 20) // gap of 4 from B -> separate

	r := Analyze(fill(64, 2, set))
	require.True(t, r.Decidable)
	require.Len(t, r.Pulses, 2)
	assert.Equal(t, 10, r.Pulses[0].Start)
	assert.Equal(t, 16, r.Pulses[0].End)
	assert.Equal(t, 7, r.Pulses[0].End-r.Pulses[0].Start+1)
	assert.Equal(t, 21, r.Pulses[1].Start)
	assert.Equal(t, 24, r.Pulses[1].End)
}

func TestAnalyze_ThresholdIsStrict(t *testing.T) {
	// 6x baseline exactly is not above threshold; no candidate.
	set := map[int]float64{}
	setRange(set, 30, 35, 12)
	r := Analyze(fill(64, 2, set))
	assert.False(t, r.Decidable)
	assert.Equal(t, ReasonNoPulses, r.Reason)
	assert.Empty(t, r.Pulses)
}

func TestAnalyze_SeverityIsStrict(t *testing.T) {
	set := map[int]float64{}
	setRange(set, 2, 5, 24) // peak == 12 * baseline -> general
	r := Analyze(fill(64, 2, set))
	require.Len(t, r.Pulses, 1)
	assert.Equal(t, SeverityGeneral, r.Pulses[0].Severity)

	for i := 2; i <= 5; i++ {
		set[i] = math.Nextafter(24, 25)
	}
	r = Analyze(fill(64, 2, set))
	require.Len(t, r.Pulses, 1)
	assert.Equal(t, SeveritySevere, r.Pulses[0].Severity)
}

func TestAnalyze_LengthFilter(t *testing.T) {
	set := map[int]float64{}
	setRange(set, 5, 7, 30) // 3 samples, isolated -> discarded
	r := Analyze(fill(64, 2, set))
	assert.False(t, r.Decidable)
	assert.Equal(t, ReasonNoPulses, r.Reason)
}

func TestAnalyze_ZeroBaseline(t *testing.T) {
	r := Analyze(make([]float64, 64))
	assert.False(t, r.Decidable)
	assert.Equal(t, ReasonBaselineZero, r.Reason)
	assert.Equal(t, 0.0, r.Baseline)
	assert.Empty(t, r.Pulses)
}

func TestAnalyze_ZeroBaselineWithSpikeFabricatesNothing(t *testing.T) {
	s := make([]float64, 64)
	s[10] = 99
	r := Analyze(s)
	assert.False(t, r.Decidable)
	assert.Equal(t, ReasonBaselineZero, r.Reason)
	assert.Empty(t, r.Pulses)
}

func TestAnalyze_PeakTieIsEarliest(t *testing.T) {
	set := map[int]float64{}
	setRange(set, 8, 12, 20)
	r := Analyze(fill(64, 2, set))
	require.Len(t, r.Pulses, 1)
	assert.Equal(t, 8, r.Pulses[0].PeakIndex)
}

func TestAnalyze_Deterministic(t *testing.T) {
	set := map[int]float64{}
	setRange(set, 5, 8, 13)
	set[6] = 25
	setRange(set, 40, 43, 13)
	s := fill(64, 2, set)

	first := Analyze(s)
	for i := 0; i < 5; i++ {
		assert.Equal(t, first, Analyze(s))
	}
}

func TestAnalyze_NegativePeaksUseMagnitude(t *testing.T) {
	set := map[int]float64{}
	for i := 4; i <= 7; i++ {
		set[i] = -25
	}
	r := Analyze(fill(64, 2, set))
	require.Len(t, r.Pulses, 1)
	assert.Equal(t, 25.0, r.Pulses[0].Peak)
	assert.Equal(t, 4, r.Pulses[0].PeakIndex)
	assert.Equal(t, SeveritySevere, r.Pulses[0].Severity)
}

func TestMergeRuns(t *testing.T) {
	// Empty and single-run cases.
	assert.Nil(t, mergeRuns(nil))
	assert.Equal(t, []run{{0, 1}}, mergeRuns([]run{{0, 1}}))
}
