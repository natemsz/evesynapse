package piplan

import (
	"math"
	"sort"
	"time"
)

// A small dense mixed-integer solver for the colony optimizer. The
// models it sees have tens of rows and a few hundred columns.
//
// The linear part is a bounded-variable simplex on one dense tableau:
// variable bounds are handled in the ratio tests instead of as rows,
// and every node of the search starts from the basis the last node
// left behind (dual simplex after a bound change, primal simplex
// after an objective change).
//
// The whole-number part is branch and bound: it dives for a first
// answer, then always expands the open node with the best bound, so
// the proven bound rises steadily. Once an answer is known, the root
// ranges of the columns flagged in Probe are tightened by asking the
// relaxation for their least and greatest value among strictly better
// answers and rounding inward. Reduced costs tighten ranges at every
// node.

type rowSense int8

const (
	senseLE rowSense = iota
	senseGE
	senseEQ
)

type lpRow struct {
	coef  map[int]float64
	sense rowSense
	rhs   float64
}

// lpModel minimises Obj·x subject to Rows, Lo <= x <= Hi; columns
// flagged in Int must take whole values. Hi < 0 means unbounded.
// Pri orders branching (higher first). Probe marks whole columns
// whose range is worth tightening at the root.
type lpModel struct {
	Obj   []float64
	Rows  []lpRow
	Lo    []float64
	Hi    []float64
	Int   []bool
	Pri   []int
	Probe []bool
}

func newLPModel() *lpModel { return &lpModel{} }

// col adds a column and returns its index.
func (m *lpModel) col(obj, lo, hi float64, integer bool) int {
	m.Obj = append(m.Obj, obj)
	m.Lo = append(m.Lo, lo)
	m.Hi = append(m.Hi, hi)
	m.Int = append(m.Int, integer)
	m.Pri = append(m.Pri, 0)
	m.Probe = append(m.Probe, false)
	return len(m.Obj) - 1
}

func (m *lpModel) row(coef map[int]float64, sense rowSense, rhs float64) {
	if len(coef) == 0 {
		return
	}
	m.Rows = append(m.Rows, lpRow{coef: coef, sense: sense, rhs: rhs})
}

// value is the objective at x.
func (m *lpModel) value(x []float64) float64 {
	v := 0.0
	for j, c := range m.Obj {
		v += c * x[j]
	}
	return v
}

// feasible reports whether x satisfies every bound, row and whole
// number requirement within tol.
func (m *lpModel) feasible(x []float64, tol float64) bool {
	for j, v := range x {
		if v < m.Lo[j]-tol || (m.Hi[j] >= 0 && v > m.Hi[j]+tol) {
			return false
		}
		if m.Int[j] && math.Abs(v-math.Round(v)) > tol {
			return false
		}
	}
	for _, r := range m.Rows {
		lhs, scale := 0.0, 1.0
		for j, c := range r.coef {
			lhs += c * x[j]
			scale = math.Max(scale, math.Abs(c*x[j]))
		}
		d := lhs - r.rhs
		switch r.sense {
		case senseLE:
			if d > tol*scale {
				return false
			}
		case senseGE:
			if d < -tol*scale {
				return false
			}
		case senseEQ:
			if math.Abs(d) > tol*scale {
				return false
			}
		}
	}
	return true
}

const (
	lpTolP   = 1e-7 // a basic value this far outside its range is infeasible
	lpTolPiv = 1e-8 // smallest usable pivot
	lpBigM   = 1e6  // stand-in upper bound for an unbounded column that earns
	lpNoCut  = 1e12 // cutoff row's right side while no answer is known
	intTol   = 1e-6
)

type lpStatus int8

const (
	lpOptimal lpStatus = iota
	lpInfeasible
	lpCutoff // the objective passed the cutoff: nothing better lies here
	lpUnbounded
	lpStalled
)

var lpInf = math.Inf(1)

// simplex is the tableau and its basis. Columns are the model's, then
// one slack per row. Rows are the model's (>= rows negated to <=),
// then the objective cutoff row.
type simplex struct {
	n, m, nt int
	a        [][]float64 // m x n rows as given
	b        []float64
	obj      []float64 // the model's objective, padded to nt
	c        []float64 // the objective in use
	cscale   float64   // the cutoff row is the objective over cscale
	t        [][]float64
	d        []float64 // reduced costs
	beta     []float64 // values of the basic columns, by row
	basis    []int     // row -> column
	where    []int     // column -> row, -1 when nonbasic
	up       []bool    // nonbasic column sits at its upper bound
	lo, hi   []float64
	hi0      []float64 // upper bounds as built, the widest a node may ask for
	z        float64   // objective in use at the current point
	tolD     float64
	dualOK   bool // reduced costs have the right signs for the dual method
	pivots   int
	sinceRef int
	nz       []int
	rbuf     []float64
}

