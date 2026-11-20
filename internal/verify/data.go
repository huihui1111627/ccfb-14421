package verify

import (
	"math/rand"

	"clockql/internal/engine"
)

// DataOptions controls random dataset generation.
type DataOptions struct {
	Lots    int
	Trades  int
	Bidders int
	Days    int
	// NullRate is the chance a nullable column is NULL.
	NullRate float64
	// DupRate is the chance an inserted row repeats an existing key,
	// producing duplicate rows in results.
	DupRate float64
}

func DefaultDataOptions() DataOptions {
	return DataOptions{Lots: 24, Trades: 40, Bidders: 8, Days: 6, NullRate: 0.18, DupRate: 0.15}
}

// DatasetSpec is the generated dataset plus the catalog it matches.
type DatasetSpec struct {
	DS  *engine.Dataset
	Cat *engine.Catalog
}

// GenerateData builds a random dataset for the auction schema. Every value is
// drawn to exercise NULLs, duplicates, fixed-point rounding and clock edges.
func GenerateData(seed int64, opts DataOptions) *DatasetSpec {
	r := rand.New(rand.NewSource(seed))
	ds := engine.NewDataset()
	cat := engine.AuctionCatalog()

	growers := []string{"Aalsmeer", "Rijnsburg", "Naaldwijk", "Bleiswijk"}
	varieties := []string{"rose", "tulip", "carnation", "hydrangea", "peony"}
	grades := []string{"A1", "A2", "B1", "B2"}
	regions := []string{"north", "south", "east", "west"}
	buyers := []string{"B1", "B2", "B3", "B4", "B5", "B6"}

	intn := func(n int) int {
		if n <= 0 {
			return 0
		}
		return r.Intn(n)
	}
	chance := func(p float64) bool { return r.Float64() < p }
	dateAt := func(day int) int32 { return int32(19000 + day) }

	var lotRows []engine.Row
	lotDays := map[int64]int{}
	for i := 0; i < opts.Lots; i++ {
		day := intn(opts.Days)
		id := int64(i + 1)
		if chance(opts.DupRate) && len(lotRows) > 0 {
			// Duplicate an existing row to create duplicate result rows.
			old := lotRows[intn(len(lotRows))]
			lotRows = append(lotRows, append(engine.Row{}, old...))
			continue
		}
		lotDays[id] = day
		stems := int64(1 + intn(500))
		base := int32(intn(engine.TickMax + 1))
		var stock engine.Value
		if chance(opts.NullRate) {
			stock = engine.NullValue(engine.KInt)
		} else {
			stock = engine.Value{Kind: engine.KInt, Int: int64(intn(1000))}
		}
		lotRows = append(lotRows, engine.Row{
			{Kind: engine.KInt, Int: id},
			{Kind: engine.KString, Str: growers[intn(len(growers))]},
			{Kind: engine.KString, Str: varieties[intn(len(varieties))]},
			{Kind: engine.KInt, Int: stems},
			{Kind: engine.KString, Str: grades[intn(len(grades))]},
			{Kind: engine.KClock, Clock: base},
			{Kind: engine.KDate, Date: dateAt(day)},
			stock,
		})
	}
	ds.Add(&engine.Table{Name: "lots", Cols: []string{"lot_id", "grower", "variety", "stems", "grade", "base_tick", "sold_date", "stock"}, Rows: lotRows})

	var tradeRows []engine.Row
	for i := 0; i < opts.Trades; i++ {
		lotID := int64(1 + intn(opts.Lots))
		day, ok := lotDays[lotID]
		if !ok {
			day = intn(opts.Days)
		}
		var disc engine.Value
		if chance(opts.NullRate) {
			disc = engine.NullValue(engine.KDec)
		} else {
			// Two decimals from 0.00 to 99.99, occasionally with a third
			// decimal digit to force scale coercion when read.
			disc = engine.Value{Kind: engine.KDec, Dec: engine.Decimal{Unscaled: int64(intn(10000)), Scale: 2}}
		}
		var note engine.Value
		switch {
		case chance(opts.NullRate):
			note = engine.NullValue(engine.KString)
		case chance(0.1):
			note = engine.Value{Kind: engine.KString, Str: ""}
		default:
			note = engine.Value{Kind: engine.KString, Str: varieties[intn(len(varieties))]}
		}
		tradeRows = append(tradeRows, engine.Row{
			{Kind: engine.KInt, Int: int64(i + 1)},
			{Kind: engine.KInt, Int: lotID},
			{Kind: engine.KString, Str: buyers[intn(len(buyers))]},
			{Kind: engine.KClock, Clock: int32(intn(engine.TickMax + 1))},
			{Kind: engine.KInt, Int: int64(1 + intn(400))},
			{Kind: engine.KDate, Date: dateAt(day)},
			disc,
			note,
		})
	}
	ds.Add(&engine.Table{Name: "trades", Cols: []string{"trade_id", "lot_id", "buyer", "price_tick", "stems", "trade_date", "discount", "note"}, Rows: tradeRows})

	var bidRows []engine.Row
	for _, b := range buyers {
		var credit engine.Value
		if chance(0.3) {
			credit = engine.NullValue(engine.KDec)
		} else {
			credit = engine.Value{Kind: engine.KDec, Dec: engine.Decimal{Unscaled: int64(1 + intn(900000)), Scale: 2}}
		}
		bidRows = append(bidRows, engine.Row{
			{Kind: engine.KString, Str: b},
			{Kind: engine.KString, Str: regions[intn(len(regions))]},
			credit,
		})
	}
	ds.Add(&engine.Table{Name: "bidders", Cols: []string{"buyer", "region", "credit"}, Rows: bidRows})

	var clockRows []engine.Row
	for d := 0; d < opts.Days; d++ {
		open := int32(intn(engine.TickMax/2) + engine.TickMax/2)
		close := int32(intn(int(open) + 1))
		floor := int32(intn(int(close) + 1))
		clockRows = append(clockRows, engine.Row{
			{Kind: engine.KDate, Date: dateAt(d)},
			{Kind: engine.KClock, Clock: open},
			{Kind: engine.KClock, Clock: close},
			{Kind: engine.KClock, Clock: floor},
		})
	}
	ds.Add(&engine.Table{Name: "clock", Cols: []string{"day", "open_tick", "close_tick", "floor_tick"}, Rows: clockRows})

	return &DatasetSpec{DS: ds, Cat: cat}
}
