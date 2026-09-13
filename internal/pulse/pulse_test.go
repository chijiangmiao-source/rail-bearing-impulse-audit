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
	assert.Nil(t, mergeRuns(nil, nil))
	assert.Equal(t, []run{{0, 1}}, mergeRuns([]run{{0, 1}}, nil))
}

func TestValidateRanges(t *testing.T) {
	// No ranges or an empty list is always valid, regardless of length
	// (length itself is the API layer's input contract).
	assert.Nil(t, ValidateRanges(nil, 64))
	assert.Nil(t, ValidateRanges([]Range{}, 64))

	// Adjacent and edge-touching ranges are legal (67 samples minus 3 = 64).
	assert.Nil(t, ValidateRanges([]Range{{0, 0}, {1, 1}, {66, 66}}, 67))

	for _, tc := range []struct {
		name       string
		ranges     []Range
		n          int
		index      int
		constraint string
	}{
		{"end below zero", []Range{{0, -1}}, 64, 0, "range"},
		{"start at length", []Range{{64, 64}}, 64, 0, "range"},
		{"end past length", []Range{{0, 64}}, 64, 0, "range"},
		{"start negative", []Range{{-1, 3}}, 64, 0, "range"},
		{"inverted interval", []Range{{10, 9}}, 64, 0, "order"},
		{"starts not ascending", []Range{{10, 11}, {9, 12}}, 64, 1, "order"},
		{"touching but equal start", []Range{{10, 12}, {10, 12}}, 64, 1, "overlap"},
		{"overlap previous end", []Range{{10, 20}, {20, 21}}, 64, 1, "overlap"},
		{"second offender located", []Range{{0, 0}, {64, 65}}, 64, 1, "range"},
		{"excluding all but 63 samples", []Range{{63, 63}}, 64, 0, "min_length"},
		{"one big exclusion leaves too few", []Range{{0, 0}, {32, 63}}, 64, 1, "min_length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verr := ValidateRanges(tc.ranges, tc.n)
			require.NotNil(t, verr)
			assert.Equal(t, tc.index, verr.Index)
			assert.Equal(t, tc.constraint, verr.Constraint)
		})
	}

	// Exactly 64 remaining samples (n=65, exclude one) is accepted.
	assert.Nil(t, ValidateRanges([]Range{{0, 0}}, 65))
}

func TestAnalyze_ExcludedRangesRecomputeBaseline(t *testing.T) {
	// 130 samples: a high-amplitude wheel-seam block of 65 samples at 30
	// ([10,74]), a 4-sample bearing burst at 13 ([80,83]) and 61 ordinary
	// samples at 2.
	//
	// Sorted multiset with everything kept: 61 twos, 4 thirteens, 65
	// thirties. For 130 samples the median is (sorted[64]+sorted[65])/2 =
	// (13+30)/2 = 21.5, threshold 129, so nothing is a candidate.
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

	full := Analyze(s)
	assert.Equal(t, 21.5, full.Baseline)
	assert.False(t, full.Decidable)
	assert.Equal(t, ReasonNoPulses, full.Reason)

	// Excluding the seam leaves 65 samples: 61 twos and 4 thirteens. The
	// median drops to 2, threshold to 12, and the 13-burst becomes a general
	// pulse (13 > 12 but not > 24).
	r := Analyze(s, Range{Start: 10, End: 74})
	require.True(t, r.Decidable)
	assert.Equal(t, 2.0, r.Baseline)
	require.Len(t, r.Pulses, 1)
	assert.Equal(t, 80, r.Pulses[0].Start)
	assert.Equal(t, 83, r.Pulses[0].End)
	assert.Equal(t, 80, r.Pulses[0].PeakIndex)
	assert.Equal(t, 13.0, r.Pulses[0].Peak)
	assert.Equal(t, SeverityGeneral, r.Pulses[0].Severity)
	assert.Equal(t, 2.0, r.Pulses[0].Baseline)
}