func newSimplex(m *lpModel) *simplex {
	n := len(m.Obj)
	rows := len(m.Rows) + 1
	s := &simplex{n: n, m: rows, nt: n + rows}
	s.a = make([][]float64, rows)
	s.b = make([]float64, rows)
	s.lo = make([]float64, s.nt)
	s.hi = make([]float64, s.nt)
	s.obj = make([]float64, s.nt)
	cmax := 0.0
	for j := 0; j < n; j++ {
		s.lo[j] = m.Lo[j]
		s.hi[j] = lpInf
		if m.Hi[j] >= 0 {
			s.hi[j] = m.Hi[j]
		}
		s.obj[j] = m.Obj[j]
		cmax = math.Max(cmax, math.Abs(m.Obj[j]))
	}
	if cmax == 0 {
		cmax = 1
	}
	s.cscale = cmax
	for i, r := range m.Rows {
		a := make([]float64, n)
		sign := 1.0
		if r.sense == senseGE {
			sign = -1
		}
		for j, v := range r.coef {
			a[j] = sign * v
		}
		s.a[i] = a
		s.b[i] = sign * r.rhs
		s.hi[n+i] = lpInf
		if r.sense == senseEQ {
			s.hi[n+i] = 0
		}
	}
	cut := make([]float64, n)
	for j := 0; j < n; j++ {
		cut[j] = m.Obj[j] / cmax
	}
	s.a[rows-1] = cut
	s.b[rows-1] = lpNoCut
	s.hi[s.nt-1] = lpInf
	// The dual method starts with every earning column at its upper
	// bound, so each needs one.
	for j := 0; j < n; j++ {
		if s.obj[j] < 0 && math.IsInf(s.hi[j], 1) {
			s.hi[j] = s.lo[j] + lpBigM
		}
	}
	s.hi0 = append([]float64(nil), s.hi[:n]...)
	s.t = make([][]float64, rows)
	for i := range s.t {
		s.t[i] = make([]float64, s.nt)
	}
	s.c = make([]float64, s.nt)
	s.d = make([]float64, s.nt)
	s.beta = make([]float64, rows)
	s.basis = make([]int, rows)
	s.where = make([]int, s.nt)
	s.up = make([]bool, s.nt)
	s.rbuf = make([]float64, rows)
	s.tolD = math.Max(1e-9, 1e-11*cmax)
	s.reset()
	return s
}

// reset returns to the all-slack basis under the model's objective.
func (s *simplex) reset() {
	for i := 0; i < s.m; i++ {
		ti := s.t[i]
		copy(ti, s.a[i])
		for k := s.n; k < s.nt; k++ {
			ti[k] = 0
		}
		ti[s.n+i] = 1
		s.basis[i] = s.n + i
	}
	copy(s.c, s.obj)
	copy(s.d, s.obj)
	s.dualOK = true
	for j := 0; j < s.nt; j++ {
		s.where[j] = -1
		s.up[j] = false
		if j < s.n && s.c[j] < 0 {
			if math.IsInf(s.hi[j], 1) {
				s.dualOK = false
			} else {
				s.up[j] = true
			}
		}
	}
	for i := 0; i < s.m; i++ {
		s.where[s.n+i] = i
	}
	s.sinceRef = 0
	s.refresh()
}

func (s *simplex) nbValue(j int) float64 {
	if s.up[j] {
		return s.hi[j]
	}
	return s.lo[j]
}

// refresh recomputes the basic values and the objective from the
// rows as given, shedding the drift of incremental updates.
func (s *simplex) refresh() {
	r := s.rbuf
	copy(r, s.b)
	for j := 0; j < s.n; j++ {
		if s.where[j] >= 0 {
			continue
		}
		v := s.nbValue(j)
		if v == 0 {
			continue
		}
		for i := 0; i < s.m; i++ {
			if a := s.a[i][j]; a != 0 {
				r[i] -= a * v
			}
		}
	}
	// A nonbasic slack sits at zero, its only finite bound.
	for i := 0; i < s.m; i++ {
		inv := s.t[i][s.n:]
		sum := 0.0
		for k, v := range inv {
			if v != 0 {
				sum += v * r[k]
			}
		}
		s.beta[i] = sum
	}
	s.z = 0
	for j := 0; j < s.nt; j++ {
		if s.c[j] != 0 {
			s.z += s.c[j] * s.valueOf(j)
		}
	}
}

