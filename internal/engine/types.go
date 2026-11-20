package engine

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Kind is the storage class of a column or expression. Decimal arithmetic is
// fixed-point, integer arithmetic is checked at runtime.
type Kind int

const (
	KInvalid Kind = iota
	KInt
	KDec
	KString
	KBool
	KDate
	KClock // a clock-face tick from 0 (empty) to TickMax (sold)
	KNull  // the type of the untyped NULL literal
)

// TickMax is 100.00: a Dutch auction starts at the ceiling and descends to the
// clock value at which the lot sells.
const TickMax = 10000

func (k Kind) String() string {
	switch k {
	case KInt:
		return "int"
	case KDec:
		return "dec"
	case KString:
		return "string"
	case KBool:
		return "bool"
	case KDate:
		return "date"
	case KClock:
		return "tick"
	case KNull:
		return "null"
	}
	return "invalid"
}

// Type is a resolved expression/column type. Only decimal and clock carry
// parameters.
type Type struct {
	Kind Kind
	// Precision and Scale are meaningful for KDec.
	Precision int
	Scale     int
}

var (
	DecType = func(p, s int) Type { return Type{Kind: KDec, Precision: p, Scale: s} }
	IntT    = Type{Kind: KInt}
	StrT    = Type{Kind: KString}
	BoolT   = Type{Kind: KBool}
	DateT   = Type{Kind: KDate}
	ClockT  = Type{Kind: KClock}
	NullT   = Type{Kind: KNull}
)

func (t Type) String() string {
	switch t.Kind {
	case KDec:
		return fmt.Sprintf("decimal(%d,%d)", t.Precision, t.Scale)
	case KClock:
		return "tick(0..100.00)"
	}
	return t.Kind.String()
}

// Same reports whether two types are identical. Null is compatible with
// everything for unification purposes.
func (t Type) Same(o Type) bool {
	if t.Kind == KNull || o.Kind == KNull {
		return true
	}
	if t.Kind != o.Kind {
		return false
	}
	if t.Kind == KDec {
		return t.Precision == o.Precision && t.Scale == o.Scale
	}
	return true
}

func (t Type) IsNumeric() bool { return t.Kind == KInt || t.Kind == KDec || t.Kind == KNull }
func (t Type) IsNull() bool    { return t.Kind == KNull }

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// Pos is a 1-based source position. Column counts runes, not bytes.
type Pos struct {
	Line int
	Col  int
}

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

func (p Pos) before(o Pos) bool {
	if p.Line != o.Line {
		return p.Line < o.Line
	}
	return p.Col < o.Col
}

// Code identifies an error category. Stored in plans so the executor can map
// errors onto its own vocabulary.
type Code string

const (
	CodeLex         Code = "lex"
	CodeSyntax      Code = "syntax"
	CodeName        Code = "name"
	CodeAmbiguous   Code = "ambiguous"
	CodeType        Code = "type"
	CodeArity       Code = "arity"
	CodeImplicit    Code = "implicit-conversion"
	CodeUnknownFn   Code = "unknown-function"
	CodeCardinality Code = "cardinality"
	CodeRewrite     Code = "rewrite"
	CodeEval        Code = "eval"
	CodeInternal    Code = "internal"
)

// Error is the single diagnostic type produced by every phase. It always
// carries a position and a stable code.
type Error struct {
	Code  Code
	Pos   Pos
	Msg   string
	Hints []string
}

func (e *Error) Error() string { return fmt.Sprintf("%d:%d: %s", e.Pos.Line, e.Pos.Col, e.Msg) }

func errAt(code Code, p Pos, format string, args ...any) *Error {
	return &Error{Code: code, Pos: p, Msg: fmt.Sprintf(format, args...)}
}

func errWithHints(code Code, p Pos, msg string, hints []string) *Error {
	return &Error{Code: code, Pos: p, Msg: msg, Hints: hints}
}