func TestAnalyze_ExcludedRangesSeverityFollowsKeptSamples(t *testing.T) {
	// 130 samples: 61 ordinary twos, a 65-sample seam at 30 ([10,74]) and a
	// 4-sample burst at 200 ([80,83]).
	//
	// Kept multiset: 61 twos, 65 thirties, 4 two-hundreds. The two middle
	// order stats are both 30 -> baseline 30, threshold 180, severeAt 360:
	// the 200-burst is retained but only general.
	s := make([]float64, 130)
	for i := range s {
		s[i] = 2
	}
	for i := 10; i <= 74; i++ {
		s[i] = 30
	}
	for i := 80; i <= 83; i++ {
		s[i] = 200
	}

	full := Analyze(s)
	require.True(t, full.Decidable)
	require.Len(t, full.Pulses, 1)
	assert.Equal(t, 30.0, full.Baseline)
	assert.Equal(t, SeverityGeneral, full.Pulses[0].Severity)

	// Shielding the seam leaves 61 twos and 4 two-hundreds: baseline drops to
	// 2, severeAt to 24, so the very same burst is now severe.
	r := Analyze(s, Range{Start: 10, End: 74})
	require.True(t, r.Decidable)
	assert.Equal(t, 2.0, r.Baseline)
	require.Len(t, r.Pulses, 1)
	assert.Equal(t, SeveritySevere, r.Pulses[0].Severity)
	assert.Equal(t, 200.0, r.Pulses[0].Peak)
}

func TestAnalyze_ExcludedRangesBreakMerging(t *testing.T) {
	// Two 2-sample over-threshold blocks with a single sample between them:
	// without an exclusion they merge (gap 1 <= 3) into one 5-sample pulse.
	set := map[int]float64{}
	setRange(set, 10, 11, 20)
	setRange(set, 14, 15, 20)
	s := fill(64, 2, set)

	merged := Analyze(s)
	require.Len(t, merged.Pulses, 1)
	assert.Equal(t, 10, merged.Pulses[0].Start)
	assert.Equal(t, 15, merged.Pulses[0].End)

	// Excluding the single in-between sample makes the seam an uncrossable
	// boundary: the two blocks are now separate 2-sample candidates, each
	// shorter than minPulseLength -> both filtered, undecidable.
	r := Analyze(s, Range{Start: 12, End: 13})
	assert.False(t, r.Decidable)
	assert.Equal(t, ReasonNoPulses, r.Reason)
	assert.Empty(t, r.Pulses)
}

func TestAnalyze_ExcludedRangeCoversCandidates(t *testing.T) {
	// A 4-sample burst wholly inside an excluded range cannot be a candidate.
	set := map[int]float64{}
	setRange(set, 20, 23, 40)
	s := fill(64, 2, set)
	r := Analyze(s, Range{Start: 20, End: 23})
	assert.False(t, r.Decidable)
	assert.Equal(t, ReasonNoPulses, r.Reason)
	assert.Empty(t, r.Pulses)
}

func TestAnalyze_ExcludedRangeSplitsCandidateAtEdges(t *testing.T) {
	// A long over-threshold run [20,27] with [23,24] excluded splits into
	// [20,22] (3 samples) and [25,27] (3 samples): both are dropped by the
	// length filter and cannot merge across the exclusion.
	set := map[int]float64{}
	setRange(set, 20, 27, 20)
	s := fill(64, 2, set)
	r := Analyze(s, Range{Start: 23, End: 24})
	assert.False(t, r.Decidable)
	assert.Empty(t, r.Pulses)

	// Widening both sides to 4 samples keeps two separate pulses.
	setRange(set, 19, 28, 20)
	s = fill(64, 2, set)
	r = Analyze(s, Range{Start: 23, End: 24})
	require.True(t, r.Decidable)
	require.Len(t, r.Pulses, 2)
	assert.Equal(t, 19, r.Pulses[0].Start)
	assert.Equal(t, 22, r.Pulses[0].End)
	assert.Equal(t, 25, r.Pulses[1].Start)
	assert.Equal(t, 28, r.Pulses[1].End)
}

func TestAnalyze_ExcludedRangesKeepLegacyResult(t *testing.T) {
	set := map[int]float64{}
	setRange(set, 5, 8, 13)
	set[6] = 25
	setRange(set, 40, 43, 13)
	s := fill(64, 2, set)

	want := Analyze(s)
	// An explicit empty range list must match the no-range call field-for-field.
	assert.Equal(t, want, Analyze(s, []Range{}...))
	// Determinism still holds with ranges present.
	withRanges := Analyze(s, Range{Start: 0, End: 0})
	for i := 0; i < 3; i++ {
		assert.Equal(t, withRanges, Analyze(s, Range{Start: 0, End: 0}))
	}
}