func (s *simplex) valueOf(j int) float64 {
	if r := s.where[j]; r >= 0 {
		return s.beta[r]
	}
	return s.nbValue(j)
}

// x copies out the model's columns.
func (s *simplex) x() []float64 {
	out := make([]float64, s.n)
	for j := range out {
		out[j] = s.valueOf(j)
	}
	return out
}

// setObjective switches the objective in use and prices it out
// against the current basis.
func (s *simplex) setObjective(c []float64) {
	copy(s.c, c)
	copy(s.d, c)
	for i := 0; i < s.m; i++ {
		cb := c[s.basis[i]]
		if cb == 0 {
			continue
		}
		for j, v := range s.t[i] {
			if v != 0 {
				s.d[j] -= cb * v
			}
		}
	}
	for i := 0; i < s.m; i++ {
		s.d[s.basis[i]] = 0
	}
	s.dualOK = false
	s.z = 0
	for j := 0; j < s.nt; j++ {
		if s.c[j] != 0 {
			s.z += s.c[j] * s.valueOf(j)
		}
	}
}

// setBounds installs new ranges for the model's columns and the
// cutoff, keeping the basis.
func (s *simplex) setBounds(lo, hi []float64, cut float64) {
	for j := 0; j < s.n; j++ {
		s.lo[j] = lo[j]
		s.hi[j] = math.Min(hi[j], s.hi0[j])
	}
	if dbgNoCutRow || math.IsInf(cut, 1) {
		s.b[s.m-1] = lpNoCut
	} else {
		s.b[s.m-1] = cut / s.cscale
	}
	s.settle()
	s.refresh()
}

// settle puts each nonbasic column on the bound its reduced cost
// wants, so the dual method can start. A column freed from a fixed
// value may sit on the wrong side.
func (s *simplex) settle() {
	ok := true
	for j := 0; j < s.nt; j++ {
		if s.where[j] >= 0 {
			continue
		}
		if s.up[j] && (math.IsInf(s.hi[j], 1) || s.d[j] > s.tolD) {
			s.up[j] = false
		}
		if !s.up[j] && s.d[j] < -s.tolD {
			if math.IsInf(s.hi[j], 1) {
				ok = false // only the primal method can price this one
			} else {
				s.up[j] = true
			}
		}
	}
	s.dualOK = ok
}

func (s *simplex) pivot(r, e int) {
	tr := s.t[r]
	inv := 1 / tr[e]
	nz := s.nz[:0]
	for k, v := range tr {
		if v != 0 {
			tr[k] = v * inv
			nz = append(nz, k)
		}
	}
	tr[e] = 1
	s.nz = nz
	for i := 0; i < s.m; i++ {
		if i == r {
			continue
		}
		ti := s.t[i]
		f := ti[e]
		if f == 0 {
			continue
		}
		for _, k := range nz {
			v := ti[k] - f*tr[k]
			if v < 1e-12 && v > -1e-12 {
				v = 0
			}
			ti[k] = v
		}
		ti[e] = 0
	}
	if f := s.d[e]; f != 0 {
		for _, k := range nz {
			s.d[k] -= f * tr[k]
		}
		s.d[e] = 0
	}
	s.where[s.basis[r]] = -1
	s.where[e] = r
	s.basis[r] = e
	s.pivots++
	s.sinceRef++
}

