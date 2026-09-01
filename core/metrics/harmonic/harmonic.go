// Package harmonic fits and evaluates least-squares harmonic (Fourier) models over
// evenly sampled time series, discovering their periodicities from the signal's own
// spectrum rather than assuming them.
//
// Time is treated as absolute (monotonic): a detected "24h" period is a fixed
// 24-hour duration, not a civil day. Human rhythms are civil-time-locked, so across a
// daylight-saving transition (a 23h or 25h civil day) the daily phase slips by an
// hour relative to a fixed-24h sinusoid. The models are therefore best used for
// multi-day cycles (weekly, biweekly, ...), where a ±1h DST wobble is a negligible
// fraction of the period; the dominant daily cycle is better carried by a civil-time
// (local time-of-day) seasonal average - which is DST-correct by construction - with
// a harmonic model of the residual on top.
package harmonic

import (
	"math"
	"slices"
	"sort"
	"time"
)

// Model is a least-squares harmonic (Fourier) model fitted to an evenly
// sampled series. It captures whatever periodicities the data actually contains -
// detected from the signal's own spectrum rather than assumed - and evaluates them
// at arbitrary (including future) times. So it reflects emergent cycles like a
// weekly commute or an every-other-Thursday absence without any of them being
// hard-coded.
//
// The method is standard spectral analysis: a Lomb-Scargle/Schuster periodogram
// locates the dominant frequency; its sinusoid is fitted and subtracted (CLEAN /
// pre-whitening); the search repeats on the residual until no peak is statistically
// significant (false-alarm probability above a threshold). The selected frequencies
// are then refit jointly by ordinary least squares. See Fit.
type Model struct {
	start time.Time     // sample index 0 is the slot starting at start
	slot  time.Duration // sample spacing
	mean  float64
	comps []harmonicComp
}

// harmonicComp is one fitted sinusoid: value += cos·cos(2πf·i) + sin·sin(2πf·i),
// with f in cycles per sample and i the (fractional) sample index from start.
type harmonicComp struct {
	freq float64 // cycles per sample
	cos  float64
	sin  float64
}

// Options tunes detection. Zero values fall back to the defaults below.
type Options struct {
	MinPeriod     time.Duration // ignore cycles shorter than this (default 2h)
	MaxPeriod     time.Duration // ignore cycles longer than this (default span/3)
	Oversample    int           // frequency-grid oversampling factor (default 5)
	MaxComponents int           // cap on fitted sinusoids (default 12)
	FAP           float64       // false-alarm probability threshold (default 0.01)
	// Validate, when >0, selects cycles by out-of-sample predictive gain instead of
	// statistical significance: the last Validate fraction of the series is held out
	// and a cycle is kept only while it improves the forecast of that tail. This
	// rejects lines that are significant in a long, noisy record but do not actually
	// predict (see the home-load backtest). 0 keeps the plain significance test.
	Validate float64
}

func (o *Options) withDefaults(span time.Duration) {
	if o.MinPeriod <= 0 {
		o.MinPeriod = 2 * time.Hour
	}
	if o.Oversample <= 0 {
		o.Oversample = 5
	}
	if o.MaxComponents <= 0 {
		o.MaxComponents = 12
	}
	if o.FAP <= 0 {
		o.FAP = 0.01
	}
	// a period is only trustworthy with a few cycles in the record
	if lim := span / 3; o.MaxPeriod <= 0 || o.MaxPeriod > lim {
		o.MaxPeriod = lim
	}
}

