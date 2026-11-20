package engine

import (
	"fmt"
	"sort"
	"strings"
)

// ColumnDef is a catalog column.
type ColumnDef struct {
	Name     string
	Type     Type
	Nullable bool
}

// TableDef is a catalog table.
type TableDef struct {
	Name    string
	Columns []ColumnDef
	// Auction notes shown in the UI.
	Note string
}

// Catalog maps table names to definitions.
type Catalog struct {
	tables map[string]*TableDef
	order  []string
}

func NewCatalog() *Catalog { return &Catalog{tables: map[string]*TableDef{}} }

func (c *Catalog) Add(t *TableDef) {
	key := strings.ToLower(t.Name)
	if _, ok := c.tables[key]; !ok {
		c.order = append(c.order, key)
	}
	c.tables[key] = t
}

func (c *Catalog) Get(name string) (*TableDef, bool) {
	t, ok := c.tables[strings.ToLower(name)]
	return t, ok
}

func (c *Catalog) Tables() []*TableDef {
	out := make([]*TableDef, 0, len(c.order))
	names := append([]string(nil), c.order...)
	sort.Strings(names)
	for _, n := range names {
		out = append(out, c.tables[n])
	}
	return out
}

// AuctionCatalog is the reference schema for the Dutch flower auction.
func AuctionCatalog() *Catalog {
	c := NewCatalog()
	c.Add(&TableDef{
		Name: "lots",
		Note: "lots on the clock; base_tick is the opening (highest) clock price",
		Columns: []ColumnDef{
			{"lot_id", IntT, false},
			{"grower", StrT, false},
			{"variety", StrT, false},
			{"stems", IntT, false},
			{"grade", StrT, false},
			{"base_tick", ClockT, false},
			{"sold_date", DateT, false},
			{"stock", IntT, true},
		},
	})
	c.Add(&TableDef{
		Name: "trades",
		Note: "executed ticks; price_tick is the clock value when the lot sold",
		Columns: []ColumnDef{
			{"trade_id", IntT, false},
			{"lot_id", IntT, false},
			{"buyer", StrT, false},
			{"price_tick", ClockT, false},
			{"stems", IntT, false},
			{"trade_date", DateT, false},
			{"discount", DecType(6, 2), true},
			{"note", StrT, true},
		},
	})
	c.Add(&TableDef{
		Name: "bidders",
		Note: "registered buyers",
		Columns: []ColumnDef{
			{"buyer", StrT, false},
			{"region", StrT, false},
			{"credit", DecType(12, 2), true},
		},
	})
	c.Add(&TableDef{
		Name: "clock",
		Note: "daily clock calibration",
		Columns: []ColumnDef{
			{"day", DateT, false},
			{"open_tick", ClockT, false},
			{"close_tick", ClockT, false},
			{"floor_tick", ClockT, false},
		},
	})
	return c
}

// Aggregate signatures. They are checked with the same machinery as scalar
// functions but live in their own table because they change cardinality.
type AggSig struct {
	Name     string
	MinArgs  int
	MaxArgs  int
	ArgKinds []Kind
	Distinct bool
}

var aggSigs = map[string]AggSig{
	"count": {Name: "count", MinArgs: 0, MaxArgs: 1, ArgKinds: nil, Distinct: true},
	"sum":   {Name: "sum", MinArgs: 1, MaxArgs: 1, ArgKinds: []Kind{KInt, KDec}, Distinct: true},
	"min":   {Name: "min", MinArgs: 1, MaxArgs: 1, ArgKinds: []Kind{KInt, KDec, KString, KDate, KClock}, Distinct: true},
	"max":   {Name: "max", MinArgs: 1, MaxArgs: 1, ArgKinds: []Kind{KInt, KDec, KString, KDate, KClock}, Distinct: true},
	"avg":   {Name: "avg", MinArgs: 1, MaxArgs: 1, ArgKinds: []Kind{KInt, KDec}, Distinct: true},
}

func isAggregateName(name string) bool {
	_, ok := aggSigs[strings.ToLower(name)]
	return ok
}

// aggReturnType performs the shared type check for an aggregate call and
// returns its result type.
func aggReturnType(p Pos, name string, argT Type, hasArg bool, star bool, distinct bool) (Type, *Error) {
	sig := aggSigs[strings.ToLower(name)]
	if star && !strings.EqualFold(name, "count") {
		return Type{}, errAt(CodeType, p, "%s(*) is not a valid aggregate; only count(*) takes *", name)
	}
	if star && distinct {
		return Type{}, errAt(CodeType, p, "COUNT(DISTINCT *) is not valid")
	}
	if !star && !hasArg {
		return Type{}, errAt(CodeArity, p, "%s requires an argument", name)
	}
	if hasArg && !star {
		if len(sig.ArgKinds) > 0 && !argT.IsNull() {
			ok := false
			// Numeric kinds promote: sum(int+dec expr) is numeric.
			for _, k := range sig.ArgKinds {
				if k == argT.Kind || (k == KDec && argT.Kind == KInt) {
					ok = true
				}
			}
			if !ok {
				hints := []string(nil)
				if argT.Kind == KClock || argT.Kind == KDate {
					hints = []string{"cast clock/date values explicitly if you mean to aggregate them numerically"}
				}
				return Type{}, errWithHints(CodeType, p, fmt.Sprintf("%s does not accept %s", name, argT), hints)
			}
		}
	}
	switch strings.ToLower(name) {
	case "count":
		return IntT, nil
	case "sum":
		if !hasArg || argT.IsNull() {
			return DecType(18, 6), nil
		}
		if argT.Kind == KInt {
			return IntT, nil
		}
		if argT.Kind == KDec {
			s := argT.Scale
			if s > 10 {
				s = 10
			}
			return DecType(18, s), nil
		}
		return Type{}, errAt(CodeType, p, "sum requires a numeric argument, got %s", argT)
	case "avg":
		if !hasArg || argT.IsNull() {
			return DecType(18, 6), nil
		}
		if argT.Kind == KInt {
			return DecType(18, 6), nil
		}
		s := argT.Scale + 4
		if s > 10 {
			s = 10
		}
		return DecType(18, s), nil
	case "min", "max":
		if !hasArg || argT.IsNull() {
			return NullT, nil
		}
		return argT, nil
	}
	return Type{}, errAt(CodeInternal, p, "unknown aggregate %s", name)
}
