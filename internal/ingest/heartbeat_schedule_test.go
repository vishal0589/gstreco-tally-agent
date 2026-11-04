package ingest

import (
	"encoding/json"
	"testing"
)

// QA 04-Nov N3 wire contract: optional key `schedule`:
// {"cron": "<spec actually in use>", "source": "config"|"default"|"override"}.
func TestHeartbeatRequest_ScheduleJSONShape(t *testing.T) {
	for _, src := range []string{ScheduleSourceConfig, ScheduleSourceDefault, ScheduleSourceOverride} {
		body, err := json.Marshal(HeartbeatRequest{
			AgentVersion: "v0.1.51",
			Schedule:     &ScheduleReport{Cron: "0 11,13,15,17 * * *", Source: src},
		})
		if err != nil {
			t.Fatal(err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			t.Fatal(err)
		}
		var sch map[string]any
		if err := json.Unmarshal(top["schedule"], &sch); err != nil {
			t.Fatalf("%s: schedule = %s: %v", src, top["schedule"], err)
		}
		if len(sch) != 2 || sch["cron"] != "0 11,13,15,17 * * *" || sch["source"] != src {
			t.Fatalf("%s: schedule = %v, want exactly cron + source", src, sch)
		}
	}
	if ScheduleSourceConfig != "config" || ScheduleSourceDefault != "default" || ScheduleSourceOverride != "override" {
		t.Fatal("source names are the wire contract")
	}
}

func TestHeartbeatRequest_ScheduleOmittedWhenNil(t *testing.T) {
	body, _ := json.Marshal(HeartbeatRequest{AgentVersion: "v"})
	var top map[string]json.RawMessage
	_ = json.Unmarshal(body, &top)
	if _, ok := top["schedule"]; ok {
		t.Fatalf("body = %s, want no schedule key", body)
	}
}
