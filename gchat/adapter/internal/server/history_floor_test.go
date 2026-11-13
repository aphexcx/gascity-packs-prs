package server

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStatusNamesTheHistoryFloor(t *testing.T) {
	f := setup(t)
	code, raw, _ := f.call(t, "/status", nil)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if code != 200 || fields["history_floor"] != nil {
		t.Fatalf("legacy floor: %s", raw)
	}
	floor := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	data, _ := json.Marshal(map[string]any{"history_floor": floor})
	f.mu.Lock()
	err := json.Unmarshal(data, f.l)
	f.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	code, raw, _ = f.call(t, "/status", nil)
	var status struct {
		Floor time.Time `json:"history_floor"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if code != 200 || !status.Floor.Equal(floor) {
		t.Fatalf("status floor=%s", status.Floor)
	}
}
