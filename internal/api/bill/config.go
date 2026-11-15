package bill

import (
	"os"
	"time"
)

const (
	defaultTemporalAddress = "localhost:7233"
	// defaultCloseWait bounds how long a close waits for the total; the rate lookup normally
	// takes milliseconds.
	defaultCloseWait = 10 * time.Second
)

// temporalAddress honours TEMPORAL_ADDRESS, the variable the Temporal CLI also reads.
func temporalAddress() string {
	if addr := os.Getenv("TEMPORAL_ADDRESS"); addr != "" {
		return addr
	}

	return defaultTemporalAddress
}