// dual restores feasibility after bounds moved. It stops early once
// the objective reaches cut.
func (s *simplex) dual(cut float64) lpStatus {
	for iter := 0; iter < 4000; iter++ {
		bland := iter > 400
		r, worst, below := -1, lpTolP, false
		for i := 0; i < s.m; i++ {
			bj := s.basis[i]
			v := s.beta[i]
			e, lowSide := s.lo[bj]-v, true
			if e2 := v - s.hi[bj]; e2 > e {
				e, lowSide = e2, false
			}
			if e <= lpTolP {
				continue
			}
			if bland {
				if r < 0 || bj < s.basis[r] {
					r, below = i, lowSide
				}
			} else if e > worst {
				worst, r, below = e, i, lowSide
			}
		}
		if r < 0 {
			return lpOptimal
		}
		tr := s.t[r]
		e, best, bestA := -1, lpInf, 0.0
		for j := 0; j < s.nt; j++ {
			a := tr[j]
			if a == 0 || s.where[j] >= 0 || s.lo[j] == s.hi[j] {
				continue
			}
			if below {
				a = -a
			}
			if s.up[j] {
				a = -a
			}
			if a <= lpTolPiv {
				continue
			}
			ratio := math.Abs(s.d[j]) / a
			if bland {
				if ratio < best-1e-12 {
					best, e, bestA = ratio, j, a
				}
			} else if ratio < best-1e-11 || (ratio <= best+1e-11 && a > bestA) {
				best, e, bestA = ratio, j, a
			}
		}
		if e < 0 {
			return lpInfeasible
		}
		bl := s.basis[r]
		target := s.hi[bl]
		if below {
			target = s.lo[bl]
		}
		theta := (s.beta[r] - target) / tr[e]
		for i := 0; i < s.m; i++ {
			if a := s.t[i][e]; a != 0 && i != r {
				s.beta[i] -= a * theta
			}
		}
		xe := s.nbValue(e) + theta
		s.z += s.d[e] * theta
		s.up[bl] = !below
		s.pivot(r, e)
		s.beta[r] = xe
		if !dbgNoEarly && s.dualOK && s.z >= cut {
			return lpCutoff
		}
	}
	return lpStalled
}

// primal improves the objective in use from a feasible basis.
func (s *simplex) primal() lpStatus {
	for iter := 0; iter < 4000; iter++ {
		bland := iter > 400
		e, best := -1, s.tolD
		for j := 0; j < s.nt; j++ {
			if s.where[j] >= 0 || s.lo[j] == s.hi[j] {
				continue
			}
			g := -s.d[j]
			if s.up[j] {
				g = -g
			}
			if g > best {
				best, e = g, j
				if bland {
					break
				}
			}
		}
		if e < 0 {
			s.dualOK = true
			return lpOptimal
		}
		dir := 1.0
		if s.up[e] {
			dir = -1
		}
		step := s.hi[e] - s.lo[e]
		r, toUpper, bestA := -1, false, 0.0
		for i := 0; i < s.m; i++ {
			a := s.t[i][e] * dir
			bj := s.basis[i]
			var t float64
			var hitUpper bool
			switch {
			case a > lpTolPiv:
				t = (s.beta[i] - s.lo[bj]) / a
			case a < -lpTolPiv && !math.IsInf(s.hi[bj], 1):
				t = (s.hi[bj] - s.beta[i]) / -a
				hitUpper = true
			default:
				continue
			}
			if t < 0 {
				t = 0
			}
			aa := math.Abs(a)
			better := t < step-1e-11
			if !better && r >= 0 && t <= step+1e-11 {
				if bland {
					better = bj < s.basis[r]
				} else {
					better = aa > bestA
				}
			}
			if better {
				step, r, toUpper, bestA = t, i, hitUpper, aa
			}
		}
		if math.IsInf(step, 1) {
			return lpUnbounded
		}
		theta := dir * step
		for i := 0; i < s.m; i++ {
			if a := s.t[i][e]; a != 0 && i != r {
				s.beta[i] -= a * theta
			}
		}
		s.z += s.d[e] * theta
		if r < 0 {
			s.up[e] = !s.up[e] // the column crossed its whole range
			continue
		}
		xe := s.nbValue(e) + theta
		s.up[s.basis[r]] = toUpper
		s.pivot(r, e)
		s.beta[r] = xe
	}
	return lpStalled
}

// refactor rebuilds the tableau for the current basis from the rows
// as given. False when the basis has gone singular.
func (s *simplex) refactor() bool {
	want := append([]int(nil), s.basis...)
	sort.Sort(sort.Reverse(sort.IntSlice(want))) // slacks first: they cost nothing
	for i := 0; i < s.m; i++ {
		ti := s.t[i]
		copy(ti, s.a[i])
		for k := s.n; k < s.nt; k++ {
			ti[k] = 0
		}
		ti[s.n+i] = 1
		s.basis[i] = -1
	}
	for j := range s.d {
		s.d[j] = 0
	}
	for j := range s.where {
		s.where[j] = -1
	}
	for _, bv := range want {
		r, big := -1, 1e-9
		for i := 0; i < s.m; i++ {
			if s.basis[i] < 0 {
				if v := math.Abs(s.t[i][bv]); v > big {
					r, big = i, v
				}
			}
		}
		if r < 0 {
			return false
		}
		s.basis[r] = bv // pivot reads the old entry
		s.pivot(r, bv)
	}
	s.sinceRef = 0
	s.setObjective(s.c)
	s.refresh()
	return true
}

