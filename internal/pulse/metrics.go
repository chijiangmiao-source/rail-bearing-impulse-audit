package pulse

import "math"

// AnalyzeWithMetrics runs the same detection pipeline as Analyze and
// additionally attaches DurationMS and RMSAmplitude to every retained
// pulse, derived from the requested sample rate. Undecidable results are
// returned unchanged: the empty pulse list carries no fabricated metrics.
func AnalyzeWithMetrics(amplitudes []float64, sampleRate float64, excludedRanges ...Range) Result {
	r := Analyze(amplitudes, excludedRanges...)
	for i := range r.Pulses {
		p := &r.Pulses[i]
		duration := durationMS(p.End-p.Start+1, sampleRate)
		rms := rmsAmplitude(amplitudes, p.Start, p.End)
		p.DurationMS = &duration
		p.RMSAmplitude = &rms
	}
	return r
}

// durationMS converts a closed-interval sample count to milliseconds at the
// given sample rate: samples/rate seconds, then scaled to milliseconds.
func durationMS(samples int, sampleRate float64) float64 {
	return float64(samples) / sampleRate * 1000
}

// rmsAmplitude returns the root-mean-square amplitude over every sample of
// the closed interval [start, end]. The scaled sum-of-squares divides each
// sample by the interval's maximum magnitude before squaring, so finite
// amplitudes near the float64 ceiling cannot overflow to +Inf in
// intermediate squares.
func rmsAmplitude(amplitudes []float64, start, end int) float64 {
	scale := 0.0
	for j := start; j <= end; j++ {
		if v := math.Abs(amplitudes[j]); v > scale {
			scale = v
		}
	}
	if scale == 0 {
		return 0
	}
	sum := 0.0
	for j := start; j <= end; j++ {
		r := amplitudes[j] / scale
		sum += r * r
	}
	return scale * math.Sqrt(sum/float64(end-start+1))
}
