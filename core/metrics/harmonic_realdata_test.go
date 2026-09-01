//go:build realdata

package metrics

import (
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evcc-io/evcc/core/metrics/harmonic"
	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/tariff"
	"github.com/stretchr/testify/require"
)

// TestRealDataHarmonic validates the harmonic detector against a real evcc
// database. It is excluded from normal builds (build tag realdata). Run with:
//
//	go test ./core/metrics -tags realdata -run RealData -v
//	EVCC_DB=/path/to/evcc.db go test ./core/metrics -tags realdata -run RealData -v
//
// It copies the database to a temp file first, so the live database is never
// written (NewInstance runs migrations).
func TestRealDataHarmonic(t *testing.T) {
	src := os.Getenv("EVCC_DB")
	if src == "" {
		home, _ := os.UserHomeDir()
		src = filepath.Join(home, ".evcc", "evcc.db")
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("no database at %s (set EVCC_DB)", src)
	}

	tmp := filepath.Join(t.TempDir(), "evcc.db")
	require.NoError(t, copyFile(src, tmp))
	require.NoError(t, db.NewInstance("sqlite", tmp))

	var e entity
	require.NoError(t, db.Instance.Where(`"group" = ?`, Home).First(&e).Error)

	from := time.Now().AddDate(0, 0, -120) // up to ~4 months of history
	start, series, err := meterSeries(e, from)
	require.NoError(t, err)

	n := len(series)
	t.Logf("home series: %d slots (%.1f days) from %s", n, float64(n)/96, start.Format("2006-01-02 15:04"))

	m := harmonic.Fit(start, tariff.SlotDuration, series, harmonic.Options{})
	t.Logf("detected %d periods (longest first):", len(m.Periods()))
	for _, p := range m.Periods() {
		t.Logf("  %6.2f days  (%.1f h)", p.Hours()/24, p.Hours())
	}

	// production path: the wired Collector.Forecast (DST-correct civil-time seasonal +
	// CV harmonic residual). Confirm it assembles and predicts finite, sane values.
	col, err := NewCollector(Home, Home, Home)
	require.NoError(t, err)
	hf, err := col.Forecast(from)
	require.NoError(t, err)

	slotBase := time.Now().Truncate(tariff.SlotDuration)
	minV, maxV := math.Inf(1), math.Inf(-1)
	for i := range 7 * 96 {
		v := hf.At(slotBase.Add(time.Duration(i) * tariff.SlotDuration))
		require.False(t, math.IsNaN(v) || math.IsInf(v, 0), "finite forecast at slot %d", i)
		minV, maxV = math.Min(minV, v), math.Max(maxV, v)
	}
	t.Logf("Collector.Forecast next-7-day range: %.3f .. %.3f kWh/slot", minV, maxV)

	// holdout backtest: fit on all but the last 7 days, predict them, and compare
	// RMSE to the time-of-day-average baseline (what the optimizer uses today)
	const hold = 7 * 96
	if n <= hold+14*96 {
		t.Skip("not enough history for a 7-day holdout")
	}

	train := series[:n-hold]

	// pure harmonic, significance (FAP) selection
	mFap := harmonic.Fit(start, tariff.SlotDuration, train, harmonic.Options{})
	// pure harmonic, cross-validated selection
	mCV := harmonic.Fit(start, tariff.SlotDuration, train, harmonic.Options{Validate: 0.2})

	// baseline: phase-locked average by time-of-day slot (relative to start) over train
	var todSum, todCnt [96]float64
	for i, v := range train {
		todSum[i%96] += v
		todCnt[i%96]++
	}
	tod := func(i int) float64 { return todSum[i%96] / todCnt[i%96] }

	// hybrid: phase-locked daily average + CV-selected harmonic model of the residual,
	// which only carries the multi-day structure the average is blind to (MinPeriod
	// skips the 24h family the average already removes)
	resid := make([]float64, len(train))
	for i := range train {
		resid[i] = train[i] - tod(i)
	}
	mr := harmonic.Fit(start, tariff.SlotDuration, resid, harmonic.Options{MinPeriod: 36 * time.Hour, Validate: 0.2})
	t.Logf("hybrid residual (multi-day) periods kept: %d", len(mr.Periods()))
	for _, p := range mr.Periods() {
		t.Logf("  %6.2f days", p.Hours()/24)
	}

	rmse := func(pred func(i int, ts time.Time) float64) float64 {
		var e float64
		for i := n - hold; i < n; i++ {
			ts := start.Add(time.Duration(i) * tariff.SlotDuration)
			d := pred(i, ts) - series[i]
			e += d * d
		}
		return math.Sqrt(e / hold)
	}

	base := rmse(func(i int, _ time.Time) float64 { return tod(i) })
	t.Logf("7-day holdout RMSE:")
	t.Logf("  time-of-day-avg (baseline) = %.4f", base)
	t.Logf("  harmonic (FAP)             = %.4f  (%.0f%%)", rmse(func(_ int, ts time.Time) float64 { return mFap.At(ts) }), 100*rmse(func(_ int, ts time.Time) float64 { return mFap.At(ts) })/base)
	t.Logf("  harmonic (CV)              = %.4f  (%.0f%%)", rmse(func(_ int, ts time.Time) float64 { return mCV.At(ts) }), 100*rmse(func(_ int, ts time.Time) float64 { return mCV.At(ts) })/base)
	t.Logf("  hybrid = ToD + CV residual = %.4f  (%.0f%%)", rmse(func(i int, ts time.Time) float64 { return tod(i) + mr.At(ts) }), 100*rmse(func(i int, ts time.Time) float64 { return tod(i) + mr.At(ts) })/base)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
