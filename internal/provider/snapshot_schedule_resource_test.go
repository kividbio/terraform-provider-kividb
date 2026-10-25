package provider

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	snapshotConfigPath = "/instances/i-1/disk-snapshot-config"
	instancePath       = "/instances/i-1"
)

// GET /instances/:id for a paid database with the given schedule. The route
// answers every column; this is trimmed to the ones the schedule reads.
func scheduledInstanceJSON(enabled bool, cadence string, retention int, mode string) string {
	r := strings.NewReplacer("ENABLED", map[bool]string{true: "true", false: "false"}[enabled],
		"CADENCE", cadence, "RETENTION", strconv.Itoa(retention), "MODE", mode)
	return r.Replace(`{"instance":{"id":"i-1","name":"orders","tier":"essentials","cloud":"aws",
		"region":"eu-central-1","data_size_gb":8,"status":"running","free_tier":false,"shard_count":1,
		"disk_snapshot_enabled":ENABLED,"disk_snapshot_cadence":"CADENCE",
		"disk_snapshot_retention_days":RETENTION,"disk_snapshot_retention_mode":"MODE"}}`)
}

func scheduleVals() map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"instance_id": str("i-1"), "enabled": boolean(true), "cadence": str("hourly"),
		"retention_mode": str("keep_days"), "retention": num(14),
	}
}

func TestSnapshotScheduleCreate(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("PATCH "+snapshotConfigPath, 200, `{"updated":true}`)
	f.answer("GET "+instancePath, 200, scheduledInstanceJSON(true, "hourly", 14, "keep_days"))

	res := doCreate(t, &snapshotScheduleResource{client: f.client()}, scheduleVals())
	failOn(t, res.Diagnostics)
	if n := res.Diagnostics.WarningsCount(); n != 0 {
		t.Errorf("a paid database's schedule runs, so no warning: %v", res.Diagnostics.Warnings())
	}

	sent := f.sent("PATCH " + snapshotConfigPath)
	want := map[string]any{"disk_snapshot_enabled": true, "disk_snapshot_cadence": "hourly",
		"disk_snapshot_retention_days": float64(14), "disk_snapshot_retention_mode": "keep_days"}
	if len(sent) != 1 || len(sent[0].Body) != len(want) {
		t.Fatalf("PATCH body = %+v", sent)
	}
	for k, v := range want {
		if sent[0].Body[k] != v {
			t.Errorf("%s = %v, want %v", k, sent[0].Body[k], v)
		}
	}
	var got snapshotScheduleModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.ID.ValueString() != "i-1" || got.Retention.ValueInt64() != 14 || got.Cadence.ValueString() != "hourly" {
		t.Errorf("state = %+v", got)
	}
}

// The API stores a schedule for a free-tier database and never acts on it.
func TestSnapshotScheduleWarnsOnTheFreeTier(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("PATCH "+snapshotConfigPath, 200, `{"updated":true}`)
	f.answer("GET "+instancePath, 200, strings.Replace(scheduledInstanceJSON(true, "hourly", 14, "keep_days"),
		`"free_tier":false`, `"free_tier":true`, 1))
	res := doCreate(t, &snapshotScheduleResource{client: f.client()}, scheduleVals())
	failOn(t, res.Diagnostics)
	if res.Diagnostics.WarningsCount() != 1 || !strings.Contains(res.Diagnostics.Warnings()[0].Summary(), "Free-tier") {
		t.Errorf("expected the free-tier warning, got %v", res.Diagnostics.Warnings())
	}
}

