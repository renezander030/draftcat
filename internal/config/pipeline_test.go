package config

import "testing"

func TestPipelineLookup(t *testing.T) {
	cfg := &Config{Pipelines: []PipelineConfig{
		{Name: "digest", Schedule: "daily"},
		{Name: "leads", Budget: PipelineBudgetConfig{PerDayTokens: 500}},
		{Name: "digest", Schedule: "hourly"},
	}}
	if p, ok := cfg.Pipeline("digest"); !ok || p.Schedule != "daily" {
		t.Fatalf("Pipeline(digest) = %+v, %v; want the first match", p, ok)
	}
	if _, ok := cfg.Pipeline("missing"); ok {
		t.Fatal("unknown pipeline was found")
	}
	if _, ok := (*Config)(nil).Pipeline("digest"); ok {
		t.Fatal("nil config found a pipeline")
	}
	if got := cfg.PipelineBudget("leads").PerDayTokens; got != 500 {
		t.Fatalf("PipelineBudget(leads) tokens = %d", got)
	}
	if got := cfg.PipelineBudget("missing"); got.Capped() {
		t.Fatalf("PipelineBudget(missing) = %+v", got)
	}
	if got := (*Config)(nil).PipelineBudget("leads"); got.Capped() {
		t.Fatalf("nil config PipelineBudget = %+v", got)
	}
}