// solve brings the tableau to the optimum for the bounds in force.
func (s *simplex) solve(cut float64) lpStatus {
	if s.sinceRef > 1500 {
		if !s.refactor() {
			s.reset()
		}
		s.settle()
	}
	for attempt := 0; ; attempt++ {
		st := s.dual(cut)
		if st == lpOptimal && !s.dualOK {
			st = s.primal()
		}
		if st == lpOptimal {
			// Check against freshly computed values before trusting it.
			s.refresh()
			clean := true
			for i := 0; i < s.m; i++ {
				bj := s.basis[i]
				if s.beta[i] < s.lo[bj]-10*lpTolP || s.beta[i] > s.hi[bj]+10*lpTolP {
					clean = false
					break
				}
			}
			if clean {
				return lpOptimal
			}
		}
		if (st != lpStalled && st != lpOptimal) || attempt >= 2 {
			if st == lpOptimal {
				return lpStalled
			}
			return st
		}
		if attempt == 1 || !s.refactor() {
			s.reset()
		}
		s.settle()
		s.refresh()
	}
}

// --- branch and bound -------------------------------------------------------

type milpOptions struct {
	budget   time.Duration
	maxNodes int
	start    []float64 // a known answer to begin from, or nil
	gapRel   float64   // answers within this share of the best are not sought
	gapAbs   float64
}

type milpResult struct {
	x     []float64
	obj   float64
	bound float64 // no whole answer is below this
	exact bool    // the search finished: obj is within the gap of the best
	ok    bool
	nodes int
}

type bbNode struct {
	lo, hi []float64
	bound  float64
	depth  int
}

type bbSearch struct {
	m        *lpModel
	s        *simplex
	opt      milpOptions
	rootLo   []float64
	rootHi   []float64
	ints     []int // whole columns, branching order
	probe    []int
	best     []float64
	bestObj  float64
	cut      float64
	open     []*bbNode
	nodes    int
	deadline time.Time
	exact    bool
	lo, hi   []float64 // scratch
}

func solveMILP(m *lpModel, opt milpOptions) milpResult {
	if opt.gapRel <= 0 {
		opt.gapRel = 1e-5
	}
	if opt.gapAbs <= 0 {
		opt.gapAbs = 1e-7
	}
	n := len(m.Obj)
	b := &bbSearch{m: m, opt: opt, bestObj: lpInf, cut: lpInf, exact: true}
	b.deadline = time.Now().Add(opt.budget)
	b.rootLo = append([]float64(nil), m.Lo...)
	b.rootHi = make([]float64, n)
	for j := range b.rootHi {
		b.rootHi[j] = lpInf
		if m.Hi[j] >= 0 {
			b.rootHi[j] = m.Hi[j]
		}
	}
	for j := 0; j < n; j++ {
		if m.Int[j] {
			b.ints = append(b.ints, j)
			if m.Probe[j] {
				b.probe = append(b.probe, j)
			}
		}
	}
	sort.SliceStable(b.ints, func(i, k int) bool { return m.Pri[b.ints[i]] > m.Pri[b.ints[k]] })
	b.lo = make([]float64, n)
	b.hi = make([]float64, n)
	b.s = newSimplex(m)
	if opt.start != nil {
		b.accept(opt.start)
	}

	res := milpResult{bound: math.Inf(-1)}
	root := &bbNode{lo: b.rootLo, hi: b.rootHi, bound: math.Inf(-1)}
	st, rootObj := b.relax(root)
	switch st {
	case lpOptimal:
		res.bound = rootObj
		root.bound = rootObj
	case lpStalled:
		b.exact = false
	default:
		// Infeasible, or nothing beats the starting answer.
		return b.result(res, true)
	}
	if b.best != nil && !b.tighten() {
		return b.result(res, true)
	}
	b.open = append(b.open, root)
	done := b.run()
	return b.result(res, done)
}