// sortErrors orders diagnostics by position, keeping insertion order for ties.
func sortErrors(errs []*Error) []*Error {
	out := make([]*Error, len(errs))
	copy(out, errs)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Pos.before(out[j-1].Pos); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Typing rules
// ---------------------------------------------------------------------------

var convHints = []string{
	"clock values do not convert implicitly: use from_clock(tick) to get a date or to_clock(date, base) to get a tick",
	"decimal and int do mix implicitly; cast with CAST(x AS ...) if the result precision needs pinning",
}

// unifyNumeric applies the fixed-point promotion rule: if either operand is a
// decimal the result is decimal(p, s) with
//
//	p = max(p1 - s1, p2 - s2) + max(s1, s2) + 1
//	s = max(s1, s2)
//
// The extra digit is the carry room for addition. Multiplication is narrower
// (no carry) and rounding only appears in division. Int - Int stays Int.
func unifyNumeric(a, b Type) (Type, bool) {
	if a.IsNull() {
		return b, b.IsNumeric()
	}
	if b.IsNull() {
		return a, a.IsNumeric()
	}
	if a.Kind == KInt && b.Kind == KInt {
		return IntT, true
	}
	ai, bi := a, b
	if ai.Kind == KInt {
		ai = DecType(19, 0)
	}
	if bi.Kind == KInt {
		bi = DecType(19, 0)
	}
	ip := ai.Precision - ai.Scale
	if x := bi.Precision - bi.Scale; x > ip {
		ip = x
	}
	sc := ai.Scale
	if bi.Scale > sc {
		sc = bi.Scale
	}
	return DecType(ip+sc+1, sc), true
}

// ---------------------------------------------------------------------------
// Decimal arithmetic at runtime
// ---------------------------------------------------------------------------

type Decimal struct {
	Unscaled int64
	Scale    int
}

// pow10 holds powers of ten up to what fits in an int64.
var pow10 = func() [19]int64 {
	var p [19]int64
	p[0] = 1
	for i := 1; i < 19; i++ {
		p[i] = p[i-1] * 10
	}
	return p
}()

func pow10Checked(n int) (int64, bool) {
	if n < 0 || n >= len(pow10) {
		return 0, false
	}
	return pow10[n], true
}

// roundDiv divides by a positive d, rounding half away from zero.
func roundDiv(n, d int64) (int64, bool) {
	if d <= 0 {
		return 0, false
	}
	if n > (1<<62)/4 || n < -(1<<62)/4 {
		// Refuse to multiply n by 2 when it could overflow.
		return 0, false
	}
	num := n * 2
	if num >= 0 {
		return (num + d) / (2 * d), true
	}
	return (num - d) / (2 * d), true
}

// rescale changes the scale, rounding half away from zero, and reports overflow.
func (d Decimal) rescale(scale int) (Decimal, bool) {
	if scale == d.Scale {
		return d, true
	}
	if scale > d.Scale {
		f, ok := pow10Checked(scale - d.Scale)
		if !ok {
			return Decimal{}, false
		}
		if d.Unscaled > (1<<62)/f || d.Unscaled < -(1<<62)/f {
			return Decimal{}, false
		}
		return Decimal{d.Unscaled * f, scale}, true
	}
	f, ok := pow10Checked(d.Scale - scale)
	if !ok {
		return Decimal{}, false
	}
	u, ok := roundDiv(d.Unscaled, f)
	if !ok {
		return Decimal{}, false
	}
	return Decimal{u, scale}, true
}

// DecAdd adds two decimals and returns the result at the given scale (the
// type-derived scale). Exact addition can only overflow; the scale of the
// result type is large enough for the operands' scale, so rounding is a no-op.
func DecAdd(a, b Decimal, scale int) (Decimal, bool) {
	aa, ok := a.rescale(scale)
	if !ok {
		return Decimal{}, false
	}
	bb, ok := b.rescale(scale)
	if !ok {
		return Decimal{}, false
	}
	s := aa.Unscaled + bb.Unscaled
	if (aa.Unscaled > 0 && bb.Unscaled > 0 && s < 0) || (aa.Unscaled < 0 && bb.Unscaled < 0 && s >= 0) {
		return Decimal{}, false
	}
	return Decimal{s, scale}, true
}

func DecSub(a, b Decimal, scale int) (Decimal, bool) {
	bb := Decimal{-b.Unscaled, b.Scale}
	return DecAdd(a, bb, scale)
}

// DecMul multiplies exactly and then rounds to the result scale. It reports
// overflow instead of silently wrapping.
func DecMul(a, b Decimal, scale int) (Decimal, bool) {
	hi, lo := mul64(a.Unscaled, b.Unscaled)
	if hi != 0 && hi != -1 {
		return Decimal{}, false
	}
	prod := lo
	// Exact product scale is a.Scale+b.Scale; we may need to reduce it.
	if a.Scale+b.Scale < scale {
		f, ok := pow10Checked(scale - a.Scale - b.Scale)
		if !ok || prod > (1<<62)/f || prod < -(1<<62)/f {
			return Decimal{}, false
		}
		return Decimal{prod * f, scale}, true
	}
	d, ok := pow10Checked(a.Scale + b.Scale - scale)
	if !ok {
		return Decimal{}, false
	}
	u, ok := roundDiv(prod, d)
	if !ok {
		return Decimal{}, false
	}
	return Decimal{u, scale}, true
}

// DecDiv computes a/b rounded half away from zero at the given scale. The
// numerator is widened by (scale + b.Scale - a.Scale) digits before dividing;
// if that intermediate does not fit the operation fails with overflow.
func DecDiv(a, b Decimal, scale int) (Decimal, bool) {
	if b.Unscaled == 0 {
		return Decimal{}, false
	}
	widen := scale + b.Scale - a.Scale
	n := a.Unscaled
	if widen >= 0 {
		f, ok := pow10Checked(widen)
		if !ok {
			return Decimal{}, false
		}
		if n > (1<<62)/f || n < -(1<<62)/f {
			return Decimal{}, false
		}
		n *= f
	} else {
		f, ok := pow10Checked(-widen)
		if !ok || n%f != 0 {
			// Reducing scale would lose information before the division;
			// fall back to a reduced exact form.
			d, ok2 := pow10Checked(-widen)
			if !ok2 {
				return Decimal{}, false
			}
			n, ok = roundDiv(n, d)
			if !ok {
				return Decimal{}, false
			}
		} else {
			n /= f
		}
	}
	u, ok := roundDiv(n, b.Unscaled)
	if !ok {
		return Decimal{}, false
	}
	return Decimal{u, scale}, true
}

func mul64(a, b int64) (hi, lo int64) {
	const mask = 1<<32 - 1
	neg := (a < 0) != (b < 0)
	ua, ub := uint64(a), uint64(b)
	if a < 0 {
		ua = uint64(-a)
	}
	if b < 0 {
		ub = uint64(-b)
	}
	al, ah := ua&mask, ua>>32
	bl, bh := ub&mask, ub>>32
	ll := al * bl
	lh := al * bh
	hl := ah * bl
	hh := ah * bh
	mid := lh + (ll >> 32)
	loU := (ll & mask) | (mid << 32)
	hiU := hh + (mid >> 32) + hl
	if neg {
		// two's complement negate the 128-bit value
		loU = ^loU + 1
		hiU = ^hiU
		if loU == 0 {
			hiU++
		}
	}
	return int64(hiU), int64(loU)
}

func (d Decimal) String() string {
	neg := d.Unscaled < 0
	u := d.Unscaled
	if neg {
		u = -u
	}
	s := strconv.FormatInt(u, 10)
	if d.Scale > 0 {
		for len(s) <= d.Scale {
			s = "0" + s
		}
		s = s[:len(s)-d.Scale] + "." + s[len(s)-d.Scale:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

func (d Decimal) Float() float64 {
	f, _ := strconv.ParseFloat(d.String(), 64)
	return f
}

// ParseDecimal parses an exact decimal literal with at most 18 total digits.
func ParseDecimal(s string) (Decimal, int, bool) {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i+1:]
	}
	if intPart == "" && frac == "" {
		return Decimal{}, 0, false
	}
	digits := intPart + frac
	if len(digits) > 18 {
		return Decimal{}, 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return Decimal{}, 0, false
		}
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return Decimal{}, 0, false
	}
	if neg {
		v = -v
	}
	ip := len(intPart)
	if ip == 0 {
		ip = 1
	}
	return Decimal{v, len(frac)}, ip, true
}

// ---------------------------------------------------------------------------
// Function registry
// ---------------------------------------------------------------------------

// FnArg describes one parameter of a function signature.
type FnArg struct {
	Name string
	// Accept is the set of kinds accepted. An empty set means "any".
	Accept []Kind
}

// FnSig describes a function's parameters and return type. Built-ins and
// auction functions are registered through exactly the same path.
type FnSig struct {
	Name     string
	Args     []FnArg
	Variadic bool
	// Ret computes the result type given the resolved argument types. It may
	// return an *Error for a type error; p is the call position.
	Ret func(p Pos, args []Type) (Type, *Error)
	// Deterministic marks pure functions usable by constant folding.
	Deterministic bool
}

// FnDef is a registered function: signature plus implementation.
type FnDef struct {
	Sig FnSig
	Fn  func(args []Value) (Value, *Error)
}

// Value is a runtime value. Null values have Null=true and Kind carries the
// static type when known.
type Value struct {
	Kind  Kind
	Null  bool
	Int   int64
	Dec   Decimal
	Str   string
	Bool  bool
	Date  int32 // days since 1970-01-01
	Clock int32 // ticks 0..TickMax
}

func NullValue(k Kind) Value { return Value{Kind: k, Null: true} }

func (v Value) String() string {
	if v.Null {
		return "NULL"
	}
	switch v.Kind {
	case KInt:
		return strconv.FormatInt(v.Int, 10)
	case KDec:
		return v.Dec.String()
	case KString:
		return v.Str
	case KBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case KDate:
		return FormatDate(v.Date)
	case KClock:
		return formatTick(v.Clock)
	}
	return "?"
}

// Equal compares two values for SQL equality with NULL semantics: NULL is not
// equal to anything, including itself, so Equal returns (false,false) for nulls.
func (v Value) Equal(o Value) (equal, known bool) {
	if v.Null || o.Null {
		return false, false
	}
	return v.EqualNonNull(o), true
}

// EqualNonNull compares values, coercing numeric kinds and tick/clock.
func (v Value) EqualNonNull(o Value) bool {
	if v.IsNumericKind() && o.IsNumericKind() {
		a, b := toDec(v), toDec(o)
		return a == b
	}
	switch v.Kind {
	case KString:
		return o.Kind == KString && v.Str == o.Str
	case KBool:
		return o.Kind == KBool && v.Bool == o.Bool
	case KDate:
		return o.Kind == KDate && v.Date == o.Date
	case KClock:
		return o.Kind == KClock && v.Clock == o.Clock
	}
	return false
}

func (v Value) IsNumericKind() bool { return v.Kind == KInt || v.Kind == KDec }

func toDec(v Value) Decimal {
	if v.Kind == KInt {
		return Decimal{v.Int, 0}
	}
	return v.Dec
}

// compareValues returns -1/0/1 and whether the comparison is defined.
func compareValues(a, b Value) (int, bool) {
	if a.Null || b.Null {
		return 0, false
	}
	if a.IsNumericKind() && b.IsNumericKind() {
		x, y := toDec(a), toDec(b)
		s, ok := DecSub(x, y, maxInt(x.Scale, y.Scale))
		if !ok {
			return 0, false
		}
		switch {
		case s.Unscaled < 0:
			return -1, true
		case s.Unscaled > 0:
			return 1, true
		}
		return 0, true
	}
	if a.Kind != b.Kind {
		return 0, false
	}
	switch a.Kind {
	case KString:
		return strings.Compare(a.Str, b.Str), true
	case KBool:
		x, y := 0, 0
		if a.Bool {
			x = 1
		}
		if b.Bool {
			y = 1
		}
		if x == y {
			return 0, true
		}
		if x < y {
			return -1, true
		}
		return 1, true
	case KDate:
		return cmpInt32(a.Date, b.Date), true
	case KClock:
		return cmpInt32(a.Clock, b.Clock), true
	}
	return 0, false
}

func cmpInt32(a, b int32) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Dates and clock ticks
// ---------------------------------------------------------------------------

// civilToDays converts a proleptic Gregorian date to days since 1970-01-01.
func civilToDays(y, m, d int) int32 {
	y -= btoi(m <= 2)
	era := floorDiv(y, 400)
	yoe := y - era*400
	mp := m
	if m > 2 {
		mp -= 3
	} else {
		mp += 9
	}
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return int32(era*146097 + doe - 719468)
}

func daysToCivil(z int32) (y, m, d int) {
	zz := int(z) + 719468
	era := floorDiv(zz, 146097)
	doe := zz - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	yy := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	dd := doy - (153*mp+2)/5 + 1
	mm := mp + 3
	if mp >= 10 {
		mm = mp - 9
	}
	if mm <= 2 {
		yy++
	}
	return yy, mm, dd
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func FormatDate(d int32) string {
	y, m, dd := daysToCivil(d)
	return fmt.Sprintf("%04d-%02d-%02d", y, m, dd)
}

func ParseDate(s string) (int32, bool) {
	parts := strings.Split(s, "-")
	if len(parts) != 3 || len(parts[0]) != 4 || len(parts[1]) != 2 || len(parts[2]) != 2 {
		return 0, false
	}
	y, e1 := strconv.Atoi(parts[0])
	m, e2 := strconv.Atoi(parts[1])
	d, e3 := strconv.Atoi(parts[2])
	if e1 != nil || e2 != nil || e3 != nil || m < 1 || m > 12 || d < 1 || d > 31 {
		return 0, false
	}
	z := civilToDays(y, m, d)
	yy, mm, dd := daysToCivil(z)
	if yy != y || mm != m || dd != d {
		return 0, false
	}
	return z, true
}

func formatTick(c int32) string {
	d := Decimal{int64(c), 2}
	return d.String()
}

// ParseTick parses a clock value in [0, 100.00] with at most 2 decimals.
func ParseTick(s string) (int32, bool) {
	d, _, ok := ParseDecimal(s)
	if !ok || d.Scale > 2 || d.Unscaled < 0 {
		return 0, false
	}
	x, ok := d.rescale(2)
	if !ok || x.Unscaled > TickMax {
		return 0, false
	}
	return int32(x.Unscaled), true
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry holds functions. The built-in set and the auction set use the same
// Register call; nothing distinguishes them.
type Registry struct {
	mu    sync.RWMutex
	funcs map[string]*FnDef
	order []string
}

func NewRegistry() *Registry {
	r := &Registry{funcs: map[string]*FnDef{}}
	RegisterBuiltins(r)
	RegisterAuction(r)
	return r
}

// Register installs a function. Re-registering a name replaces it, which is
// how a deployment overrides a builtin with its own auction variant.
func (r *Registry) Register(d *FnDef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.funcs[d.Sig.Name]; !ok {
		r.order = append(r.order, d.Sig.Name)
	}
	r.funcs[d.Sig.Name] = d
}

func (r *Registry) Lookup(name string) (*FnDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.funcs[name]
	return d, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// RegisterFunction is the public extension point. A host installs an auction
// function exactly as the builtins were installed.
func (r *Registry) RegisterFunction(name string, args []FnArg, variadic bool, ret func(p Pos, args []Type) (Type, *Error), fn func(args []Value) (Value, *Error)) {
	r.Register(&FnDef{
		Sig: FnSig{Name: name, Args: args, Variadic: variadic, Ret: ret, Deterministic: true},
		Fn:  fn,
	})
}

// CheckArgs validates a call's argument types against the signature and
// returns the result type. Both builtins and auction functions go through it.
func (s *FnSig) CheckArgs(p Pos, args []Type) (Type, *Error) {
	if !s.Variadic && len(args) != len(s.Args) {
		return Type{}, errAt(CodeArity, p, "%s expects %d argument(s), got %d", s.Name, len(s.Args), len(args))
	}
	if s.Variadic && len(args) < len(s.Args) {
		return Type{}, errAt(CodeArity, p, "%s expects at least %d argument(s), got %d", s.Name, len(s.Args), len(args))
	}
	for i, at := range args {
		var spec FnArg
		if i < len(s.Args) {
			spec = s.Args[i]
		} else {
			spec = s.Args[len(s.Args)-1]
		}
		if len(spec.Accept) == 0 || at.IsNull() {
			continue
		}
		ok := false
		for _, k := range spec.Accept {
			if k == at.Kind {
				ok = true
				break
			}
		}
		if !ok {
			return Type{}, errWithHints(CodeType, p,
				fmt.Sprintf("%s argument %d (%s) must be %s, got %s", s.Name, i+1, spec.Name, kindList(spec.Accept), at),
				hintForKind(spec.Accept, at))
		}
	}
	if s.Ret == nil {
		return NullT, nil
	}
	return s.Ret(p, args)
}

func hintForKind(accept []Kind, got Type) []string {
	for _, k := range accept {
		if (k == KDate && got.Kind == KClock) || (k == KClock && got.Kind == KDate) {
			return convHints
		}
	}
	return nil
}

func kindList(ks []Kind) string {
	parts := make([]string, len(ks))
	for i, k := range ks {
		parts[i] = k.String()
	}
	return strings.Join(parts, " or ")
}

// fixedDec returns a return-type function for functions that widen input
// decimals to a fixed scale.
func fixedDec(p, s int) func(Pos, []Type) (Type, *Error) {
	return func(Pos, []Type) (Type, *Error) { return DecType(p, s), nil }
}

func RegisterBuiltins(r *Registry) {
	num := []Kind{KInt, KDec}

	r.RegisterFunction("lower", []FnArg{{"s", []Kind{KString}}}, false,
		func(Pos, []Type) (Type, *Error) { return StrT, nil },
		func(a []Value) (Value, *Error) {
			if a[0].Null {
				return NullValue(KString), nil
			}
			return Value{Kind: KString, Str: strings.ToLower(a[0].Str)}, nil
		})

	r.RegisterFunction("upper", []FnArg{{"s", []Kind{KString}}}, false,
		func(Pos, []Type) (Type, *Error) { return StrT, nil },
		func(a []Value) (Value, *Error) {
			if a[0].Null {
				return NullValue(KString), nil
			}
			return Value{Kind: KString, Str: strings.ToUpper(a[0].Str)}, nil
		})

	r.RegisterFunction("length", []FnArg{{"s", []Kind{KString}}}, false,
		func(Pos, []Type) (Type, *Error) { return IntT, nil },
		func(a []Value) (Value, *Error) {
			if a[0].Null {
				return NullValue(KInt), nil
			}
			return Value{Kind: KInt, Int: int64(len([]rune(a[0].Str)))}, nil
		})

	r.RegisterFunction("abs", []FnArg{{"x", num}}, false,
		func(p Pos, at []Type) (Type, *Error) {
			if at[0].Kind == KInt {
				return IntT, nil
			}
			if at[0].Kind == KDec {
				return at[0], nil
			}
			return NullT, nil
		},
		func(a []Value) (Value, *Error) {
			if a[0].Null {
				return a[0], nil
			}
			switch a[0].Kind {
			case KInt:
				v := a[0].Int
				if v == -1<<63 {
					return Value{}, errAt(CodeEval, Pos{}, "abs overflow")
				}
				if v < 0 {
					v = -v
				}
				return Value{Kind: KInt, Int: v}, nil
			case KDec:
				d := a[0].Dec
				if d.Unscaled < 0 {
					d.Unscaled = -d.Unscaled
				}
				return Value{Kind: KDec, Dec: d}, nil
			}
			return Value{}, errAt(CodeEval, Pos{}, "abs of %s", a[0].Kind)
		})

	// round_to(x, n) -- half away from zero, n in [0, 6].
	r.RegisterFunction("round_to", []FnArg{{"x", num}, {"n", []Kind{KInt}}}, false,
		func(p Pos, at []Type) (Type, *Error) {
			if at[0].Kind == KInt {
				return IntT, nil
			}
			if at[0].Kind == KDec {
				if at[1].Kind == KInt {
					// n is a constant at check time; the evaluator clamps.
					return at[0], nil
				}
				return at[0], nil
			}
			return NullT, nil
		},
		func(a []Value) (Value, *Error) {
			if a[0].Null || a[1].Null {
				return NullValue(a[0].Kind), nil
			}
			if a[0].Kind == KInt {
				return a[0], nil
			}
			n := a[1].Int
			if n < 0 || n > 6 {
				return Value{}, errAt(CodeEval, Pos{}, "round_to digits must be between 0 and 6")
			}
			d, ok := a[0].Dec.rescale(int(n))
			if !ok {
				return Value{}, errAt(CodeEval, Pos{}, "round_to overflow")
			}
			return Value{Kind: KDec, Dec: d}, nil
		})

	// from_clock(tick, base_date) -> date. The base date is the auction day;
	// the tick is interpreted as a clock reading and converted to a date by
	// adding base + (tick / 24h)? No: a tick is a price, not a time. Instead
	// from_clock maps the tick onto a date within the auction week: the clock
	// face is divided into auction days. The canonical explicit conversion is
	// from_clock(tick) with an optional base. We define it as base + floor(tick)
	// days, so `from_clock(to_clock(d, base)) = d` for dates >= base within 101
	// days, which the verifier checks.
	r.RegisterFunction("from_clock", []FnArg{{"tick", []Kind{KClock}}}, true,
		func(p Pos, at []Type) (Type, *Error) {
			if len(at) > 2 {
				return Type{}, errAt(CodeArity, p, "from_clock takes at most 2 arguments")
			}
			if len(at) == 2 && !at[1].IsNull() && at[1].Kind != KDate {
				return Type{}, errAt(CodeType, p, "from_clock base must be a date, got %s", at[1])
			}
			return DateT, nil
		},
		func(a []Value) (Value, *Error) {
			if a[0].Null || (len(a) > 1 && a[1].Null) {
				return NullValue(KDate), nil
			}
			base := int32(0)
			if len(a) > 1 {
				base = a[1].Date
			}
			return Value{Kind: KDate, Date: base + a[0].Clock/100}, nil
		})

	r.RegisterFunction("to_clock", []FnArg{{"d", []Kind{KDate}}, {"base", []Kind{KDate}}}, true,
		func(p Pos, at []Type) (Type, *Error) {
			if len(at) < 2 {
				return Type{}, errAt(CodeArity, p, "to_clock requires a base date: to_clock(date, base)")
			}
			return ClockT, nil
		},
		func(a []Value) (Value, *Error) {
			if a[0].Null || a[1].Null {
				return NullValue(KClock), nil
			}
			diff := int64(a[0].Date - a[1].Date)
			if diff < 0 || diff*100 > TickMax {
				return Value{}, errAt(CodeEval, Pos{}, "date %s is outside the 101-day clock window from base %s",
					FormatDate(a[0].Date), FormatDate(a[1].Date))
			}
			return Value{Kind: KClock, Clock: int32(diff * 100)}, nil
		})

	r.RegisterFunction("date_add", []FnArg{{"d", []Kind{KDate}}, {"days", []Kind{KInt}}}, false,
		func(Pos, []Type) (Type, *Error) { return DateT, nil },
		func(a []Value) (Value, *Error) {
			if a[0].Null || a[1].Null {
				return NullValue(KDate), nil
			}
			return Value{Kind: KDate, Date: a[0].Date + int32(a[1].Int)}, nil
		})
}

// RegisterAuction installs the auction-specific functions. It is deliberately
// an ordinary library: any deployment can add its own the same way.
func RegisterAuction(r *Registry) {
	// tick_round(price.tick) -> tick rounded to the nearest whole tick unit
	// (0.01), half away from zero. Prices are already at 0.01 resolution at
	// minimum; this is the canonical "round to the clock face" primitive.
	r.RegisterFunction("tick_round", []FnArg{{"t", []Kind{KClock}}}, false,
		func(Pos, []Type) (Type, *Error) { return ClockT, nil },
		func(a []Value) (Value, *Error) {
			if a[0].Null {
				return NullValue(KClock), nil
			}
			return a[0], nil
		})

	// tick_bucket(t, step_ticks) -> tick. Snaps a clock value down to a bucket
	// boundary. step must be a positive integer tick count.
	r.RegisterFunction("tick_bucket", []FnArg{{"t", []Kind{KClock}}, {"step", []Kind{KInt}}}, false,
		func(p Pos, at []Type) (Type, *Error) {
			if at[1].Kind == KInt && at[1].Kind != KNull {
				return ClockT, nil
			}
			if at[1].IsNull() {
				return ClockT, nil
			}
			return Type{}, errAt(CodeType, p, "tick_bucket step must be an integer tick count")
		},
		func(a []Value) (Value, *Error) {
			if a[0].Null || a[1].Null {
				return NullValue(KClock), nil
			}
			if a[1].Int <= 0 {
				return Value{}, errAt(CodeEval, Pos{}, "tick_bucket step must be positive")
			}
			return Value{Kind: KClock, Clock: a[0].Clock - a[0].Clock%int32(a[1].Int)}, nil
		})

	// tick_span(t1, t2) -> decimal(3,2): absolute distance down the clock.
	r.RegisterFunction("tick_span", []FnArg{{"a", []Kind{KClock}}, {"b", []Kind{KClock}}}, false,
		func(Pos, []Type) (Type, *Error) { return DecType(5, 2), nil },
		func(a []Value) (Value, *Error) {
			if a[0].Null || a[1].Null {
				return NullValue(KDec), nil
			}
			d := int32(a[0].Clock - a[1].Clock)
			if d < 0 {
				d = -d
			}
			return Value{Kind: KDec, Dec: Decimal{int64(d), 2}}, nil
		})

	// price_to_cent(t) -> int: whole cents, rounded half away from zero.
	r.RegisterFunction("price_to_cent", []FnArg{{"t", []Kind{KClock}}}, false,
		func(Pos, []Type) (Type, *Error) { return IntT, nil },
		func(a []Value) (Value, *Error) {
			if a[0].Null {
				return NullValue(KInt), nil
			}
			return Value{Kind: KInt, Int: int64(a[0].Clock)}, nil
		})
}

// CheckCall is the shared entry point used by the binder for all functions.
func (r *Registry) CheckCall(p Pos, name string, args []Type) (Type, *Error) {
	d, ok := r.Lookup(name)
	if !ok {
		return Type{}, errAt(CodeUnknownFn, p, "unknown function %q", name)
	}
	return d.Sig.CheckArgs(p, args)
}

// EvalCall evaluates a registered function. Arity is re-checked here because
// constant folding can reach EvalCall with a partially bound call.
func (r *Registry) EvalCall(p Pos, name string, args []Value) (Value, *Error) {
	d, ok := r.Lookup(name)
	if !ok {
		return Value{}, errAt(CodeUnknownFn, p, "unknown function %q", name)
	}
	if (!d.Sig.Variadic && len(args) != len(d.Sig.Args)) || (d.Sig.Variadic && len(args) < len(d.Sig.Args)) {
		return Value{}, errAt(CodeArity, p, "%s called with %d argument(s)", name, len(args))
	}
	v, e := d.Fn(args)
	if e != nil {
		if e.Pos.Line == 0 {
			e.Pos = p
		}
		return Value{}, e
	}
	return v, nil
}
