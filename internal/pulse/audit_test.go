package pulse

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// alternating fills n samples with pos and neg alternating by index, then
// applies the per-index overrides. Consecutive base samples differ, so the
// longest flatline without overrides is exactly 1.
func alternating(n int, pos, neg float64, set map[int]float64) []float64 {
	s := make([]float64, n)
	for i := range s {
		if i%2 == 0 {
			s[i] = pos
		} else {
			s[i] = neg
		}
		if v, ok := set[i]; ok {
			s[i] = v
		}
	}
	return s
}

// everyKth builds n samples alternating between pos and neg, with one
// fixed value placed at every k-th index starting at start.
func everyKth(n, start, k int, pos, neg, v float64) []float64 {
	s := alternating(n, pos, neg, nil)
	for i := start; i < n; i += k {
		s[i] = v
	}
	return s
}

func TestAudit_HealthyQuietSignal(t *testing.T) {
	// 256 samples alternating around zero at 1% of full scale: no clipping,
	// no run longer than one sample, mean ratio 0.01.
	a := AuditAcquisition(alternating(256, 1, -1, nil), 100)
	assert.Equal(t, AuditHealthy, a.Status)
	assert.Equal(t, 0.0, a.ClippingRatio)
	assert.Equal(t, 1, a.LongestFlatline)
	assert.Equal(t, 0.01, a.MeanAbsRatio)
	assert.Empty(t, a.Findings)
}

func TestAudit_ClippingBoundaryEquality(t *testing.T) {
	// 1000 samples, exactly five at or past full scale (one negative, one
	// above): ratio lands exactly on the degraded boundary 0.005.
	n := 1000
	set := map[int]float64{0: 100, 200: 100, 400: -100, 600: 100, 800: 101}
	a := AuditAcquisition(alternating(n, 1, -1, set), 100)
	assert.Equal(t, 0.005, a.ClippingRatio, "|x| >= full_scale clips, including -full_scale and above")
	assert.Equal(t, AuditDegraded, a.Status)
	require.Len(t, a.Findings, 1)
	assert.Equal(t, FindingClipping, a.Findings[0].Code)
	assert.Equal(t, AuditDegraded, a.Findings[0].Severity)

	// Exactly 50 of 1000 isolated samples at full scale lands exactly on
	// the rejected boundary 0.05: equality rejects.
	reject := AuditAcquisition(everyKth(n, 0, 20, 1, -1, 100), 100)
	assert.Equal(t, 0.05, reject.ClippingRatio)
	assert.Equal(t, AuditRejected, reject.Status)
	require.Len(t, reject.Findings, 1, "mean ratio 0.0595 and no flatline add no findings")
	assert.Equal(t, FindingClipping, reject.Findings[0].Code)
	assert.Equal(t, AuditRejected, reject.Findings[0].Severity)

	// One sample fewer at full scale: 0.049 stays degraded (below the
	// rejected boundary, still at/above 0.5%).
	oneLess := everyKth(n, 0, 20, 1, -1, 100)
	oneLess[980] = -1
	b := AuditAcquisition(oneLess, 100)
	assert.Equal(t, 0.049, b.ClippingRatio)
	assert.Equal(t, AuditDegraded, b.Status)

	// Four of 1000 (0.4%) is below even the degraded boundary: healthy.
	healthy := AuditAcquisition(alternating(n, 1, -1,
		map[int]float64{0: 100, 250: 100, 500: 100, 750: 100}), 100)
	assert.Equal(t, 0.004, healthy.ClippingRatio)
	assert.Equal(t, AuditHealthy, healthy.Status)
	assert.Empty(t, healthy.Findings)
}

func TestAudit_FlatlineBoundaryEqualityAndLongestRun(t *testing.T) {
	// A run of exactly 16 equal small samples lands on the degraded
	// boundary. The base alternates ±1; overriding 0..15 to 1 would extend
	// onto index 16 (whose base is also 1), so 16 is forced to -1.
	sixteen := alternating(256, 1, -1, nil)
	for i := 0; i <= 15; i++ {
		sixteen[i] = 1
	}
	sixteen[16] = -1
	d := AuditAcquisition(sixteen, 100)
	assert.Equal(t, 16, d.LongestFlatline)
	assert.Equal(t, AuditDegraded, d.Status)
	require.Len(t, d.Findings, 1)
	assert.Equal(t, FindingFlatline, d.Findings[0].Code)
	assert.Equal(t, AuditDegraded, d.Findings[0].Severity)

	// A run of exactly 64 equal samples lands on the rejected boundary and
	// beats any shorter run elsewhere.
	samples := make([]float64, 256)
	for i := 0; i <= 63; i++ {
		samples[i] = 1
	}
	for i := 64; i < 256; i++ { // break at index 64, then alternate
		if i%2 == 0 {
			samples[i] = -1
		} else {
			samples[i] = 1
		}
	}
	// A shorter run of twenty later must not replace the 64-sample longest.
	for i := 200; i <= 219; i++ {
		samples[i] = -5
	}
	r := AuditAcquisition(samples, 100)
	assert.Equal(t, 64, r.LongestFlatline)
	assert.Equal(t, AuditRejected, r.Status)
	require.Len(t, r.Findings, 1)
	assert.Equal(t, FindingFlatline, r.Findings[0].Code)
	assert.Equal(t, AuditRejected, r.Findings[0].Severity)

	// 63 equal samples is one short of the rejected boundary and stays
	// degraded (it is at least 16).
	s63 := make([]float64, 256)
	for i := 0; i <= 62; i++ {
		s63[i] = 1
	}
	s63[63] = -1 // break the run so it stays exactly 63
	for i := 64; i < 256; i++ {
		if i%2 == 0 {
			s63[i] = -1
		} else {
			s63[i] = 1
		}
	}
	b := AuditAcquisition(s63, 100)
	assert.Equal(t, 63, b.LongestFlatline)
	assert.Equal(t, AuditDegraded, b.Status)

	// A run of 15 equal negatives is below the degraded boundary.
	fifteen := alternating(256, 1, -1, nil)
	for i := 100; i <= 114; i++ {
		fifteen[i] = -7
	}
	h := AuditAcquisition(fifteen, 100)
	assert.Equal(t, 15, h.LongestFlatline)
	assert.Equal(t, AuditHealthy, h.Status)
	assert.Empty(t, h.Findings)
}

