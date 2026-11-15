package bill

import "os"

const defaultTemporalAddress = "localhost:7233"

// temporalAddress honours TEMPORAL_ADDRESS, the variable the Temporal CLI also reads.
func temporalAddress() string {
	if addr := os.Getenv("TEMPORAL_ADDRESS"); addr != "" {
		return addr
	}

	return defaultTemporalAddress
}
