// Package pulse implements the trackside acoustic pulse detector.
//
// All operations are deterministic: given the same finite float64 input the
// analysis yields bit-for-bit identical output.
package pulse

import (
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

// Analyze runs the full detection pipeline.
//
// Validation of ranges and finiteness is the API layer's responsibility;
// Analyze itself relies only on finite float64 values.
func Analyze(amplitudes []float64) Result {
	baseline := medianAbs(amplitudes)

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
	candidates := make([]run, 0)
	for i := 0; i < len(amplitudes); {
		if math.Abs(amplitudes[i]) > threshold {
			start := i
			for i < len(amplitudes) && math.Abs(amplitudes[i]) > threshold {
				i++
			}
			candidates = append(candidates, run{start: start, end: i - 1})
		} else {
			i++
		}
	}

	// Stages 2-3: merge close runs, then discard short closed intervals.
	pulses := make([]Pulse, 0)
	for _, r := range mergeRuns(candidates) {
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

// mergeRuns joins runs separated by at most maxMergeGap non-exceeding samples.
// Runs are expected in ascending index order, which preserves that order.
func mergeRuns(runs []run) []run {
	if len(runs) == 0 {
		return nil
	}
	merged := make([]run, 0, len(runs))
	merged = append(merged, runs[0])
	for _, r := range runs[1:] {
		last := &merged[len(merged)-1]
		if r.start-last.end-1 <= maxMergeGap {
			last.end = r.end
		} else {
			merged = append(merged, r)
		}
	}
	return merged
}

// medianAbs returns the median of the absolute amplitudes. For an even number
// of samples it is the arithmetic mean of the two middle values.
func medianAbs(a []float64) float64 {
	abs := make([]float64, len(a))
	for i, v := range a {
		abs[i] = math.Abs(v)
	}
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
