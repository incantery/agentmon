package pricing

import (
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCostKnownModels(t *testing.T) {
	cases := []struct {
		model string
		u     Usage
		want  float64
	}{
		// 1M input tokens on fable = $10
		{"claude-fable-5", Usage{InputTokens: 1_000_000}, 10.00},
		// full spread on opus 4.8: 1M of each bucket = 5 + 25 + 0.50 + 6.25 + 10
		{"claude-opus-4-8", Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000, Cache5mWriteTokens: 1_000_000, Cache1hWriteTokens: 1_000_000}, 46.75},
		// date-suffixed haiku resolves via prefix
		{"claude-haiku-4-5-20251001", Usage{OutputTokens: 2_000_000}, 10.00},
		// sonnet 5 at list prices
		{"claude-sonnet-5", Usage{InputTokens: 500_000, OutputTokens: 100_000}, 3.00},
		// realistic small turn on fable: 6199 in, 383 out, 18341 read, 3899 5m-write
		{"claude-fable-5", Usage{InputTokens: 6199, OutputTokens: 383, CacheReadTokens: 18341, Cache5mWriteTokens: 3899}, 6199*10.0/1e6 + 383*50.0/1e6 + 18341*1.0/1e6 + 3899*12.5/1e6},
	}
	for _, c := range cases {
		got, priced := Cost(c.model, c.u)
		if !priced {
			t.Errorf("%s: not priced", c.model)
			continue
		}
		if !almost(got, c.want) {
			t.Errorf("%s: got %v want %v", c.model, got, c.want)
		}
	}
}

func TestUnknownModelsAreNotGuessed(t *testing.T) {
	for _, m := range []string{"", "gpt-4o", "claude-opus-4-1", "claude-3-5-haiku-20241022", "qwen3:8b"} {
		if _, priced := Cost(m, Usage{InputTokens: 1_000_000}); priced {
			t.Errorf("%q must be unpriced (never guess)", m)
		}
	}
}

func TestLongestPrefixWins(t *testing.T) {
	// claude-sonnet-5 must not be shadowed by any shorter overlapping prefix
	usd, priced := Cost("claude-sonnet-5", Usage{InputTokens: 1_000_000})
	if !priced || !almost(usd, 3.00) {
		t.Errorf("got %v %v", usd, priced)
	}
}