// Fit detects the significant periodicities in the evenly sampled series y
// (sample i is the slot starting at start + i·slot) and returns a model that
// predicts the series at any time. Gaps must already be filled (e.g. with the mean)
// so the sampling is regular. With too little data or no significant cycle it
// returns a mean-only model, which At still evaluates.
func Fit(start time.Time, slot time.Duration, y []float64, opt Options) *Model {
	n := len(y)
	m := &Model{start: start, slot: slot}
	if n == 0 || slot <= 0 {
		return m
	}

	var sum float64
	for _, v := range y {
		sum += v
	}
	m.mean = sum / float64(n)

	opt.withDefaults(slot * time.Duration(n))

	pMin := opt.MinPeriod.Seconds() / slot.Seconds() // samples
	pMax := opt.MaxPeriod.Seconds() / slot.Seconds()
	if pMax <= pMin || pMax < 2 {
		return m // not enough span for a meaningful search
	}

	// mean-removed series
	detr := make([]float64, n)
	for i, v := range y {
		detr[i] = v - m.mean
	}

	// frequency grid (cycles/sample), oversampled beyond the natural 1/n spacing
	fMin, fMax := 1/pMax, 1/pMin
	df := 1 / (float64(opt.Oversample) * float64(n))

	// pick the frequencies to keep, then refit their amplitudes jointly on all the data
	var selected []float64
	if opt.Validate > 0 {
		selected = selectCV(detr, fMin, fMax, df, opt)
	} else {
		selected = selectFAP(detr, fMin, fMax, df, opt)
	}
	if len(selected) == 0 {
		return m
	}

	m.comps = jointRefit(detr, selected)
	return m
}

// selectFAP greedily selects frequencies whose periodogram peak is statistically
// significant (false-alarm probability below opt.FAP), pre-whitening the residual
// between picks.
func selectFAP(detr []float64, fMin, fMax, df float64, opt Options) []float64 {
	n := len(detr)
	basis := buildBasis(fMin, fMax, df, n)
	if len(basis) == 0 {
		return nil
	}
	// multiple-testing correction uses the count of independent (natural) frequencies
	indep := math.Max(1, float64(n)*(fMax-fMin))
	zThresh := math.Log(indep / opt.FAP)

	res := slices.Clone(detr)
	var selected []float64
	for range opt.MaxComponents {
		best, ok := nextPeak(basis, res, selected, df, zThresh)
		if !ok {
			break
		}
		subtractSinusoid(res, best)
		selected = append(selected, best.freq)
	}
	return selected
}

// minCVTrain is the fewest training samples cross-validated selection will split a
// holdout from; below this the validation tail is too small to be informative and no
// cycles are selected (the caller falls back to the mean).
const minCVTrain = 16

// selectCV greedily selects frequencies that reduce the forecast error on a held-out
// tail (the last opt.Validate fraction of the series), stopping as soon as a further
// cycle no longer improves the out-of-sample fit. This keeps genuinely predictive
// cycles and rejects lines that are merely significant in a long, noisy record.
func selectCV(detr []float64, fMin, fMax, df float64, opt Options) []float64 {
	n := len(detr)
	nt := int(float64(n) * (1 - opt.Validate))
	if nt < minCVTrain || nt >= n {
		return nil
	}
	basis := buildBasis(fMin, fMax, df, nt) // periodogram over the training part only
	if len(basis) == 0 {
		return nil
	}
	indep := math.Max(1, float64(nt)*(fMax-fMin))
	zThresh := math.Log(indep / opt.FAP)

	train := detr[:nt]
	val := detr[nt:]
	res := slices.Clone(train)

	// running validation prediction; starts at 0 (the mean, since detr is mean-removed).
	// cand is the reused scratch for the tentative prediction with one more cycle.
	valPred := make([]float64, len(val))
	cand := make([]float64, len(val))
	bestErr := sse(valPred, val)

	var selected []float64
	for range opt.MaxComponents {
		best, ok := nextPeak(basis, res, selected, df, zThresh)
		if !ok {
			break
		}

		// forecast this sinusoid across the validation tail (indices continue past nt)
		w := 2 * math.Pi * best.freq
		for j := range val {
			idx := float64(nt + j)
			cand[j] = valPred[j] + best.a*math.Cos(w*idx) + best.b*math.Sin(w*idx)
		}
		candErr := sse(cand, val)
		if candErr >= bestErr {
			break // no out-of-sample gain from this cycle: stop
		}

		bestErr = candErr
		copy(valPred, cand)
		subtractSinusoid(res, best)
		selected = append(selected, best.freq)
	}
	return selected
}