func (b *bbSearch) result(res milpResult, done bool) milpResult {
	res.nodes = b.nodes
	res.exact = done && b.exact
	if b.best != nil {
		res.x, res.obj, res.ok = b.best, b.bestObj, true
	}
	if res.exact {
		if res.ok {
			res.bound = math.Max(res.bound, b.cut)
		}
		return res
	}
	low := b.bestObj
	for _, nd := range b.open {
		low = math.Min(low, nd.bound)
	}
	res.bound = math.Max(res.bound, low)
	return res
}

// accept takes x as the best answer if it is whole, feasible and
// better than the one held.
func (b *bbSearch) accept(x []float64) bool {
	y := append([]float64(nil), x...)
	for _, j := range b.ints {
		y[j] = math.Round(y[j])
	}
	if !b.m.feasible(y, 1e-6) {
		return false
	}
	obj := b.m.value(y)
	if obj >= b.bestObj {
		return false
	}
	b.best, b.bestObj = y, obj
	b.cut = obj - math.Max(b.opt.gapAbs, b.opt.gapRel*math.Abs(obj))
	keep := b.open[:0]
	for _, nd := range b.open {
		if nd.bound < b.cut {
			keep = append(keep, nd)
		}
	}
	b.open = keep
	return true
}

// relax solves a node's relaxation inside the root ranges.
func (b *bbSearch) relax(nd *bbNode) (lpStatus, float64) {
	for j := range b.lo {
		b.lo[j] = math.Max(nd.lo[j], b.rootLo[j])
		b.hi[j] = math.Min(nd.hi[j], b.rootHi[j])
		if b.lo[j] > b.hi[j]+intTol {
			return lpInfeasible, 0
		}
	}
	b.s.setBounds(b.lo, b.hi, b.cut)
	st := b.s.solve(b.cut)
	if dbgCheck != nil {
		dbgCheck(b.lo, b.hi, st, b.s.z, b.cut)
	}
	if st == lpOptimal && b.s.z >= b.cut {
		return lpCutoff, b.s.z
	}
	return st, b.s.z
}

// branchOn picks the whole column to split: among the fractional
// ones of the highest priority, the one furthest from a whole value.
func (b *bbSearch) branchOn(x []float64) int {
	bj, frac, pri := -1, 0.0, 0
	for _, j := range b.ints {
		if bj >= 0 && b.m.Pri[j] < pri {
			break
		}
		v := x[j]
		f := v - math.Floor(v)
		d := math.Min(f, 1-f)
		if d > intTol && d > frac+1e-9 {
			bj, frac, pri = j, d, b.m.Pri[j]
		}
	}
	return bj
}

// run expands nodes until none is left (true) or the budget is spent.
func (b *bbSearch) run() bool {
	var cur *bbNode
	for {
		if cur == nil {
			if len(b.open) == 0 {
				return true
			}
			// Best bound first; the newest of equals, which is nearest
			// the basis in hand.
			bi := 0
			for i, nd := range b.open {
				if nd.bound <= b.open[bi].bound {
					bi = i
				}
			}
			cur = b.open[bi]
			b.open = append(b.open[:bi], b.open[bi+1:]...)
		}
		if b.nodes >= b.opt.maxNodes || (b.nodes&7 == 0 && time.Now().After(b.deadline)) {
			b.open = append(b.open, cur)
			return false
		}
		b.nodes++
		st, obj := b.relax(cur)
		if dbgTrace {
			println("node", b.nodes, "depth", cur.depth, "st", st, "obj", obj, "cut", b.cut)
		}
		if st == lpStalled {
			b.exact = false
			dbgStat["stalled"]++
		}
		if st != lpOptimal {
			cur = nil
			continue
		}
		x := b.s.x()
		bj := b.branchOn(x)
		if bj < 0 {
			if b.accept(x) {
				if !b.tighten() {
					b.open = b.open[:0]
					return true
				}
			} else if obj < b.cut {
				dbgStat["rejected"]++
				b.exact = false // whole but rejected on a closer look
			}
			cur = nil
			continue
		}
		lo := append([]float64(nil), b.lo...)
		hi := append([]float64(nil), b.hi...)
		b.reducedCostTighten(lo, hi, obj)
		v := x[bj]
		down := &bbNode{lo: lo, hi: append([]float64(nil), hi...), bound: obj, depth: cur.depth + 1}
		down.hi[bj] = math.Floor(v)
		upn := &bbNode{lo: append([]float64(nil), lo...), hi: hi, bound: obj, depth: cur.depth + 1}
		upn.lo[bj] = math.Ceil(v)
		// Dive on the round-up side: a model that needs capacity finds
		// a feasible answer fastest by building more.
		b.open = append(b.open, down)
		cur = upn
	}
}

