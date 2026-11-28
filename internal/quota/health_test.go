package quota

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The freshness rule is shared by the routing selector and the status API, so
// it is tested once here: a future-dated observation is as untrustworthy as an
// old one, and neither is actionable.
func TestClassifyHealth(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		observedAt time.Time
		maxAge     time.Duration
		want       HealthFreshness
	}{
		{"current", now.Add(-time.Minute), time.Hour, HealthFresh},
		{"exactly at the bound", now.Add(-time.Hour), time.Hour, HealthFresh},
		{"older than the bound", now.Add(-2 * time.Hour), time.Hour, HealthStale},
		{"dated in the future", now.Add(time.Minute), time.Hour, HealthStale},
		{"never observed", time.Time{}, time.Hour, HealthNotObserved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ClassifyHealth(tt.observedAt, now, tt.maxAge))
		})
	}
}
