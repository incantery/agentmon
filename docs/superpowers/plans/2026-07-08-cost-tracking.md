# agentmon: Token & Cost Tracking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every `assistant_message` event carries a computed `cost_usd` (from per-model pricing over input/output/cache tokens), the parser captures the 5m/1h cache-write split the transcripts already record, and the Grafana dashboard gains cost + token panels.

**Architecture:** A new pure `internal/pricing` package holds the per-model rate table (values sourced from Anthropic's published pricing, cached 2026-06-24 — cite in comments) with longest-prefix model matching. The parser (`internal/transcript`) calls it at derivation time and stamps `cost_usd` on `AssistantMessagePayload` — costs are computed once, at ingest, and flow to Loki inside the JSON line where LogQL `unwrap` can aggregate them. Unknown models yield cost 0 and are NEVER guessed; the payload carries no cost field so dashboards can distinguish "free" from "unpriced".

**Tech Stack:** Go 1.25 stdlib; LogQL panels in the existing provisioned dashboard.

## Global Constraints

- Pricing values are exactly these (USD per MTok; from the claude-api reference, cached 2026-06-24 — Sonnet 5 has intro pricing $2/$10 through 2026-08-31, we use list prices deliberately):

| model prefix | input | output | cache read (0.1×) | cache write 5m (1.25×) | cache write 1h (2×) |
|---|---|---|---|---|---|
| `claude-fable-5`, `claude-mythos-5` | 10.00 | 50.00 | 1.00 | 12.50 | 20.00 |
| `claude-opus-4-8`, `claude-opus-4-7`, `claude-opus-4-6` | 5.00 | 25.00 | 0.50 | 6.25 | 10.00 |
| `claude-sonnet-5`, `claude-sonnet-4-6` | 3.00 | 15.00 | 0.30 | 3.75 | 6.00 |
| `claude-haiku-4-5` | 1.00 | 5.00 | 0.10 | 1.25 | 2.00 |

- Longest-prefix match on the model string (handles date-suffixed IDs like `claude-haiku-4-5-20251001`). No match → `(0, false)` — never guess a price.
- Cache-write cost: use the 5m/1h split when the transcript provides it (`usage.cache_creation.ephemeral_5m_input_tokens` / `ephemeral_1h_input_tokens`); when absent (older lines), treat ALL of `cache_creation_input_tokens` as 5m-TTL (1.25×) — the conservative-low default, documented.
- `cost_usd` is a float64 with `omitempty`-like semantics via a pointer? NO — use plain `float64` + a separate `priced` distinction: the field is `cost_usd` and is ONLY set (non-zero-or-present) when the model priced; unpriced events omit it entirely (pointer field, `omitempty`). Dashboards use `__error__=""` filters so old/unpriced events don't break queries.
- Redaction: new fields are numeric — the metadata level keeps them (cost IS the metadata). The redact property test must keep passing without allowlist changes.
- Determinism: cost math must be integer-safe — compute in micro-dollars-per-token style using float64 only at the end, OR compute as `float64(tokens) * rate / 1e6`; either is fine, but the SAME event must always produce byte-identical JSON (Loki dedupe). float64 formatting via encoding/json is deterministic for a given value — acceptable.
- Every task ends with `go build ./... && go test ./...` green and a commit.

---

### Task 1: `internal/pricing`

**Files:**
- Create: `internal/pricing/pricing.go`
- Test: `internal/pricing/pricing_test.go`

**Interfaces:**
- Produces:
  - `type Usage struct { InputTokens, OutputTokens, CacheReadTokens, Cache5mWriteTokens, Cache1hWriteTokens int64 }`
  - `func Cost(model string, u Usage) (usd float64, priced bool)` — longest-prefix match against the table; `(0, false)` for unknown models.

- [ ] **Step 1: Write the failing tests**

`internal/pricing/pricing_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/pricing/`
Expected: FAIL — package doesn't exist.

- [ ] **Step 3: Implement**

`internal/pricing/pricing.go`:

```go
// Package pricing computes the notional API cost of Claude usage from
// per-model published rates (USD per million tokens). Values are from
// Anthropic's published pricing as cached 2026-06-24; Sonnet 5 has
// introductory pricing ($2/$10) through 2026-08-31 — we use list prices
// deliberately (stable, conservative overestimate). Unknown models are
// never guessed: Cost returns priced=false and callers must surface
// "unpriced" rather than zero-cost.
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/pricing/`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/pricing/
git commit -m "feat(pricing): per-model rate table with longest-prefix match — unknown models never guessed"
```

---

### Task 2: Parser — cache-TTL split + cost stamping

**Files:**
- Modify: `internal/transcript/event.go` (AssistantMessagePayload fields)
- Modify: `internal/transcript/parser.go` (usage extraction + cost)
- Test: `internal/transcript/parser_test.go` (append/adjust)

**Interfaces:**
- Consumes: `pricing.Cost`, `pricing.Usage`.
- Produces: `AssistantMessagePayload` gains:
  - `Cache5mTokens int64` (tag `cache_5m_tokens,omitempty`) and `Cache1hTokens int64` (tag `cache_1h_tokens,omitempty`) — from `usage.cache_creation.ephemeral_5m_input_tokens` / `ephemeral_1h_input_tokens`.
  - `CostUSD *float64` (tag `cost_usd,omitempty`) — set only when the model priced; nil (omitted from JSON) when unpriced. Pointer so "unpriced" is distinguishable from "$0.00".

**Semantics:** if the 5m/1h split is present and sums > 0, use it for cost; else treat all `cache_creation_input_tokens` as 5m. The existing `CacheCreationTokens` field keeps its meaning (total).

- [ ] **Step 1: Write the failing tests**

Append to `internal/transcript/parser_test.go`:

```go
func TestAssistantCostStamping(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"assistant","message":{"model":"claude-fable-5","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":6199,"output_tokens":383,"cache_read_input_tokens":18341,"cache_creation_input_tokens":3899,"cache_creation":{"ephemeral_5m_input_tokens":3000,"ephemeral_1h_input_tokens":899}}},"timestamp":"2026-07-08T10:00:00.000Z","sessionId":"sess-1"}`,
	)
	am := got[1].Payload.(AssistantMessagePayload)
	if am.Cache5mTokens != 3000 || am.Cache1hTokens != 899 {
		t.Errorf("cache split = %d/%d", am.Cache5mTokens, am.Cache1hTokens)
	}
	if am.CostUSD == nil {
		t.Fatal("fable-5 must be priced")
	}
	want := 6199*10.0/1e6 + 383*50.0/1e6 + 18341*1.0/1e6 + 3000*12.5/1e6 + 899*20.0/1e6
	if diff := *am.CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %v want %v", *am.CostUSD, want)
	}
}

func TestAssistantCostFallbackWithoutSplit(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":10,"cache_creation_input_tokens":1000}},"sessionId":"sess-1"}`,
	)
	am := got[1].Payload.(AssistantMessagePayload)
	if am.CostUSD == nil {
		t.Fatal("haiku must be priced via prefix")
	}
	// no split → all cache_creation treated as 5m (1.25×)
	want := 100*1.0/1e6 + 10*5.0/1e6 + 1000*1.25/1e6
	if diff := *am.CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %v want %v", *am.CostUSD, want)
	}
}

