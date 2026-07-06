// Package pricing computes the notional API cost of Claude usage from
// per-model published rates (USD per million tokens). Values are from
// Anthropic's published pricing as cached 2026-06-24; Sonnet 5 has
// introductory pricing ($2/$10) through 2026-08-31 — we use list prices
// deliberately (stable, conservative overestimate). Unknown models are
// never guessed: Cost returns priced=false and callers must surface
// "unpriced" rather than zero-cost.
//
// Changing this table changes the bytes of re-derived events: replayed
// history from before a table change will not dedupe against already-shipped
// lines (Loki dedupes exact bytes only). Same applies to the one-time
// upgrade that introduced cost stamping.
package pricing

import "strings"

// rates are USD per million tokens.
type rates struct {
	Input        float64
	Output       float64
	CacheRead    float64 // 0.1× input
	Cache5mWrite float64 // 1.25× input
	Cache1hWrite float64 // 2× input
}

// table is keyed by model-ID prefix; longest prefix wins (date-suffixed
// IDs like claude-haiku-4-5-20251001 resolve to their family).
var table = map[string]rates{
	"claude-fable-5":    {10, 50, 1.00, 12.50, 20},
	"claude-mythos-5":   {10, 50, 1.00, 12.50, 20},
	"claude-opus-4-8":   {5, 25, 0.50, 6.25, 10},
	"claude-opus-4-7":   {5, 25, 0.50, 6.25, 10},
	"claude-opus-4-6":   {5, 25, 0.50, 6.25, 10},
	"claude-sonnet-5":   {3, 15, 0.30, 3.75, 6},
	"claude-sonnet-4-6": {3, 15, 0.30, 3.75, 6},
	"claude-haiku-4-5":  {1, 5, 0.10, 1.25, 2},
}

type Usage struct {
	InputTokens        int64
	OutputTokens       int64
	CacheReadTokens    int64
	Cache5mWriteTokens int64
	Cache1hWriteTokens int64
}

// Cost returns the USD cost of u under model's published rates.
// priced is false when the model isn't in the table — the caller must
// treat the usage as unpriced, not free.
func Cost(model string, u Usage) (usd float64, priced bool) {
	var best string
	for prefix := range table {
		if strings.HasPrefix(model, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return 0, false
	}
	r := table[best]
	usd = float64(u.InputTokens)*r.Input/1e6 +
		float64(u.OutputTokens)*r.Output/1e6 +
		float64(u.CacheReadTokens)*r.CacheRead/1e6 +
		float64(u.Cache5mWriteTokens)*r.Cache5mWrite/1e6 +
		float64(u.Cache1hWriteTokens)*r.Cache1hWrite/1e6
	return usd, true
}
