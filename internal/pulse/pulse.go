// Package pulse implements the trackside acoustic pulse detector.
//
// All operations are deterministic: given the same finite float64 input the
// analysis yields bit-for-bit identical output.
package pulse

import (
	"fmt"
	"math"
	"sort"
)

// Input and decision constants. They are part of the detection contract.
const (
	MinSampleRate = 1000.0
	MaxSampleRate = 48000.0
	MinSamples    = 64
	MaxSamples    = 20000

	// thresholdFactor: a sample strictly above thresholdFactor*baseline is
	// above the threshold.
	thresholdFactor = 6.0
	// severeFactor: a pulse peak strictly above severeFactor*baseline is severe.
	severeFactor = 12.0
	// maxMergeGap: candidates separated by at most this many non-exceeding
	// samples are merged into one closed interval.
	maxMergeGap = 3
	// minPulseLength: merged intervals shorter than this many samples are
	// discarded.
	minPulseLength = 4
)

// Severity classifies a retained pulse.
type Severity string

const (
	SeveritySevere  Severity = "severe"
	SeverityGeneral Severity = "general"
)

// Pulse is one retained closed interval with its earliest-maximum peak.
type Pulse struct {
	// Start and End are zero-based, inclusive sample indices.
	Start int `json:"start"`
	End   int `json:"end"`
	// PeakIndex is the earliest index carrying the maximum absolute value.
	PeakIndex int `json:"peak_index"`
	// Peak is the maximum absolute amplitude inside the interval.
	Peak float64 `json:"peak"`
	// Severity is "severe" (peak strictly above 12*baseline) or "general".
	Severity Severity `json:"severity"`
	// Baseline is the whole-sequence absolute median used for this pulse.
	Baseline float64 `json:"baseline"`
}

// Result is the full analysis outcome.
type Result struct {
	// Decidable is false when the baseline is zero or no interval survives.
	Decidable bool    `json:"decidable"`
	Baseline  float64 `json:"baseline"`
	// Pulses is never nil and is ordered by ascending start index.
	Pulses []Pulse `json:"pulses"`
	// Reason is "baseline_zero" or "no_pulses" when Decidable is false.
	Reason string `json:"reason,omitempty"`
}

// Reasons reported for undecidable input.
const (
	ReasonBaselineZero = "baseline_zero"
	ReasonNoPulses     = "no_pulses"
)

type run struct {
	start int
	end   int
}

// Range is one zero-based, inclusive [Start, End] sample interval that does
// not participate in bearing judgement: its samples are neither candidates
// nor part of the baseline, and it is a boundary merging can never cross.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// ValidationError describes one invalid excluded_ranges element. Index is the
// zero-based element index inside excluded_ranges and Constraint is one of
// "range", "order", "overlap" or "min_length" (structural type errors are
// produced by the API layer before validation).
type ValidationError struct {
	Index      int
	Constraint string
	Message    string
}

func (e *ValidationError) Error() string { return e.Message }

// ValidateRanges checks excluded ranges against an amplitudes slice of length
// n: endpoints must lie inside [0, n-1], every interval must be ordered
// start <= end, starts must be strictly ascending, intervals must not overlap
// (adjacency is allowed), and at least MinSamples samples must remain.
//
// An empty list is always valid. Structural violations report the first
// offending element; a remaining-count shortfall is attributed to the first
// element whose exclusion pushes the kept count below the minimum.
func ValidateRanges(ranges []Range, n int) *ValidationError {
	for i, r := range ranges {
		if r.Start < 0 || r.Start >= n || r.End < 0 || r.End >= n {
			return &ValidationError{
				Index: i, Constraint: "range",
				Message: fmt.Sprintf(
					"excluded_ranges[%d] endpoints must lie within amplitudes [0, %d], got {%d, %d}",
					i, n-1, r.Start, r.End),
			}
		}
		if r.Start > r.End {
			return &ValidationError{
				Index: i, Constraint: "order",
				Message: fmt.Sprintf(
					"excluded_ranges[%d] start must not be greater than end, got {%d, %d}",
					i, r.Start, r.End),
			}
		}
		if i > 0 && r.Start < ranges[i-1].Start {
			return &ValidationError{
				Index: i, Constraint: "order",
				Message: fmt.Sprintf(
					"excluded_ranges must be ascending by start; excluded_ranges[%d] starts at %d after %d",
					i, r.Start, ranges[i-1].Start),
			}
		}
		if i > 0 && r.Start <= ranges[i-1].End {
			return &ValidationError{
				Index: i, Constraint: "overlap",
				Message: fmt.Sprintf(
					"excluded_ranges[%d] {%d, %d} overlaps excluded_ranges[%d] ending at %d",
					i, r.Start, r.End, i-1, ranges[i-1].End),
			}
		}
	}

	remaining := n
	for i, r := range ranges {
		remaining -= r.End - r.Start + 1
		if remaining < MinSamples {
			// Locate the first element whose exclusion makes the kept count
			// fall below the minimum, not the last element of the list.
			return &ValidationError{
				Index: i, Constraint: "min_length",
				Message: fmt.Sprintf(
					"at least %d samples must remain after excluding ranges, got %d",
					MinSamples, remaining),
			}
		}
	}
	return nil
}

