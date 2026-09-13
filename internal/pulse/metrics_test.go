package pulse

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnalyzeWithMetrics_NormalPulseExactMetrics(t *testing.T) {
	// 4-sample pulse [5,8] with values 13, 25, 13, 13 at 16 kHz:
	// duration = 4/16000 s = 0.25 ms and the RMS covers exactly the closed
	// interval: sqrt((13² + 25² + 13² + 13²)/4) = sqrt(283).
	set := map[int]float64{}
	setRange(set, 5, 8, 13)
	set[6] = 25
	s := fill(64, 2, set)

	r := AnalyzeWithMetrics(s, 16000)
	require.True(t, r.Decidable)
	require.Len(t, r.Pulses, 1)
	p := r.Pulses[0]
	require.NotNil(t, p.DurationMS)
	require.NotNil(t, p.RMSAmplitude)
	assert.Equal(t, 0.25, *p.DurationMS)
	assert.InDelta(t, math.Sqrt(283), *p.RMSAmplitude, 1e-12)

	// Detection fields are identical to the plain analysis.
	plain := Analyze(s)
	require.Len(t, plain.Pulses, 1)
	assert.Equal(t, plain.Pulses[0].Start, p.Start)
	assert.Equal(t, plain.Pulses[0].End, p.End)
	assert.Equal(t, plain.Pulses[0].PeakIndex, p.PeakIndex)
	assert.Equal(t, plain.Pulses[0].Peak, p.Peak)
	assert.Equal(t, plain.Pulses[0].Severity, p.Severity)
	assert.Equal(t, plain.Pulses[0].Baseline, p.Baseline)
}

func TestAnalyzeWithMetrics_HugeFiniteAmplitudeStaysFinite(t *testing.T) {
	// Four samples at 1e308: a naive sum of squares overflows to +Inf; the
	// scaled sum-of-squares returns exactly 1e308.
	set := map[int]float64{}
	setRange(set, 5, 8, 1e308)
	r := AnalyzeWithMetrics(fill(64, 2, set), 16000)
	require.True(t, r.Decidable)
	require.Len(t, r.Pulses, 1)
	require.NotNil(t, r.Pulses[0].RMSAmplitude)
	assert.False(t, math.IsInf(*r.Pulses[0].RMSAmplitude, 0))
	assert.Equal(t, 1e308, *r.Pulses[0].RMSAmplitude)
	require.NotNil(t, r.Pulses[0].DurationMS)
	assert.Equal(t, 0.25, *r.Pulses[0].DurationMS)
}

func TestAnalyzeWithMetrics_MixedHugeMagnitudesStayFinite(t *testing.T) {
	// Mixed magnitudes near the float64 ceiling: the RMS must remain a
	// finite value between the smallest and the largest magnitude.
	set := map[int]float64{
		5: math.MaxFloat64,
		6: math.MaxFloat64 / 2,
		7: math.MaxFloat64,
		8: math.MaxFloat64 / 4,
	}
	r := AnalyzeWithMetrics(fill(64, 2, set), 16000)
	require.True(t, r.Decidable)
	require.Len(t, r.Pulses, 1)
	require.NotNil(t, r.Pulses[0].RMSAmplitude)
	rms := *r.Pulses[0].RMSAmplitude
	assert.False(t, math.IsInf(rms, 0))
	assert.False(t, math.IsNaN(rms))
	assert.Greater(t, rms, math.MaxFloat64/4)
	assert.LessOrEqual(t, rms, math.MaxFloat64)
}

func TestAnalyzeWithMetrics_UndecidableFabricatesNoMetrics(t *testing.T) {
	// Zero baseline: the undecidable result is unchanged and empty.
	zero := AnalyzeWithMetrics(make([]float64, 64), 16000)
	assert.False(t, zero.Decidable)
	assert.Equal(t, ReasonBaselineZero, zero.Reason)
	assert.Equal(t, 0.0, zero.Baseline)
	assert.Empty(t, zero.Pulses)

	// No retained pulses: same undecidable result as the plain analysis.
	quiet := AnalyzeWithMetrics(fill(64, 2, nil), 16000)
	assert.False(t, quiet.Decidable)
	assert.Equal(t, ReasonNoPulses, quiet.Reason)
	assert.Equal(t, 2.0, quiet.Baseline)
	assert.Empty(t, quiet.Pulses)
}

func TestAnalyze_PlainAnalysisCarriesNoMetrics(t *testing.T) {
	// Analyze (used by the legacy route and the dual-channel review) never
	// attaches metrics.
	set := map[int]float64{}
	setRange(set, 5, 8, 13)
	set[6] = 25
	r := Analyze(fill(64, 2, set))
	require.Len(t, r.Pulses, 1)
	assert.Nil(t, r.Pulses[0].DurationMS)
	assert.Nil(t, r.Pulses[0].RMSAmplitude)
}

func TestAnalyzeWithMetrics_ExcludedRangesMetricsCoverInterval(t *testing.T) {
	// 130-sample seam fixture: with the seam shielded the burst at [80,83]
	// is retained; its metrics cover exactly those four samples (13 each),
	// so the RMS is 13 and the duration 0.25 ms at 16 kHz.
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

	r := AnalyzeWithMetrics(s, 16000, Range{Start: 10, End: 74})
	require.True(t, r.Decidable)
	require.Len(t, r.Pulses, 1)
	p := r.Pulses[0]
	assert.Equal(t, 80, p.Start)
	assert.Equal(t, 83, p.End)
	require.NotNil(t, p.DurationMS)
	require.NotNil(t, p.RMSAmplitude)
	assert.Equal(t, 0.25, *p.DurationMS)
	assert.Equal(t, 13.0, *p.RMSAmplitude)
}

func TestRmsAmplitude_ScaledSumOfSquares(t *testing.T) {
	// All-equal amplitudes: the RMS is the amplitude itself, even at the
	// float64 ceiling where naive squaring overflows.
	ceiling := []float64{math.MaxFloat64, math.MaxFloat64, math.MaxFloat64, math.MaxFloat64}
	assert.Equal(t, math.MaxFloat64, rmsAmplitude(ceiling, 0, 3))

	// Signs do not matter: sqrt((3² + 4²)/2) = sqrt(12.5).
	assert.InDelta(t, math.Sqrt(12.5), rmsAmplitude([]float64{-3, 4}, 0, 1), 1e-15)

	// A silent interval has zero RMS.
	assert.Equal(t, 0.0, rmsAmplitude(make([]float64, 4), 0, 3))
}

func TestDurationMS(t *testing.T) {
	assert.Equal(t, 0.25, durationMS(4, 16000))
	assert.Equal(t, 1.0, durationMS(16, 16000))
	assert.Equal(t, 4.0, durationMS(4, 1000))
}
