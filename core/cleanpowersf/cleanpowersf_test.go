package cleanpowersf

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func at(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, laLocation)
}

func TestGenerationRate(t *testing.T) {
	// summer (Aug) TOU, matching the published/bill rates
	assert.Equal(t, 0.24327, GenerationRate(at(2026, time.August, 15, 19))) // peak
	assert.Equal(t, 0.15767, GenerationRate(at(2026, time.August, 15, 15))) // part-peak (3pm)
	assert.Equal(t, 0.15767, GenerationRate(at(2026, time.August, 15, 22))) // part-peak (10pm)
	assert.Equal(t, 0.11871, GenerationRate(at(2026, time.August, 15, 12))) // off-peak

	// winter (January) TOU
	assert.Equal(t, 0.10323, GenerationRate(at(2027, time.January, 15, 19)))
	assert.Equal(t, 0.08598, GenerationRate(at(2027, time.January, 15, 22)))
	assert.Equal(t, 0.07445, GenerationRate(at(2027, time.January, 15, 12)))

	// season boundary: Sep is summer, Oct is winter
	assert.Equal(t, 0.24327, GenerationRate(at(2026, time.September, 30, 19)))
	assert.Equal(t, 0.10323, GenerationRate(at(2026, time.October, 1, 19)))
}

func TestGenerationRateAcrossUTCInput(t *testing.T) {
	// a UTC instant that is 7pm PDT (Aug, UTC-7 -> 02:00 next day UTC) must still
	// resolve to the summer peak rate
	utc := time.Date(2026, time.August, 16, 2, 0, 0, 0, time.UTC)
	assert.Equal(t, 0.24327, GenerationRate(utc))
}

func TestNetSurplusCompensation(t *testing.T) {
	assert.Equal(t, 0.0893, NetSurplusCompensation(at(2026, time.August, 15, 12)))
	// before the earliest effective date, fall back to the earliest known value
	assert.Equal(t, 0.0893, NetSurplusCompensation(at(2023, time.January, 1, 12)))
}

func TestTrueUpMonth(t *testing.T) {
	assert.Equal(t, time.May, TrueUpMonth)
}