// reducedCostTighten narrows whole columns that cannot move far from
// their bound without the objective passing the cutoff.
func (b *bbSearch) reducedCostTighten(lo, hi []float64, obj float64) {
	if dbgNoRC || math.IsInf(b.cut, 1) {
		return
	}
	room := b.cut - obj
	s := b.s
	for _, j := range b.ints {
		if s.where[j] >= 0 || lo[j] >= hi[j] {
			continue
		}
		d := s.d[j]
		if s.up[j] && d < -s.tolD {
			if k := math.Floor(room/-d + intTol); hi[j]-k > lo[j] {
				lo[j] = hi[j] - k
			}
		} else if !s.up[j] && d > s.tolD {
			if k := math.Floor(room/d + intTol); lo[j]+k < hi[j] {
				hi[j] = lo[j] + k
			}
		}
	}
}

// tighten narrows the root ranges of the probe columns to what an
// answer better than the cutoff allows. False when no such answer
// exists, which ends the search.
func (b *bbSearch) tighten() bool {
	if len(b.probe) == 0 || math.IsInf(b.cut, 1) {
		return true
	}
	s := b.s
	s.setBounds(b.rootLo, b.rootHi, b.cut)
	if st := s.solve(lpInf); st != lpOptimal {
		if st == lpStalled {
			b.exact = false
			return true
		}
		return false
	}
	real := append([]float64(nil), s.c...)
	c := make([]float64, s.nt)
	// seenLo and seenHi are values each column took at some feasible
	// point since the last change: a column seen at its bound cannot
	// be tightened there.
	seenLo := make([]float64, s.n)
	seenHi := make([]float64, s.n)
	clear := func() {
		for j := range seenLo {
			seenLo[j], seenHi[j] = lpInf, math.Inf(-1)
		}
	}
	note := func() {
		for _, j := range b.probe {
			v := s.valueOf(j)
			seenLo[j] = math.Min(seenLo[j], v)
			seenHi[j] = math.Max(seenHi[j], v)
		}
	}
	clear()
	note()
	alive := true
	for round := 0; round < 4 && alive; round++ {
		changed := false
		for _, j := range b.probe {
			for _, sense := range []float64{1, -1} {
				if !alive || time.Now().After(b.deadline) {
					break
				}
				if sense > 0 && seenLo[j] <= b.rootLo[j]+intTol {
					continue
				}
				if sense < 0 && seenHi[j] >= b.rootHi[j]-intTol {
					continue
				}
				c[j] = sense
				s.setObjective(c)
				c[j] = 0
				st := s.primal()
				if st == lpStalled {
					s.reset()
					return true
				}
				if st != lpOptimal {
					continue // unbounded: nothing to learn
				}
				note()
				v := s.valueOf(j)
				moved := false
				if sense > 0 {
					if nl := math.Ceil(v - 1e-7); nl > b.rootLo[j] {
						b.rootLo[j], moved = nl, true
					}
				} else {
					if nh := math.Floor(v + 1e-7); nh < b.rootHi[j] {
						b.rootHi[j], moved = nh, true
					}
				}
				if !moved {
					continue
				}
				changed = true
				if b.rootLo[j] > b.rootHi[j] {
					alive = false
					break
				}
				// The basis is optimal for this objective, so the dual
				// method can absorb the new bound.
				s.lo[j], s.hi[j] = b.rootLo[j], b.rootHi[j]
				s.refresh()
				if st := s.dual(lpInf); st == lpStalled {
					s.reset()
					return true
				} else if st != lpOptimal {
					alive = false
					break
				}
				clear()
				note()
			}
		}
		if !changed {
			break
		}
	}
	s.setObjective(real)
	if !alive {
		return false
	}
	if st := s.primal(); st != lpOptimal {
		s.reset()
	}
	return true
}

var dbgNoRC, dbgNoEarly, dbgNoCutRow, dbgTrace bool

var dbgCheck func(lo, hi []float64, st lpStatus, obj, cut float64)

var dbgStat = map[string]int{}
