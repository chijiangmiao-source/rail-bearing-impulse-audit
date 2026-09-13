package pulse

import (
	"math"
	"sort"
)

// Acquisition audit input bounds. They are part of the audit contract: an
// audit needs enough samples for the flatline and clipping ratios to mean
// anything, and the upper bound matches the analysis pipeline.
const (
	AuditMinSamples = 256
	AuditMaxSamples = MaxSamples
)

// Audit status classes, worst case wins.
type AuditStatus string

const (
	AuditHealthy  AuditStatus = "healthy"
	AuditDegraded AuditStatus = "degraded"
	AuditRejected AuditStatus = "rejected"
)

// Finding codes are the fixed, sortable vocabulary of audit findings.
type FindingCode string

const (
	// FindingClipping flags samples whose magnitude reaches full scale.
	FindingClipping FindingCode = "clipping"
	// FindingFlatline flags the longest run of consecutive equal samples.
	FindingFlatline FindingCode = "flatline"
	// FindingHighMeanLevel flags a mean absolute level at or above 10% of
	// full scale: a saturated or dead channel whose samples stay legal.
	FindingHighMeanLevel FindingCode = "high_mean_level"
)

// Audit thresholds are inclusive: a metric that lands exactly on the boundary
// already triggers the corresponding class (or finding). They are part of the
// contract and locked by tests.
const (
	clippingRejectRatio = 0.05  // >=5% of samples at full scale -> rejected
	clippingDegradedAt  = 0.005 // >=0.5% -> degraded
	flatlineRejectAt    = 64    // longest run >= 64 samples -> rejected
	flatlineDegradedAt  = 16    // longest run >= 16 samples -> degraded
	meanLevelDegradedAt = 0.10  // |mean|/full_scale >= 10% -> degraded
)

// Finding is one audit observation in the fixed vocabulary. Severity is the
// class the observation pushes the audit to ("degraded" or "rejected").
type Finding struct {
	Code     FindingCode `json:"code"`
	Severity AuditStatus `json:"severity"`
	Message  string      `json:"message"`
}

// AcquisitionAudit is the independent acquisition-quality report. It is
// produced before pulse judgement from the raw samples alone, so a loose
// pickup, a saturated channel or a stalled capture card that still submits
// legal numeric values cannot hide behind the analysis.
type AcquisitionAudit struct {
	// ClippingRatio is the fraction of samples with |x| >= full_scale.
	ClippingRatio float64 `json:"clipping_ratio"`
	// LongestFlatline is the length in samples of the longest closed
	// interval of consecutive equal values (equality is by float64 bit
	// value); with at least one sample it is at least 1.
	LongestFlatline int `json:"longest_flatline"`
	// MeanAbsRatio is mean(|x|) / full_scale.
	MeanAbsRatio float64 `json:"mean_abs_ratio"`
	// Status is "healthy", "degraded" or "rejected".
	Status AuditStatus `json:"status"`
	// Findings is never nil and is ordered by the fixed rule: code
	// alphabetically, then severity (rejected before degraded), then
	// message. The three codes are mutually exclusive per audit, so in
	// practice the list is simply code-sorted; the remaining keys make the
	// rule total.
	Findings []Finding `json:"findings"`
}

// AuditAcquisition computes the acquisition-quality report from finite
// samples with a positive full scale. The API layer guarantees length,
// finiteness and full scale; AuditAcquisition itself relies only on those,
// mirroring Analyze's split of responsibilities. The result is deterministic:
// one input yields one bit-identical report.
func AuditAcquisition(samples []float64, fullScale float64) AcquisitionAudit {
	clipped := 0
	longestRun := 0
	runLength := 0
	maxAbs := 0.0

	// First pass: clipping count, longest equal-value run and the largest
	// magnitude. The mean is accumulated scaled in a second pass so finite
	// samples near the float64 ceiling cannot overflow a plain sum to +Inf
	// (the same scaling concern rmsAmplitude guards against).
	for i, v := range samples {
		av := math.Abs(v)
		if av >= fullScale {
			clipped++
		}
		if av > maxAbs {
			maxAbs = av
		}

		// Consecutive equal values, equality by float64 bit value: -0 == 0
		// and two NaNs would compare equal too, but the API rejects NaN.
		if i > 0 && v == samples[i-1] {
			runLength++
		} else {
			runLength = 1
		}
		if runLength > longestRun {
			longestRun = runLength
		}
	}

	scaledSum := 0.0
	if maxAbs > 0 {
		for _, v := range samples {
			scaledSum += math.Abs(v) / maxAbs
		}
	}

	n := float64(len(samples))
	clippingRatio := float64(clipped) / n
	meanAbsRatio := maxAbs * scaledSum / n / fullScale

	audit := AcquisitionAudit{
		ClippingRatio:   clippingRatio,
		LongestFlatline: longestRun,
		MeanAbsRatio:    meanAbsRatio,
		Status:          AuditHealthy,
		Findings:        []Finding{},
	}

	// Rejected boundaries are evaluated first and win: the rejected class
	// is never downgraded by a degraded observation.
	if clippingRatio >= clippingRejectRatio {
		audit.Status = AuditRejected
		audit.Findings = append(audit.Findings, Finding{
			Code: FindingClipping, Severity: AuditRejected,
			Message: "clipping ratio at or above 5% of samples reaching full scale",
		})
	} else if clippingRatio >= clippingDegradedAt {
		audit.Status = AuditDegraded
		audit.Findings = append(audit.Findings, Finding{
			Code: FindingClipping, Severity: AuditDegraded,
			Message: "clipping ratio at or above 0.5% of samples reaching full scale",
		})
	}

	if longestRun >= flatlineRejectAt {
		audit.Status = AuditRejected
		audit.Findings = append(audit.Findings, Finding{
			Code: FindingFlatline, Severity: AuditRejected,
			Message: "longest flatline run at or above 64 consecutive equal samples",
		})
	} else if longestRun >= flatlineDegradedAt {
		if audit.Status != AuditRejected {
			audit.Status = AuditDegraded
		}
		audit.Findings = append(audit.Findings, Finding{
			Code: FindingFlatline, Severity: AuditDegraded,
			Message: "longest flatline run at or above 16 consecutive equal samples",
		})
	}

	if meanAbsRatio >= meanLevelDegradedAt {
		if audit.Status != AuditRejected {
			audit.Status = AuditDegraded
		}
		audit.Findings = append(audit.Findings, Finding{
			Code: FindingHighMeanLevel, Severity: AuditDegraded,
			Message: "mean absolute level at or above 10% of full scale",
		})
	}

	sort.Slice(audit.Findings, func(i, j int) bool {
		a, b := audit.Findings[i], audit.Findings[j]
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.Severity != b.Severity {
			// "rejected" sorts before "degraded".
			return a.Severity == AuditRejected
		}
		return a.Message < b.Message
	})

	return audit
}