func TestSnapshotScheduleRead(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET "+instancePath, 200, scheduledInstanceJSON(false, "weekly", 3, "keep_last"))
	vals := scheduleVals()
	vals["id"] = str("i-1")
	res := doRead(t, &snapshotScheduleResource{client: f.client()}, vals)
	failOn(t, res.Diagnostics)
	var got snapshotScheduleModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.Enabled.ValueBool() || got.Cadence.ValueString() != "weekly" || got.Retention.ValueInt64() != 3 ||
		got.RetentionMode.ValueString() != "keep_last" {
		t.Errorf("read did not refresh: %+v", got)
	}

	g := newFakeAPI(t)
	g.answer("GET "+instancePath, 404, `{"error":{"code":"not_found"}}`)
	res = doRead(t, &snapshotScheduleResource{client: g.client()}, vals)
	failOn(t, res.Diagnostics)
	if !res.State.Raw.IsNull() {
		t.Error("the schedule of a database that is gone must be removed from state")
	}
}

// An older API that does not return the columns reads as their defaults.
func TestSnapshotScheduleReadsDefaultsWhenTheFieldsAreMissing(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET "+instancePath, 200, `{"instance":{"id":"i-1","status":"running"}}`)
	res := doRead(t, &snapshotScheduleResource{client: f.client()}, map[string]tftypes.Value{"id": str("i-1"), "instance_id": str("i-1")})
	failOn(t, res.Diagnostics)
	var got snapshotScheduleModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.Cadence.ValueString() != "daily" || got.Retention.ValueInt64() != 5 || got.RetentionMode.ValueString() != "keep_last" {
		t.Errorf("defaults not applied: %+v", got)
	}
}

func TestSnapshotScheduleUpdate(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("PATCH "+snapshotConfigPath, 200, `{"updated":true}`)
	f.answer("GET "+instancePath, 200, scheduledInstanceJSON(true, "daily", 7, "keep_last"))
	prior := scheduleVals()
	prior["id"] = str("i-1")
	plan := scheduleVals()
	plan["cadence"], plan["retention"], plan["retention_mode"] = str("daily"), num(7), str("keep_last")
	res := doUpdate(t, &snapshotScheduleResource{client: f.client()}, prior, plan)
	failOn(t, res.Diagnostics)
	if sent := f.sent("PATCH " + snapshotConfigPath); sent[0].Body["disk_snapshot_retention_days"] != float64(7) {
		t.Errorf("PATCH body = %+v", sent)
	}
}

// Delete turns the schedule off and leaves retention alone, so the snapshots
// it already holds are not expired by a reset to the default.
func TestSnapshotScheduleDeleteOnlyTurnsItOff(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("PATCH "+snapshotConfigPath, 200, `{"updated":true}`)
	vals := scheduleVals()
	vals["id"] = str("i-1")
	failOn(t, doDelete(t, &snapshotScheduleResource{client: f.client()}, vals).Diagnostics)
	sent := f.sent("PATCH " + snapshotConfigPath)
	if len(sent) != 1 || len(sent[0].Body) != 1 || sent[0].Body["disk_snapshot_enabled"] != false {
		t.Errorf("delete must send only disk_snapshot_enabled=false, sent %+v", sent)
	}

	g := newFakeAPI(t)
	g.answer("PATCH "+snapshotConfigPath, 404, `{"error":{"code":"not_found"}}`)
	failOn(t, doDelete(t, &snapshotScheduleResource{client: g.client()}, vals).Diagnostics)
}

func TestSnapshotScheduleImport(t *testing.T) {
	res := doImport(t, &snapshotScheduleResource{}, "i-1")
	failOn(t, res.Diagnostics)
	var got snapshotScheduleModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.InstanceID.ValueString() != "i-1" || got.ID.ValueString() != "i-1" {
		t.Errorf("import state = %+v", got)
	}
}

func TestSnapshotScheduleRefusalIsSurfaced(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("PATCH "+snapshotConfigPath, 403, `{"error":{"code":"forbidden","message":"This API key has the viewer role, which cannot change databases."}}`)
	res := doCreate(t, &snapshotScheduleResource{client: f.client()}, scheduleVals())
	wantError(t, res.Diagnostics, "This API key has the viewer role, which cannot change databases.", "admin role")
}
