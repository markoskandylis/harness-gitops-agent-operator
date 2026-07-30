package projectmapping

import (
	"testing"
	"time"
)

func TestValidateMappingIntervals(t *testing.T) {
	tests := []struct {
		name          string
		pendingRetry  time.Duration
		harnessResync time.Duration
		wantErr       bool
	}{
		{
			name:          "defaults",
			pendingRetry:  DefaultAppProjectPendingRetryInterval,
			harnessResync: DefaultHarnessMappingResyncInterval,
		},
		{
			name:          "minimum",
			pendingRetry:  MinimumMappingInterval,
			harnessResync: MinimumMappingInterval,
		},
		{
			name:          "pending retry below minimum",
			pendingRetry:  MinimumMappingInterval - time.Millisecond,
			harnessResync: DefaultHarnessMappingResyncInterval,
			wantErr:       true,
		},
		{
			name:          "resync below minimum",
			pendingRetry:  DefaultAppProjectPendingRetryInterval,
			harnessResync: 0,
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMappingIntervals(tt.pendingRetry, tt.harnessResync)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateMappingIntervals() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}