func TestUnknownModelUnpriced(t *testing.T) {
	p := NewParser("sess-1")
	got := collect(t, p,
		`{"type":"assistant","message":{"model":"qwen3:8b","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":10}},"sessionId":"sess-1"}`,
	)
	am := got[1].Payload.(AssistantMessagePayload)
	if am.CostUSD != nil {
		t.Errorf("unknown model must be unpriced (nil), got %v", *am.CostUSD)
	}
	// and the JSON must omit the field entirely
	b, _ := json.Marshal(am)
	if strings.Contains(string(b), "cost_usd") {
		t.Errorf("unpriced payload must omit cost_usd: %s", b)
	}
}
```

(add `strings` to imports if missing; `json` is already imported in the test file — verify.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/transcript/`
Expected: FAIL — `am.Cache5mTokens` undefined.

- [ ] **Step 3: Implement**

In `internal/transcript/event.go`, extend `AssistantMessagePayload` (after `CacheCreationTokens`):

```go
	Cache5mTokens int64 `json:"cache_5m_tokens,omitempty"`
	Cache1hTokens int64 `json:"cache_1h_tokens,omitempty"`
	// CostUSD is the notional API cost at published rates, stamped at
	// derivation. nil = the model isn't in the pricing table (unpriced,
	// NOT free) — the field is omitted from JSON so dashboards can tell
	// the difference.
	CostUSD *float64 `json:"cost_usd,omitempty"`
```

In `internal/transcript/parser.go`:

1. Extend `rawMessage.Usage` with the nested split:

```go
		CacheCreation struct {
			Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
			Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
```

2. In `assistantPayloads`, after building `am` (import `github.com/incantery/agentmon/internal/pricing`):

