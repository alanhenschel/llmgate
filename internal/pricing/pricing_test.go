package pricing

import (
	"math"
	"testing"
)

func TestCost(t *testing.T) {
	tbl := New(map[string]Price{"custom": {InputPer1M: 10, OutputPer1M: 20}, "gpt-4o-mini": {InputPer1M: 1, OutputPer1M: 2}})
	tests := []struct {
		name     string
		model    string
		in, out  int
		want     float64
		wantMicr int64
	}{
		{"default price", "gpt-4o", 1_000_000, 1_000_000, 12.5, 12_500_000},
		{"override wins", "gpt-4o-mini", 500_000, 250_000, 1.0, 1_000_000},
		{"custom", "custom", 100, 100, 0.003, 3000},
		{"unknown is free", "llama3.2", 123456, 654321, 0, 0},
		{"zero tokens", "gpt-4o", 0, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tbl.Cost(tt.model, tt.in, tt.out)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("cost = %v, want %v", got, tt.want)
			}
			if m := Micros(got); m != tt.wantMicr {
				t.Fatalf("micros = %d, want %d", m, tt.wantMicr)
			}
		})
	}
}
