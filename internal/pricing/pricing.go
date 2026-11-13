// Package pricing computes USD cost from token counts.
package pricing

import "math"

// Price is USD per 1M tokens.
type Price struct {
	InputPer1M  float64
	OutputPer1M float64
}

// Table maps model name to price. Models not in the table cost zero
// (local Ollama models, mock).
type Table struct {
	prices map[string]Price
}

// defaults are list prices at the time of writing and are meant to be
// overridden through the `pricing:` config section when they drift.
var defaults = map[string]Price{
	"gpt-4o-mini":       {InputPer1M: 0.15, OutputPer1M: 0.60},
	"gpt-4o":            {InputPer1M: 2.50, OutputPer1M: 10.00},
	"gpt-4.1-mini":      {InputPer1M: 0.40, OutputPer1M: 1.60},
	"claude-haiku-5-5":  {InputPer1M: 1.00, OutputPer1M: 5.00},
	"claude-sonnet-5-5": {InputPer1M: 3.00, OutputPer1M: 15.00},
}

// New returns a table with built-in defaults overlaid by overrides.
func New(overrides map[string]Price) *Table {
	m := make(map[string]Price, len(defaults)+len(overrides))
	for k, v := range defaults {
		m[k] = v
	}
	for k, v := range overrides {
		m[k] = v
	}
	return &Table{prices: m}
}

// Cost returns the USD cost of a call.
func (t *Table) Cost(model string, inputTokens, outputTokens int) float64 {
	p, ok := t.prices[model]
	if !ok {
		return 0
	}
	return (float64(inputTokens)*p.InputPer1M + float64(outputTokens)*p.OutputPer1M) / 1e6
}

// Micros converts USD to integer micro-dollars (the unit stored in Redis).
func Micros(usd float64) int64 {
	return int64(math.Round(usd * 1e6))
}
