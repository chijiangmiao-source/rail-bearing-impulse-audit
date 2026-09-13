package pulse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// burstSignal builds a 64-sample baseline-2 signal and stamps a 4-sample
// general burst (value 13) at [start, start+3], optionally raising one
// sample to peakVal to move the earliest maximum.
func burstSignal(bursts ...burst) []float64 {
	set := map[int]float64{}
	for _, b := range bursts {
		for i := b.start; i <= b.start+3; i++ {
			set[i] = 13
		}
		set[b.peakIndex] = b.peakValue
	}
	return fill(64, 2, set)
}

type burst struct {
	start     int
	peakIndex int
	peakValue float64
}

// generalBurst is a 4-sample equal-amplitude burst; its earliest sample
// carries the (tied) peak.
func generalBurst(start int) burst {
	return burst{start: start, peakIndex: start, peakValue: 13}
}

// severeBurstAt raises peakIndex inside the burst starting at start.
func severeBurstAt(start, peakIndex int) burst {
	return burst{start: start, peakIndex: peakIndex, peakValue: 25}
}

func correlateFixture(sampleRate float64, tolerance int, left, right []float64, lr, rr []Range) CorrelationRequest {
	return CorrelationRequest{
		SampleRate:       sampleRate,
		ToleranceSamples: tolerance,
		Left: ChannelInput{
			SampleRate: sampleRate, Amplitudes: left, ExcludedRanges: lr,
		},
		Right: ChannelInput{
			SampleRate: sampleRate, Amplitudes: right, ExcludedRanges: rr,
		},
	}
}

func TestCorrelate_PrecisePairing(t *testing.T) {
	// Two pulses on each side, peaks at 6 (severe) and 40 (general).
	sig := burstSignal(severeBurstAt(5, 6), generalBurst(40))
	r := Correlate(correlateFixture(16000, 5, sig, sig, nil, nil))

	require.True(t, r.Left.Decidable)
	require.True(t, r.Right.Decidable)
	require.Len(t, r.Left.Pulses, 2)
	require.Len(t, r.Right.Pulses, 2)
	assert.Equal(t, []Pair{
		{LeftPulseIndex: 0, RightPulseIndex: 0, TimeDifference: 0},
		{LeftPulseIndex: 1, RightPulseIndex: 1, TimeDifference: 0},
	}, r.Pairs)
	assert.Empty(t, r.LeftUnpaired)
	assert.Empty(t, r.RightUnpaired)

	// The original analyses are preserved verbatim.
	assert.Equal(t, Analyze(sig), r.Left)
	assert.Equal(t, Analyze(sig), r.Right)
}

func TestCorrelate_ToleranceBoundaryIsInclusive(t *testing.T) {
	// Left peak at 6, right peak one sample later at 7.
	left := burstSignal(severeBurstAt(5, 6))
	right := burstSignal(severeBurstAt(5, 7))

	zero := Correlate(correlateFixture(16000, 0, left, right, nil, nil))
	assert.Empty(t, zero.Pairs, "zero tolerance must not bridge different peak times")
	assert.Equal(t, []int{0}, zero.LeftUnpaired)
	assert.Equal(t, []int{0}, zero.RightUnpaired)

	one := Correlate(correlateFixture(16000, 1, left, right, nil, nil))
	assert.Equal(t, []Pair{{LeftPulseIndex: 0, RightPulseIndex: 0, TimeDifference: 1}},
		one.Pairs)
	assert.Empty(t, one.LeftUnpaired)
	assert.Empty(t, one.RightUnpaired)
}

func TestCorrelate_UnequalPulseCounts(t *testing.T) {
	// Left has two pulses; the right side only carries the later one.
	left := burstSignal(generalBurst(5), generalBurst(40))
	right := burstSignal(generalBurst(40))
	r := Correlate(correlateFixture(16000, 3, left, right, nil, nil))

	assert.Equal(t, []Pair{
		{LeftPulseIndex: 1, RightPulseIndex: 0, TimeDifference: 0},
	}, r.Pairs)
	// Evidence stays ordered by ascending left pulse index.
	assert.Equal(t, []int{0}, r.LeftUnpaired)
	assert.Empty(t, r.RightUnpaired)
}

func TestCorrelate_CandidateChosenBySmallestTimeDifference(t *testing.T) {
	// One left pulse (peak 12) with two right candidates: the earlier right
	// pulse peaks at 4 (difference 8) and the later right pulse at 10
	// (difference 2). Enumerating by right pulse index would grab R0; the
	// contract orders candidates by time difference first, so R1 wins even
	// though R0 is also inside the tolerance of 8.
	left := burstSignal(severeBurstAt(10, 12))
	right := burstSignal(severeBurstAt(2, 4), generalBurst(10))
	r := Correlate(correlateFixture(16000, 8, left, right, nil, nil))

	assert.Equal(t, []Pair{
		{LeftPulseIndex: 0, RightPulseIndex: 1, TimeDifference: 2},
	}, r.Pairs)
	assert.Empty(t, r.LeftUnpaired)
	assert.Equal(t, []int{0}, r.RightUnpaired, "the larger-difference candidate is left unpaired")

	// With tolerance 7 the difference-8 candidate is out of range outright;
	// the outcome is the same unique pairing.
	r = Correlate(correlateFixture(16000, 7, left, right, nil, nil))
	assert.Equal(t, []Pair{{LeftPulseIndex: 0, RightPulseIndex: 1, TimeDifference: 2}}, r.Pairs)
	assert.Equal(t, []int{0}, r.RightUnpaired)
}