// Analyze runs the full detection pipeline.
//
// Validation of amplitudes (finiteness, length) is the API layer's
// responsibility; Analyze itself relies only on finite float64 values.
// excludedRanges are assumed validated with ValidateRanges: when non-empty the
// baseline is recomputed from non-excluded samples, excluded samples can never
// start or join a candidate, and an excluded interval is a boundary that
// merging cannot cross.
func Analyze(amplitudes []float64, excludedRanges ...Range) Result {
	excluded := excludedMask(len(amplitudes), excludedRanges)
	baseline := medianAbsKept(amplitudes, excluded)

	// A zero baseline makes thresholding meaningless: any non-zero sample
	// would trivially exceed 0. Report undecidable and fabricate no pulses.
	if baseline == 0 {
		return Result{
			Decidable: false,
			Baseline:  0,
			Pulses:    []Pulse{},
			Reason:    ReasonBaselineZero,
		}
	}

	threshold := thresholdFactor * baseline
	severeAt := severeFactor * baseline

	// Stage 1: maximal contiguous runs of samples strictly above threshold.
	// An excluded sample is not a candidate and breaks a run; mergeRuns then
	// refuses to bridge any pair of runs that has an excluded sample between
	// them, even when their index gap is at most maxMergeGap. Candidates
	// touching a boundary therefore never include excluded samples.
	candidates := make([]run, 0)
	for i := 0; i < len(amplitudes); {
		if !excluded[i] && math.Abs(amplitudes[i]) > threshold {
			start := i
			for i < len(amplitudes) && !excluded[i] && math.Abs(amplitudes[i]) > threshold {
				i++
			}
			candidates = append(candidates, run{start: start, end: i - 1})
		} else {
			i++
		}
	}

	// Stages 2-3: merge close runs, then discard short closed intervals.
	pulses := make([]Pulse, 0)
	for _, r := range mergeRuns(candidates, excluded) {
		if r.end-r.start+1 < minPulseLength {
			continue
		}

		// Stage 4: peak over the whole merged interval; ties keep the
		// earliest index because only a strictly larger value replaces it.
		peakIndex := r.start
		peak := math.Abs(amplitudes[r.start])
		for j := r.start + 1; j <= r.end; j++ {
			v := math.Abs(amplitudes[j])
			if v > peak {
				peak = v
				peakIndex = j
			}
		}

		severity := SeverityGeneral
		if peak > severeAt {
			severity = SeveritySevere
		}

		pulses = append(pulses, Pulse{
			Start:     r.start,
			End:       r.end,
			PeakIndex: peakIndex,
			Peak:      peak,
			Severity:  severity,
			Baseline:  baseline,
		})
	}

	if len(pulses) == 0 {
		return Result{
			Decidable: false,
			Baseline:  baseline,
			Pulses:    []Pulse{},
			Reason:    ReasonNoPulses,
		}
	}

	return Result{
		Decidable: true,
		Baseline:  baseline,
		Pulses:    pulses,
	}
}

// mergeRuns joins runs separated by at most maxMergeGap non-exceeding
// samples. Runs are expected in ascending index order, which preserves that
// order. An excluded sample between two runs is an uncrossable boundary even
// when the index gap is at most maxMergeGap.
func mergeRuns(runs []run, excluded []bool) []run {
	if len(runs) == 0 {
		return nil
	}
	merged := make([]run, 0, len(runs))
	merged = append(merged, runs[0])
	for _, r := range runs[1:] {
		last := &merged[len(merged)-1]
		crossable := r.start-last.end-1 <= maxMergeGap &&
			!hasExcludedBetween(excluded, last.end+1, r.start-1)
		if crossable {
			last.end = r.end
		} else {
			merged = append(merged, r)
		}
	}
	return merged
}

// hasExcludedBetween reports whether the closed interval [lo, hi] contains an
// excluded sample. An empty interval (lo > hi) returns false.
func hasExcludedBetween(excluded []bool, lo, hi int) bool {
	for j := lo; j <= hi; j++ {
		if j >= 0 && j < len(excluded) && excluded[j] {
			return true
		}
	}
	return false
}

// excludedMask builds a per-sample exclusion lookup from validated ranges.
func excludedMask(n int, ranges []Range) []bool {
	mask := make([]bool, n)
	for _, r := range ranges {
		for j := r.Start; j <= r.End; j++ {
			mask[j] = true
		}
	}
	return mask
}

// medianAbs returns the median of the absolute amplitudes. For an even number
// of samples it is the arithmetic mean of the two middle values.
func medianAbs(a []float64) float64 {
	abs := make([]float64, len(a))
	for i, v := range a {
		abs[i] = math.Abs(v)
	}
	return medianSorted(abs)
}

// medianAbsKept is medianAbs over samples whose mask entry is false. With a
// nil mask every sample participates, matching the legacy whole-sequence
// baseline bit-for-bit.
func medianAbsKept(a []float64, excluded []bool) float64 {
	abs := make([]float64, 0, len(a))
	for i, v := range a {
		if excluded != nil && excluded[i] {
			continue
		}
		abs = append(abs, math.Abs(v))
	}
	return medianSorted(abs)
}

// medianSorted returns the median of the given values, sorting them in place
// first. For an even count it is the arithmetic mean of the two middle values.
func medianSorted(abs []float64) float64 {
	sort.Float64s(abs)

	n := len(abs)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return abs[n/2]
	}
	x, y := abs[n/2-1], abs[n/2]
	mean := (x + y) / 2
	// Guard against overflow when both middle values approach MaxFloat64:
	// divide first so the reported baseline stays finite.
	if math.IsInf(mean, 0) {
		mean = x/2 + y/2
	}
	return mean
}