func TestAudit_MeanLevelBoundaryEquality(t *testing.T) {
	// Every sample at exactly 10% of full scale with alternating signs: no
	// clipping and no flatline, mean ratio lands exactly on 0.10.
	a := AuditAcquisition(alternating(256, 10, -10, nil), 100)
	assert.Equal(t, 0.0, a.ClippingRatio)
	assert.Equal(t, 1, a.LongestFlatline)
	assert.Equal(t, 0.10, a.MeanAbsRatio)
	assert.Equal(t, AuditDegraded, a.Status)
	require.Len(t, a.Findings, 1)
	assert.Equal(t, FindingHighMeanLevel, a.Findings[0].Code)
	assert.Equal(t, AuditDegraded, a.Findings[0].Severity)

	// Just under 10% is healthy even though the level is high in absolute
	// terms.
	h := AuditAcquisition(alternating(256, 9.99, -9.99, nil), 100)
	assert.Equal(t, AuditHealthy, h.Status)
	assert.Empty(t, h.Findings)
}

func TestAudit_RejectedStatusWinsAndFindingsOrdered(t *testing.T) {
	// n=1000: five isolated full-scale samples (0.5%, degraded clipping),
	// a run of sixteen 50s on [101,116] (degraded flatline) and a base
	// level of ±10 (mean ratio 0.1 plus the raises, high mean level).
	// Nothing else is clipped; the status must be degraded (no reject
	// trigger), and findings must be code-sorted.
	n := 1000
	set := map[int]float64{0: 100, 200: 100, 400: 100, 600: 100, 800: 100}
	for i := 101; i <= 116; i++ {
		set[i] = 50
	}
	d := AuditAcquisition(alternating(n, 10, -10, set), 100)
	assert.Equal(t, AuditDegraded, d.Status)
	require.Len(t, d.Findings, 3)
	assert.Equal(t, []FindingCode{
		FindingClipping, FindingFlatline, FindingHighMeanLevel,
	}, []FindingCode{d.Findings[0].Code, d.Findings[1].Code, d.Findings[2].Code})
	assert.Equal(t, AuditDegraded, d.Findings[0].Severity)

	// Push clipping to the rejected boundary: status becomes rejected even
	// though the other two observations are only degraded, the clipping
	// finding carries rejected severity and the ordering is unchanged.
	for i := 20; i < n; i += 20 {
		if _, ok := set[i]; !ok {
			set[i] = 100 // 45 additional isolated clips -> 50 total
		}
	}
	r := AuditAcquisition(alternating(n, 10, -10, set), 100)
	assert.Equal(t, 0.05, r.ClippingRatio)
	assert.Equal(t, AuditRejected, r.Status)
	require.Len(t, r.Findings, 3)
	assert.Equal(t, []FindingCode{
		FindingClipping, FindingFlatline, FindingHighMeanLevel,
	}, []FindingCode{r.Findings[0].Code, r.Findings[1].Code, r.Findings[2].Code})
	assert.Equal(t, AuditRejected, r.Findings[0].Severity)
	assert.Equal(t, AuditDegraded, r.Findings[1].Severity)
	assert.Equal(t, AuditDegraded, r.Findings[2].Severity)
}

func TestAudit_AllZeroSignalIsRejectedFlatline(t *testing.T) {
	// A stalled capture card submitting legal zeros: ratio 0 but the whole
	// sequence is one flatline, so the audit rejects instead of trusting
	// the values.
	a := AuditAcquisition(make([]float64, 256), 100)
	assert.Equal(t, 0.0, a.ClippingRatio)
	assert.Equal(t, 256, a.LongestFlatline)
	assert.Equal(t, 0.0, a.MeanAbsRatio)
	assert.Equal(t, AuditRejected, a.Status)
	require.Len(t, a.Findings, 1)
	assert.Equal(t, FindingFlatline, a.Findings[0].Code)
}

func TestAudit_Deterministic(t *testing.T) {
	set := map[int]float64{0: 100, 200: 100, 400: 100, 600: 100, 800: 100}
	for i := 101; i <= 116; i++ {
		set[i] = 50
	}
	samples := alternating(1000, 10, -10, set)
	first := AuditAcquisition(samples, 100)
	for i := 0; i < 5; i++ {
		assert.Equal(t, first, AuditAcquisition(samples, 100))
	}
}

func TestAudit_HugeFiniteValuesStayFinite(t *testing.T) {
	// Finite samples near the float64 ceiling must not turn the mean sum
	// into +Inf: a plain sum would overflow. The scaled accumulation keeps
	// every reported ratio finite.
	samples := alternating(256, 1, -1, nil)
	samples[0] = math.MaxFloat64
	a := AuditAcquisition(samples, 100)
	assert.False(t, math.IsInf(a.MeanAbsRatio, 0))
	assert.False(t, math.IsNaN(a.MeanAbsRatio))
	assert.False(t, math.IsInf(a.ClippingRatio, 0))
	assert.Equal(t, 1.0/256, a.ClippingRatio)
	// One clip is below the 0.5% clipping boundary, but the enormous mean
	// level flags the capture as degraded.
	assert.Equal(t, AuditDegraded, a.Status)
}