func TestCorrelate_OccupiedPulseSkipsLaterCandidate(t *testing.T) {
	// Left peaks at 10 and 20, the single right pulse peaks at 15. Both
	// candidates differ by exactly the tolerance of 5; the tie breaks to
	// the smaller left pulse index, and once R0 is occupied the later
	// candidate is skipped.
	left := burstSignal(severeBurstAt(8, 10), severeBurstAt(18, 20))
	right := burstSignal(severeBurstAt(13, 15))
	r := Correlate(correlateFixture(16000, 5, left, right, nil, nil))

	assert.Equal(t, []Pair{
		{LeftPulseIndex: 0, RightPulseIndex: 0, TimeDifference: 5},
	}, r.Pairs)
	assert.Equal(t, []int{1}, r.LeftUnpaired)
	assert.Empty(t, r.RightUnpaired)
}

func TestCorrelate_EvidenceOrderedByLeftPeakRegardlessOfSelection(t *testing.T) {
	// Left peaks at 10 (L0) and 20 (L1); right peaks at 15 (R0) and 21
	// (R1). Greedy order by time difference selects L1-R1 (difference 1)
	// before L0-R0 (difference 5); the response must still list evidence
	// by ascending left pulse index.
	left := burstSignal(severeBurstAt(8, 10), severeBurstAt(18, 20))
	right := burstSignal(severeBurstAt(13, 15), severeBurstAt(21, 21))
	r := Correlate(correlateFixture(16000, 5, left, right, nil, nil))

	assert.Equal(t, []Pair{
		{LeftPulseIndex: 0, RightPulseIndex: 0, TimeDifference: 5},
		{LeftPulseIndex: 1, RightPulseIndex: 1, TimeDifference: 1},
	}, r.Pairs)
	assert.Empty(t, r.LeftUnpaired)
	// The alternative L1-R0 candidate (difference 5) is skipped because R0
	// is already paired with L0.
	assert.Empty(t, r.RightUnpaired)
}

func TestCorrelate_OneSideUndecidableProducesNoEvidence(t *testing.T) {
	// The seam fixture is undecidable without shielding and decidable once
	// the seam [10,74] is excluded.
	s := seamDomainSamples()

	shielded := []Range{{Start: 10, End: 74}}
	left := Analyze(s, shielded...)
	require.True(t, left.Decidable)
	right := Analyze(s)
	require.False(t, right.Decidable)
	require.Len(t, left.Pulses, 1)
	assert.Equal(t, 80, left.Pulses[0].PeakIndex)

	r := Correlate(correlateFixture(16000, 3, s, s, shielded, nil))
	assert.True(t, r.Left.Decidable)
	assert.False(t, r.Right.Decidable)
	assert.Equal(t, ReasonNoPulses, r.Right.Reason)
	assert.Empty(t, r.Pairs, "an undecidable side forbids any evidence")
	assert.Equal(t, []int{0}, r.LeftUnpaired)
	assert.Empty(t, r.RightUnpaired)

	// Shielding both sides pairs the surviving bursts.
	r = Correlate(correlateFixture(16000, 3, s, s, shielded, shielded))
	require.True(t, r.Right.Decidable)
	assert.Equal(t, []Pair{{LeftPulseIndex: 0, RightPulseIndex: 0, TimeDifference: 0}},
		r.Pairs)
	assert.Empty(t, r.LeftUnpaired)
	assert.Empty(t, r.RightUnpaired)
}

func TestCorrelate_BaselineZeroSideIsUndecidable(t *testing.T) {
	left := burstSignal(generalBurst(5))
	right := make([]float64, 64) // baseline 0
	r := Correlate(correlateFixture(16000, 3, left, right, nil, nil))

	assert.True(t, r.Left.Decidable)
	assert.False(t, r.Right.Decidable)
	assert.Equal(t, ReasonBaselineZero, r.Right.Reason)
	assert.Empty(t, r.Pairs)
	assert.Equal(t, []int{0}, r.LeftUnpaired)
	assert.Empty(t, r.RightUnpaired)
}

func TestCorrelate_SlicesAreNeverNilAndDeterministic(t *testing.T) {
	// Flat signals: neither side is decidable and every output slice must
	// still be an empty JSON array, never null.
	flat := make([]float64, 64)
	for i := range flat {
		flat[i] = 2
	}
	r := Correlate(correlateFixture(16000, 5, flat, flat, nil, nil))
	assert.False(t, r.Left.Decidable)
	assert.False(t, r.Right.Decidable)
	assert.NotNil(t, r.Pairs)
	assert.NotNil(t, r.LeftUnpaired)
	assert.NotNil(t, r.RightUnpaired)
	assert.Len(t, r.Pairs, 0)

	again := Correlate(correlateFixture(16000, 5, flat, flat, nil, nil))
	assert.Equal(t, r, again)
}

// seamDomainSamples is the wheel-seam fixture used in the package tests.
func seamDomainSamples() []float64 {
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
