package stats

import "math"

// ChangePoint represents a detected shift in a time series.
type ChangePoint struct {
	Index      int     // position in the series where the change was detected
	BeforeMean float64 // mean of values before the change
	AfterMean  float64 // mean of values after the change
	PctChange  float64 // percentage change (positive = slowdown, negative = speedup)
	Direction  string  // "slowdown" or "speedup"
}

// slackMultiplier controls the CUSUM slack parameter (k = slackMultiplier * stddev).
// Standard value for detecting mean shifts of ~1 stddev.
const slackMultiplier = 0.5

// CUSUMDetect runs two-sided CUSUM (Cumulative Sum) change-point detection.
// It uses adaptive thresholds based on the local coefficient of variation:
//   - slack (k) = 0.5 * stddev of baseline
//   - threshold (h) = thresholdMultiplier * stddev of baseline
//
// minSegment controls the minimum number of points between detected change points
// and at the start/end of the series.
func CUSUMDetect(data []float64, thresholdMultiplier float64, minSegment int) []ChangePoint {
	n := len(data)
	if n < 2*minSegment {
		return nil
	}

	// Use the first minSegment points as baseline
	baseline := data[:minSegment]
	mu := Mean(baseline)
	sigma := Stddev(baseline)

	if sigma == 0 {
		// If baseline has no variance, use full-series stddev
		sigma = Stddev(data)
		if sigma == 0 {
			return nil
		}
	}

	k := slackMultiplier * sigma     // slack parameter
	h := thresholdMultiplier * sigma // decision threshold

	var points []ChangePoint
	sHigh := 0.0 // detects upward shifts (slowdowns)
	sLow := 0.0  // detects downward shifts (speedups)
	lastChangeIdx := 0

	for i := minSegment; i < n; i++ {
		sHigh = math.Max(0, sHigh+(data[i]-mu-k))
		sLow = math.Max(0, sLow+(mu-data[i]-k))

		if (sHigh > h || sLow > h) && (i-lastChangeIdx) >= minSegment {
			// The alarm lags the true shift: the cumulative statistic needs
			// several samples to cross h. Walk back over the consecutive
			// shifted-side points immediately before the alarm so the
			// reported index is the change onset — commit attribution and
			// segment splits key off it. (Consecutive-only, so a slow noise
			// drift far into the baseline cannot drag the onset with it.)
			onset := i
			if sHigh > h {
				for onset > lastChangeIdx+1 && data[onset-1] > mu+k {
					onset--
				}
			} else {
				for onset > lastChangeIdx+1 && data[onset-1] < mu-k {
					onset--
				}
			}

			// Compute before/after means around the onset
			beforeMean := Mean(data[lastChangeIdx:onset])
			afterEnd := min(onset+minSegment, n)
			afterMean := Mean(data[onset:afterEnd])

			pctChange := 0.0
			if beforeMean != 0 {
				pctChange = (afterMean - beforeMean) / beforeMean * 100
			}

			direction := "slowdown"
			if pctChange < 0 {
				direction = "speedup"
			}

			points = append(points, ChangePoint{
				Index:      onset,
				BeforeMean: beforeMean,
				AfterMean:  afterMean,
				PctChange:  pctChange,
				Direction:  direction,
			})

			// Reset CUSUM and update baseline
			sHigh = 0
			sLow = 0
			mu = afterMean
			sigma = Stddev(data[onset:afterEnd])
			if sigma == 0 {
				sigma = Stddev(data)
			}
			k = 0.5 * sigma
			h = thresholdMultiplier * sigma
			lastChangeIdx = i
		}
	}

	return points
}