```go
	am.Cache5mTokens = msg.Usage.CacheCreation.Ephemeral5m
	am.Cache1hTokens = msg.Usage.CacheCreation.Ephemeral1h
	u := pricing.Usage{
		InputTokens:        am.InputTokens,
		OutputTokens:       am.OutputTokens,
		CacheReadTokens:    am.CacheReadTokens,
		Cache5mWriteTokens: am.Cache5mTokens,
		Cache1hWriteTokens: am.Cache1hTokens,
	}
	if u.Cache5mWriteTokens+u.Cache1hWriteTokens == 0 {
		// Older lines carry only the total: bill it all at the cheaper
		// 5m rate (conservative-low, documented in the plan).
		u.Cache5mWriteTokens = am.CacheCreationTokens
	}
	if usd, priced := pricing.Cost(am.Model, u); priced {
		am.CostUSD = &usd
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./...`
Expected: PASS everywhere — including the redact property test (new fields are numeric/pointer-to-numeric; no allowlist change needed) and all existing assistant-line tests (their payload `==` comparisons still work: `CostUSD` is a pointer, and existing fixtures use `claude-fable-5`… **CHECK**: `TestAssistantMessageWithToolUse` compares `am != want` with `want` lacking CostUSD — that comparison will now FAIL because the parser stamps CostUSD. Update those existing tests minimally: compare fields individually or set `want.CostUSD = am.CostUSD` after asserting it's non-nil, with a comment. State the adaptation in your report.)

- [ ] **Step 5: Commit**

```bash
git add internal/transcript/
git commit -m "feat(transcript): cache-TTL split + cost_usd stamping on assistant messages"
```

---

### Task 3: Dashboard — cost & token panels

**Files:**
- Modify: `deploy/k8s/grafana/dashboards/agentmon.json`

**Interfaces:** none in Go. Add four panels below the existing rows (y: 18), keeping existing panel ids/gridPos untouched; new ids start at 6:

```json
    {
      "id": 6,
      "type": "stat",
      "title": "Cost (24h, USD)",
      "gridPos": { "h": 7, "w": 6, "x": 0, "y": 18 },
      "datasource": { "type": "loki", "uid": "loki" },
      "fieldConfig": { "defaults": { "unit": "currencyUSD", "decimals": 2 } },
      "targets": [
        {
          "refId": "A",
          "expr": "sum(sum_over_time({job=\"agentmon\", type=\"assistant_message\"} | json | unwrap payload_cost_usd | __error__=\"\" [24h]))"
        }
      ]
    },
    {
      "id": 7,
      "type": "timeseries",
      "title": "Cost/hour by model (USD)",
      "gridPos": { "h": 7, "w": 18, "x": 6, "y": 18 },
      "datasource": { "type": "loki", "uid": "loki" },
      "fieldConfig": { "defaults": { "unit": "currencyUSD" } },
      "targets": [
        {
          "refId": "A",
          "expr": "sum by (payload_model) (sum_over_time({job=\"agentmon\", type=\"assistant_message\"} | json | unwrap payload_cost_usd | __error__=\"\" [1h]))"
        }
      ]
    },
    {
      "id": 8,
      "type": "timeseries",
      "title": "Tokens/hour (output vs cache-read)",
      "gridPos": { "h": 7, "w": 12, "x": 0, "y": 25 },
      "datasource": { "type": "loki", "uid": "loki" },
      "targets": [
        {
          "refId": "A",
          "legendFormat": "output",
          "expr": "sum(sum_over_time({job=\"agentmon\", type=\"assistant_message\"} | json | unwrap payload_output_tokens | __error__=\"\" [1h]))"
        },
        {
          "refId": "B",
          "legendFormat": "cache read",
          "expr": "sum(sum_over_time({job=\"agentmon\", type=\"assistant_message\"} | json | unwrap payload_cache_read_tokens | __error__=\"\" [1h]))"
        }
      ]
    },
    {
      "id": 9,
      "type": "table",
      "title": "Top sessions by cost (24h)",
      "gridPos": { "h": 7, "w": 12, "x": 12, "y": 25 },
      "datasource": { "type": "loki", "uid": "loki" },
      "fieldConfig": { "defaults": { "unit": "currencyUSD", "decimals": 3 } },
      "targets": [
        {
          "refId": "A",
          "instant": true,
          "expr": "topk(10, sum by (session_id) (sum_over_time({job=\"agentmon\", type=\"assistant_message\"} | json | unwrap payload_cost_usd | __error__=\"\" [24h])))"
        }
      ]
    }
```

Note on JSON field flattening: Loki's `| json` flattens nested objects with `_` separators — the envelope's `payload.cost_usd` becomes label `payload_cost_usd`, top-level `session_id` stays `session_id`, `payload.model` becomes `payload_model`. The `__error__=""` guard drops lines where `unwrap` fails (e.g. old events without `cost_usd`), so every panel tolerates pre-feature history.

- [ ] **Step 1: Add the panels** (verbatim above, appended to the `panels` array).

- [ ] **Step 2: Validate + apply**

```bash
python3 -c "import json; d=json.load(open('deploy/k8s/grafana/dashboards/agentmon.json')); assert len(d['panels'])==9; print('dashboard json ok')"
kubectl kustomize deploy/k8s > /dev/null && echo "kustomize renders"
kubectl --context homelab apply -k deploy/k8s | grep dashboard
kubectl --context homelab -n agentmon rollout restart deploy/grafana && kubectl --context homelab -n agentmon rollout status deploy/grafana --timeout=120s
```

(the kubectl steps require the lab context; if unreachable from this environment, note it and leave them for the controller.)

- [ ] **Step 3: Commit**

```bash
git add deploy/
git commit -m "feat(deploy): cost + token dashboard panels (LogQL unwrap over cost_usd)"
```

---

## Done when

- `go test ./...` green; new fable-5/haiku pricing tests pass; unknown models stay unpriced.
- A live `agentmon watch` stamps `cost_usd` on new assistant_message events (verify: `bin/agentmon parse --level metadata <fresh transcript> | grep -m1 cost_usd`).
- Grafana shows the four new panels with data accruing as sessions run.
