package warmup

import "math"

// FitQuadratic fits y = a + b*x + c*x² to the given data points
// using ordinary least squares. Solves the 3×3 normal equations
// via Gaussian elimination.
func FitQuadratic(xs, ys []float64) QuadModel {
	n := len(xs)
	if n < 3 {
		return FitLinear(xs, ys)
	}

	// Build the normal equations: M × [a, b, c]^T = rhs
	// where M[i][j] = Σ x^(i+j) and rhs[i] = Σ x^i × y
	var s [5]float64 // s[k] = Σ x^k
	var r [3]float64 // r[k] = Σ x^k × y
	for i := 0; i < n; i++ {
		x, y := xs[i], ys[i]
		xp := 1.0
		for k := 0; k < 5; k++ {
			s[k] += xp
			if k < 3 {
				r[k] += xp * y
			}
			xp *= x
		}
	}

	// 3×3 augmented matrix [M | rhs]
	m := [3][4]float64{
		{s[0], s[1], s[2], r[0]},
		{s[1], s[2], s[3], r[1]},
		{s[2], s[3], s[4], r[2]},
	}

	return solveQuad(m)
}

// FitLinear fits y = a + b*x via OLS. Returns a QuadModel with C=0.
func FitLinear(xs, ys []float64) QuadModel {
	n := len(xs)
	if n == 0 {
		return QuadModel{}
	}
	if n == 1 {
		return QuadModel{A: ys[0]}
	}

	var sx, sy, sxx, sxy float64
	for i := 0; i < n; i++ {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}

	fn := float64(n)
	denom := fn*sxx - sx*sx
	if denom == 0 {
		return QuadModel{A: sy / fn}
	}

	b := (fn*sxy - sx*sy) / denom
	a := (sy - b*sx) / fn
	return QuadModel{A: a, B: b}
}

// solveQuad solves a 3×3 system via Gaussian elimination with partial pivoting.
func solveQuad(m [3][4]float64) QuadModel {
	// Forward elimination with partial pivoting
	for col := 0; col < 3; col++ {
		// Find pivot
		maxVal := math.Abs(m[col][col])
		maxRow := col
		for row := col + 1; row < 3; row++ {
			if v := math.Abs(m[row][col]); v > maxVal {
				maxVal = v
				maxRow = row
			}
		}
		m[col], m[maxRow] = m[maxRow], m[col]

		if math.Abs(m[col][col]) < 1e-15 {
			continue
		}

		// Eliminate below
		for row := col + 1; row < 3; row++ {
			factor := m[row][col] / m[col][col]
			for j := col; j < 4; j++ {
				m[row][j] -= factor * m[col][j]
			}
		}
	}

	// Back substitution
	var x [3]float64
	for i := 2; i >= 0; i-- {
		if math.Abs(m[i][i]) < 1e-15 {
			continue
		}
		x[i] = m[i][3]
		for j := i + 1; j < 3; j++ {
			x[i] -= m[i][j] * x[j]
		}
		x[i] /= m[i][i]
	}

	return QuadModel{A: x[0], B: x[1], C: x[2]}
}

// ComputeDerived computes analytical metrics from the fitted TPOT model.
func ComputeDerived(model QuadModel, targetConcurrency int) DerivedMetrics {
	d := DerivedMetrics{}

	baseline := model.A

	// Optimal throughput concurrency: C_opt = sqrt(a/c)
	// Throughput(C) = C / (a + b*C + c*C²), maximized at C = sqrt(a/c)
	if model.C > 0 && model.A > 0 {
		cOpt := math.Sqrt(model.A / model.C)
		d.OptimalConcurrency = int(math.Round(cOpt))
		if d.OptimalConcurrency < 1 {
			d.OptimalConcurrency = 1
		}
		d.MaxThroughputTokS = cOpt / (model.Predict(cOpt) / 1000) // TPOT is in ms, throughput in tok/s
	}

	// 2× degradation: TPOT(C) = 2 × baseline → a + b*C + c*C² = 2a → b*C + c*C² = a
	// Solve c*C² + b*C - a = 0 using quadratic formula
	if model.C > 0 {
		disc := model.B*model.B + 4*model.C*baseline
		if disc >= 0 {
			c2x := (-model.B + math.Sqrt(disc)) / (2 * model.C)
			if c2x > 0 {
				d.TwoxDegradationC = int(math.Round(c2x))
			}
		}
	} else if model.B > 0 {
		// Linear model: b*C = a → C = a/b
		d.TwoxDegradationC = int(math.Round(baseline / model.B))
	}

	// Regime at target concurrency
	tc := float64(targetConcurrency)
	if model.C > 0 && model.C*tc*tc > model.A {
		d.Regime = "compute-bound"
	} else {
		d.Regime = "bandwidth-bound"
	}

	return d
}
