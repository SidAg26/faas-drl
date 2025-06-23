package plugin

import "math/rand"

// ScorePodAlternative calculates the score for an alternative pod version
func ScorePodAlternative(memory, requestedMemory, cpu, requestedCPU uint64) uint64 {
	return abs(memory, requestedMemory) + abs(cpu, requestedCPU)
}

// ScoreColdStart calculates the cold start score based on a version score and a randomization factor
func ScoreColdStart(versionScore uint64) uint64 {
	lower := uint64(float64(versionScore) * 0.8)
	upper := max(uint64(float64(versionScore)*1.2), lower)
	score := lower
	if upper > lower {
		score = lower + uint64(rand.Int63n(int64(upper-lower+1)))
	}
	return score
}

// abs returns the absolute difference between two uint64 values
func abs(x, y uint64) uint64 {
	if x > y {
		return x - y
	}
	return y - x
}

// max returns the maximum of two uint64 values
func max(x, y uint64) uint64 {
	if x > y {
		return x
	}
	return y
}
