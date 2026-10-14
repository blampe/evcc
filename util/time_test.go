package util

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetOccurrences(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)

	// Wednesday 2026-10-14 09:00, after today's 07:00
	from := time.Date(2026, 10, 14, 9, 0, 0, 0, loc)
	until := from.Add(4 * 24 * time.Hour)

	res, err := GetOccurrences([]int{2, 3, 4, 5}, "07:00", "America/Los_Angeles", from, until)
	require.NoError(t, err)
	assert.Equal(t, []time.Time{
		time.Date(2026, 10, 15, 7, 0, 0, 0, loc), // Thu
		time.Date(2026, 10, 16, 7, 0, 0, 0, loc), // Fri
	}, res)

	// across the DST change the wall-clock time is kept
	from = time.Date(2026, 10, 31, 9, 0, 0, 0, loc)
	res, err = GetOccurrences([]int{0}, "07:00", "America/Los_Angeles", from, from.Add(2*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, []time.Time{time.Date(2026, 11, 1, 7, 0, 0, 0, loc)}, res)

	_, err = GetOccurrences([]int{1}, "7am", "America/Los_Angeles", from, until)
	assert.Error(t, err)
}
