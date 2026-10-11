package main

// percentileFromHistogram computes the p50, p95, and p99 latencies from a
// cumulative histogram. The histogram is a slice of counts where index i
// holds the number of observations at or below boundaries[i], and the last
// slot is the +Inf bucket (everything above the largest finite boundary).
//
// The computation is linear interpolation between the two bucket boundaries
// that bracket the target rank, which is the standard method for histogram
// percentile estimation and the same one Prometheus' histogram_quantile
// uses. It is exact for observations that land exactly on a boundary and
// approximate for those that fall within a bucket — the approximation is
// the price of storing counts rather than every observation, and it is
// good enough for a dashboard: the alternative (storing every latency) is
// not bounded.
//
// The boundaries are passed in rather than read from the package-level
// variable so the function is testable with arbitrary bucket shapes: a
// caller that wants p99.9 or a different set of boundaries can pass them
// directly. The dashboard always passes attemptLatencyBounds.
//
// Returns zero values when the histogram is empty (total count 0), which
// the caller suppresses with omitempty rather than rendering a misleading
// "0ms" for an endpoint that never answered.
func percentileFromHistogram(hist []int64, boundaries []float64) latencyPercentiles {
	if len(hist) == 0 || len(boundaries) == 0 {
		return latencyPercentiles{}
	}

	// The histogram has one more slot than boundaries: the last slot is the
	// +Inf bucket. If they disagree the caller built the histogram wrong;
	// rather than guess, treat it as empty so the result is zero values
	// rather than a panic or a nonsense number.
	if len(hist) != len(boundaries)+1 {
		return latencyPercentiles{}
	}

	total := int64(0)
	for _, c := range hist {
		total += c
	}
	if total == 0 {
		return latencyPercentiles{}
	}

	// The cumulative distribution: cum[i] is the count of observations at or
	// below boundaries[i]. The +Inf bucket is cum[len(hist)-1] == total.
	cum := make([]int64, len(hist))
	cum[0] = hist[0]
	for i := 1; i < len(hist); i++ {
		cum[i] = cum[i-1] + hist[i]
	}

	// percentileAt returns the latency in seconds at which `pct`% of
	// observations fall at or below it, by linear interpolation between the
	// two boundaries that bracket the target rank.
	percentileAt := func(pct float64) float64 {
		rank := float64(total) * pct
		// Find the first bucket whose cumulative count reaches or exceeds
		// the target rank. The +Inf bucket is the catch-all; if the rank
		// lands in it we return the largest finite boundary, which is the
		// honest answer: we know the observation was > boundary, but not
		// by how much.
		for i := 0; i < len(cum); i++ {
			if float64(cum[i]) >= rank {
				if i == 0 {
					// Everything is in the first bucket: the
					// observation is at or below boundaries[0].
					// Interpolate between 0 and boundaries[0].
					return boundaries[0] * (rank / float64(cum[i]))
				}
				// The last slot is the +Inf bucket: it has no upper
				// boundary to interpolate toward. The observation
				// exceeded the largest finite boundary and the
				// histogram does not say by how much, so that
				// boundary is the honest answer. Indexing
				// boundaries[i] here would run one past the end of
				// the boundary array.
				if i == len(cum)-1 {
					return boundaries[len(boundaries)-1]
				}
				// Interpolate between boundaries[i-1] and boundaries[i].
				// The fraction is how far into this bucket the rank
				// falls, as a share of the bucket's own count.
				frac := (rank - float64(cum[i-1])) / float64(cum[i]-cum[i-1])
				return boundaries[i-1] + (boundaries[i]-boundaries[i-1])*frac
			}
		}
		// The rank is in the +Inf bucket (the last slot). Return the
		// largest finite boundary: we know the observation exceeded it,
		// but the histogram does not say by how much.
		return boundaries[len(boundaries)-1]
	}

	return latencyPercentiles{
		P50: percentileAt(0.50) * 1000, // seconds → milliseconds
		P95: percentileAt(0.95) * 1000,
		P99: percentileAt(0.99) * 1000,
	}
}
