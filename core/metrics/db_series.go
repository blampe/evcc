package metrics

import (
	"time"

	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/tariff"
)

// meterSeries returns the entity's recorded per-slot values since from as an evenly
// sampled series on the 15min grid, with the timestamp of sample 0. Unlike
// energyProfile (which averages by time-of-day, collapsing the calendar), this keeps
// the raw chronological series so periodicities beyond a single day can be recovered.
//
// Missing slots (downtime, excluded recovered slots) are filled with the mean of the
// present slots so the sampling is regular for spectral analysis. It returns
// ErrIncomplete when there is no data (matching energyProfile).
func meterSeries(entity entity, from time.Time) (time.Time, []float64, error) {
	database, err := db.Instance.DB()
	if err != nil {
		return time.Time{}, nil, err
	}

	rows, err := database.Query(
		`SELECT ts, COALESCE(energy, 0) FROM meters
		 WHERE meter = ? AND ts >= ? AND COALESCE(recovered, 0) = 0
		 ORDER BY ts ASC`,
		entity.Id, from.Unix(),
	)
	if err != nil {
		return time.Time{}, nil, err
	}
	defer rows.Close()

	var ts []int64
	var vals []float64
	for rows.Next() {
		var t int64
		var v float64
		if err := rows.Scan(&t, &v); err != nil {
			return time.Time{}, nil, err
		}
		ts = append(ts, t)
		vals = append(vals, v)
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, nil, err
	}
	if len(ts) == 0 {
		return time.Time{}, nil, ErrIncomplete
	}

	step := int64(tariff.SlotDuration.Seconds())
	start := ts[0]
	n := int((ts[len(ts)-1]-start)/step) + 1

	series := make([]float64, n)
	present := make([]bool, n)
	var sum float64
	var count int
	for i, t := range ts {
		idx := int((t - start) / step)
		if idx >= 0 && idx < n {
			series[idx] = vals[i]
			present[idx] = true
			sum += vals[i]
			count++
		}
	}

	// fill gaps with the mean of the present slots so the grid is regular
	mean := sum / float64(count)
	for i := range series {
		if !present[i] {
			series[i] = mean
		}
	}

	return time.Unix(start, 0), series, nil
}
