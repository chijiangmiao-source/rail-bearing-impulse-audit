package pulse

import "sort"

// PairingTolerance bounds a peak-time-difference tolerance measured in
// samples; zero is a legal tolerance (peaks must carry the same index).
const (
	MinToleranceSamples = 0
	MaxToleranceSamples = 100
)

// ChannelInput is one rail-side picker's sampling inside a dual-channel
// review. The single-channel contract applies to it unchanged: finite
// amplitudes within [MinSamples, MaxSamples], excluded ranges validated
// with ValidateRanges.
type ChannelInput struct {
	SampleRate     float64
	Amplitudes     []float64
	ExcludedRanges []Range
}

// CorrelationRequest is one dual-channel review. SampleRate is the common
// sampling rate (both pickers must sample at the same rate) and
// ToleranceSamples the inclusive peak-time-difference tolerance. The
// request is expected to have passed the API layer's validation first:
// per-channel validation exactly as a single-channel request, equal
// sample rates and an integer tolerance in
// [MinToleranceSamples, MaxToleranceSamples].
type CorrelationRequest struct {
	SampleRate       float64
	ToleranceSamples int
	Left             ChannelInput
	Right            ChannelInput
}

// Pair is one one-to-one piece of evidence: the index of the paired pulse
// inside each channel's pulse list, together with the absolute peak-index
// difference in samples.
type Pair struct {
	LeftPulseIndex  int `json:"left_pulse_index"`
	RightPulseIndex int `json:"right_pulse_index"`
	TimeDifference  int `json:"time_difference_samples"`
}

// CorrelationResult keeps both channels' original analyses and adds the
// pairing outcome. Every slice is never nil; pairs are ordered by
// ascending left peak time (left_pulse_index) and the unpaired lists are
// ordered by ascending pulse index.
type CorrelationResult struct {
	Left          Result `json:"left"`
	Right         Result `json:"right"`
	Pairs         []Pair `json:"pairs"`
	LeftUnpaired  []int  `json:"left_unpaired"`
	RightUnpaired []int  `json:"right_unpaired"`
}

// Correlate runs pulse.Analyze on each channel with that channel's own
// amplitudes and excluded ranges, then performs the one-to-one
// correlation.
//
// Evidence is only produced when both channels are decidable. Candidate
// pairs whose absolute peak-index difference is at most ToleranceSamples
// are considered in ascending order of (time difference, left peak
// index, right peak index); indexing the start-ordered pulse lists is
// equivalent to ordering by peak index because retained intervals are
// disjoint, so pulse i's peak always precedes pulse i+1's. The first
// candidate that can claim both pulses wins and every later candidate
// touching an occupied pulse is skipped. Greedy selection in that order
// yields one unique result even when several candidates fall inside the
// tolerance or pulse counts differ.
func Correlate(req CorrelationRequest) CorrelationResult {
	left := Analyze(req.Left.Amplitudes, req.Left.ExcludedRanges...)
	right := Analyze(req.Right.Amplitudes, req.Right.ExcludedRanges...)

	out := CorrelationResult{
		Left:          left,
		Right:         right,
		Pairs:         []Pair{},
		LeftUnpaired:  []int{},
		RightUnpaired: []int{},
	}
	if !left.Decidable || !right.Decidable {
		// No evidence when either side cannot be judged; every pulse index
		// of a decidable side is reported as unpaired.
		for i := range left.Pulses {
			out.LeftUnpaired = append(out.LeftUnpaired, i)
		}
		for i := range right.Pulses {
			out.RightUnpaired = append(out.RightUnpaired, i)
		}
		return out
	}

	candidates := make([]candidate, 0, len(left.Pulses)*len(right.Pulses))
	for li, lp := range left.Pulses {
		for ri, rp := range right.Pulses {
			diff := lp.PeakIndex - rp.PeakIndex
			if diff < 0 {
				diff = -diff
			}
			if diff <= req.ToleranceSamples {
				candidates = append(candidates, candidate{
					left: li, right: ri, diff: diff,
				})
			}
		}
	}
	// Fixed tie-breaking is what makes the pairing unique when several
	// candidates fall inside the tolerance.
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.diff != b.diff {
			return a.diff < b.diff
		}
		if a.left != b.left {
			return a.left < b.left
		}
		return a.right < b.right
	})

	leftTaken := make([]bool, len(left.Pulses))
	rightTaken := make([]bool, len(right.Pulses))
	selected := make([]candidate, 0)
	for _, cand := range candidates {
		// A pulse participates in at most one pair; later candidates whose
		// peer was already claimed are skipped.
		if leftTaken[cand.left] || rightTaken[cand.right] {
			continue
		}
		leftTaken[cand.left] = true
		rightTaken[cand.right] = true
		selected = append(selected, cand)
	}

	// Evidence is reported in fixed peak-time order (ascending left peak),
	// independent of the greedy selection order.
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].left != selected[j].left {
			return selected[i].left < selected[j].left
		}
		return selected[i].right < selected[j].right
	})
	for _, cand := range selected {
		out.Pairs = append(out.Pairs, Pair{
			LeftPulseIndex:  cand.left,
			RightPulseIndex: cand.right,
			TimeDifference:  cand.diff,
		})
	}

	for i, taken := range leftTaken {
		if !taken {
			out.LeftUnpaired = append(out.LeftUnpaired, i)
		}
	}
	for i, taken := range rightTaken {
		if !taken {
			out.RightUnpaired = append(out.RightUnpaired, i)
		}
	}
	return out
}

type candidate struct {
	left  int
	right int
	diff  int
}
