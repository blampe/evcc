//go:build realdata

package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/server/db"
	"github.com/stretchr/testify/require"
)

// TestRealDataConsumptionForecast exercises the full production path against the live
// database: load a loadpoint's sessions, calibrate odometer->SoC, build away-episodes,
// fit the forecast and predict a few away-windows. Run:
//
//	go test ./core -tags realdata -run TestRealDataConsumptionForecast -v
func TestRealDataConsumptionForecast(t *testing.T) {
	src := os.Getenv("EVCC_DB")
	if src == "" {
		home, _ := os.UserHomeDir()
		src = filepath.Join(home, ".evcc", "evcc.db")
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("no database at %s (set EVCC_DB)", src)
	}
	tmp := filepath.Join(t.TempDir(), "evcc.db")
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tmp, data, 0o644))
	require.NoError(t, db.NewInstance("sqlite", tmp))

	rows := vehicleSessions("Home", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NotEmpty(t, rows, "no sessions for loadpoint (check the title)")

	socPerKm := defaultWhPerKm / (66.5 * 1000) * 100
	eps := awayEpisodes(rows, socPerKm, calibrateSocPerKwh(rows, 66.5))
	fc := metrics.NewConsumptionForecast(eps)
	require.NotNil(t, fc, "not enough episodes for a forecast")

	// a Monday in the DB's local time so time-of-day indexing matches the stored sessions
	loc := rows[len(rows)-1].Created.Location()
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, loc)
	full := fc.Drain(day, day.AddDate(0, 0, 1))
	commute := fc.Drain(day.Add(8*time.Hour), day.Add(18*time.Hour))
	short := fc.Drain(day.Add(8*time.Hour), day.Add(12*time.Hour))

	t.Logf("sessions=%d  socPerKm=%.3f  episodes=%d  loc=%s", len(rows), socPerKm, len(eps), loc)
	t.Logf("predicted drain  full-day = %4.1f   08:00-18:00 = %4.1f   08:00-12:00 = %4.1f SoC", full, commute, short)

	require.Greater(t, commute, short, "a longer away-window drains more")
	require.Greater(t, short, 0.0)
	require.Less(t, commute, 50.0, "prediction should be a plausible daily drain")
}