// nextPeak returns the next significant, well-separated periodogram peak of res, or
// ok=false when the strongest peak is insignificant (false-alarm probability above
// the threshold) or a leakage neighbour of a line already taken.
func nextPeak(basis []freqBasis, res []float64, selected []float64, df, zThresh float64) (peak, bool) {
	var s2 float64
	for _, v := range res {
		s2 += v * v
	}
	s2 /= float64(len(res))
	if s2 <= 0 {
		return peak{}, false
	}

	best := peakPower(basis, res)
	// normalized Lomb-Scargle power ~ Exp(1) under white noise
	if best.power/(2*s2) < zThresh {
		return peak{}, false
	}
	if tooClose(selected, best.freq, df) {
		return peak{}, false
	}
	return best, true
}

// sse returns the sum of squared errors between a prediction and the target.
func sse(pred, target []float64) float64 {
	var e float64
	for i := range target {
		d := pred[i] - target[i]
		e += d * d
	}
	return e
}

// At evaluates the model at time t, which may be in the future. Only the phase
// relative to the model start matters, so times outside the fitted span are fine.
func (m *Model) At(t time.Time) float64 {
	i := t.Sub(m.start).Seconds() / m.slot.Seconds()
	v := m.mean
	for _, c := range m.comps {
		w := 2 * math.Pi * c.freq * i
		v += c.cos*math.Cos(w) + c.sin*math.Sin(w)
	}
	return v
}

// Periods returns the detected periods, longest first, for logging and tests.
// Harmonics of a non-sinusoidal cycle appear as their own shorter-period entries.
func (m *Model) Periods() []time.Duration {
	res := make([]time.Duration, len(m.comps))
	for i, c := range m.comps {
		res[i] = time.Duration(float64(m.slot) / c.freq)
	}
	sort.Slice(res, func(a, b int) bool { return res[a] > res[b] })
	return res
}

// freqBasis holds a trial frequency and the sums that depend only on it and n:
// cw, sw drive the cos/sin recurrence, cc and ss normalise the periodogram.
type freqBasis struct {
	freq, cw, sw, cc, ss float64
}

// buildBasis precomputes the per-frequency constants for the grid [fMin, fMax].
func buildBasis(fMin, fMax, df float64, n int) []freqBasis {
	count := int((fMax-fMin)/df) + 1
	if count < 1 {
		return nil
	}
	basis := make([]freqBasis, 0, count)
	for k := range count {
		f := fMin + float64(k)*df
		w := 2 * math.Pi * f
		cw, sw := math.Cos(w), math.Sin(w)
		// accumulate cc=Σcos², ss=Σsin² via the same recurrence used for the fit
		c, s := 1.0, 0.0
		var cc, ss float64
		for range n {
			cc += c * c
			ss += s * s
			c, s = c*cw-s*sw, s*cw+c*sw
		}
		if cc > 0 && ss > 0 {
			basis = append(basis, freqBasis{f, cw, sw, cc, ss})
		}
	}
	return basis
}

// peak is the best frequency found in one periodogram pass.
type peak struct {
	freq, power, a, b float64
}

// peakPower scans the grid for the frequency of maximum periodogram power against
// the current residual, returning its least-squares sinusoid amplitudes (a·cos +
// b·sin). The cos/sin values are generated by an angle-addition recurrence so the
// inner loop is free of trig calls (a Goertzel-style evaluation).
func peakPower(basis []freqBasis, res []float64) peak {
	var best peak
	for _, fb := range basis {
		c, s := 1.0, 0.0
		var sumC, sumS float64
		for i := range res {
			sumC += res[i] * c
			sumS += res[i] * s
			c, s = c*fb.cw-s*fb.sw, s*fb.cw+c*fb.sw
		}
		// RSS reduction from fitting a sinusoid at this frequency
		power := sumC*sumC/fb.cc + sumS*sumS/fb.ss
		if power > best.power {
			best = peak{fb.freq, power, sumC / fb.cc, sumS / fb.ss}
		}
	}
	return best
}

