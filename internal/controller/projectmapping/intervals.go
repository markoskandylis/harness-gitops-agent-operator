package projectmapping

import (
	"fmt"
	"time"
)

const (
	DefaultAppProjectPendingRetryInterval = 20 * time.Second
	DefaultHarnessMappingResyncInterval   = 5 * time.Minute
	MinimumMappingInterval                = time.Second
	immediateRequeueInterval              = time.Nanosecond
)

func ValidateMappingIntervals(pendingRetry, harnessResync time.Duration) error {
	if pendingRetry < MinimumMappingInterval {
		return fmt.Errorf(
			"appProject pending retry interval must be at least %s",
			MinimumMappingInterval,
		)
	}
	if harnessResync < MinimumMappingInterval {
		return fmt.Errorf(
			"harness mapping resync interval must be at least %s",
			MinimumMappingInterval,
		)
	}
	return nil
}
