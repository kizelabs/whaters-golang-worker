package session

import (
	"math/rand"
	"time"
)

func JitterDuration(base time.Duration, rng *rand.Rand) time.Duration {
	if base <= 0 {
		return time.Second
	}
	jitter := time.Duration(rng.Int63n(int64(base / 5)))
	return base + jitter
}

func BackoffDuration(base time.Duration, failures int, rng *rand.Rand) time.Duration {
	maxMul := 6
	mul := failures + 1
	if mul > maxMul {
		mul = maxMul
	}
	wait := time.Duration(mul) * base
	jitter := time.Duration(rng.Int63n(int64(base / 3)))
	return wait + jitter
}