// subtractSinusoid pre-whitens the residual by removing the fitted peak.
func subtractSinusoid(res []float64, p peak) {
	cw, sw := math.Cos(2*math.Pi*p.freq), math.Sin(2*math.Pi*p.freq)
	c, s := 1.0, 0.0
	for i := range res {
		res[i] -= p.a*c + p.b*s
		c, s = c*cw-s*sw, s*cw+c*sw
	}
}

// tooClose reports whether f is within one grid step of an already-selected
// frequency, i.e. spectral leakage from the same line rather than a new cycle.
func tooClose(selected []float64, f, df float64) bool {
	for _, g := range selected {
		if math.Abs(g-f) < 1.5*df {
			return true
		}
	}
	return false
}

// jointRefit fits cos/sin amplitudes for all selected frequencies together against
// the mean-removed series by ordinary least squares. Fitting jointly (rather than
// keeping the sequential CLEAN estimates) corrects for the non-orthogonality of
// off-grid frequencies.
func jointRefit(detr []float64, freqs []float64) []harmonicComp {
	n := len(detr)
	p := 2 * len(freqs)

	// design columns per frequency: cos then sin, via the recurrence
	cols := make([][]float64, p)
	for k, f := range freqs {
		cw, sw := math.Cos(2*math.Pi*f), math.Sin(2*math.Pi*f)
		cosCol := make([]float64, n)
		sinCol := make([]float64, n)
		c, s := 1.0, 0.0
		for i := range n {
			cosCol[i], sinCol[i] = c, s
			c, s = c*cw-s*sw, s*cw+c*sw
		}
		cols[2*k], cols[2*k+1] = cosCol, sinCol
	}

	// normal equations XᵀX β = Xᵀd
	xtx := make([][]float64, p)
	for i := range p {
		xtx[i] = make([]float64, p)
	}
	xtd := make([]float64, p)
	for a := range p {
		for b := a; b < p; b++ {
			var v float64
			for i := range n {
				v += cols[a][i] * cols[b][i]
			}
			xtx[a][b], xtx[b][a] = v, v
		}
		var v float64
		for i := range n {
			v += cols[a][i] * detr[i]
		}
		xtd[a] = v
	}

	beta := choleskySolve(xtx, xtd)
	comps := make([]harmonicComp, len(freqs))
	for k, f := range freqs {
		comps[k] = harmonicComp{freq: f, cos: beta[2*k], sin: beta[2*k+1]}
	}
	return comps
}

// choleskySolve solves the symmetric positive-definite system Aβ = b with a small
// ridge on the diagonal to stay well-conditioned when frequencies are near-collinear.
func choleskySolve(a [][]float64, b []float64) []float64 {
	n := len(b)

	var maxDiag float64
	for i := range n {
		maxDiag = math.Max(maxDiag, a[i][i])
	}
	ridge := 1e-9 * maxDiag

	l := make([][]float64, n)
	for i := range n {
		l[i] = make([]float64, n)
		for j := 0; j <= i; j++ {
			sum := a[i][j]
			if i == j {
				sum += ridge
			}
			for k := range j {
				sum -= l[i][k] * l[j][k]
			}
			if i == j {
				l[i][j] = math.Sqrt(math.Max(sum, ridge))
			} else {
				l[i][j] = sum / l[j][j]
			}
		}
	}

	// forward solve Ly = b
	y := make([]float64, n)
	for i := range n {
		sum := b[i]
		for k := range i {
			sum -= l[i][k] * y[k]
		}
		y[i] = sum / l[i][i]
	}
	// back solve Lᵀβ = y
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		sum := y[i]
		for k := i + 1; k < n; k++ {
			sum -= l[k][i] * x[k]
		}
		x[i] = sum / l[i][i]
	}
	return x
}
