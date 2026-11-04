package main

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/scheduler"
)

// QA 04-Nov N3: each source, and the reported cron is what the scheduler
// built from the same selection runWithCtx makes (unchanged).
func TestScheduleSource_EachSource(t *testing.T) {
	cases := []struct {
		name, override, configCron string
		wantCron, wantSource       string
	}{
		{"default", "", "", scheduler.DefaultSpec, ingest.ScheduleSourceDefault},
		{"config", "", "0 9 * * 1-5", "0 9 * * 1-5", ingest.ScheduleSourceConfig},
		{"override wins over config", "0 */6 * * *", "0 9 * * 1-5", "0 */6 * * *", ingest.ScheduleSourceOverride},
		{"override without config", "30 10 * * *", "", "30 10 * * *", ingest.ScheduleSourceOverride},
	}
	for _, tc := range cases {
		// The selection exactly as runWithCtx does it.
		spec := tc.override
		if spec == "" {
			spec = tc.configCron
		}
		s, err := scheduler.New(scheduler.Options{
			Spec:     spec,
			TickFunc: func(context.Context) error { return nil },
			Logger:   zerolog.Nop(),
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := ingest.ScheduleReport{Cron: s.Spec(), Source: scheduleSource(tc.override, tc.configCron)}
		if got.Cron != tc.wantCron || got.Source != tc.wantSource {
			t.Fatalf("%s: schedule = %+v, want {%s %s}", tc.name, got, tc.wantCron, tc.wantSource)
		}
	}
}
