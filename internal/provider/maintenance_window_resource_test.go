package provider

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const maintenancePath = "/instances/i-1/maintenance"
const maintenanceWindowPath = "/instances/i-1/maintenance-window"

// What GET /instances/:id/maintenance answers, for a window on the given day.
func maintenanceJSON(day, hour int, tz string) string {
	return fmt.Sprintf(`{"window":{"day":%d,"hour":%d,"timezone":%q,"length_hours":4,
		"description":"Weekday %02d:00-%02d:00 (%s)"},"notice_hours":48,
		"next_window_start":"2026-10-26T02:00:00.000Z",
		"planned":[{"kind":"tls_certificate_renewal","starts_at":"2026-10-26T02:00:00.000Z","restarts_nodes":true}]}`,
		day, hour, tz, hour, (hour+4)%24, tz)
}

func TestMaintenanceWindowCreate(t *testing.T) {
	f := newFakeAPI(t)
	f.on("PATCH "+maintenanceWindowPath, func(c apiCall) apiReply {
		return apiReply{200, maintenanceJSON(1, 22, "Europe/Berlin")}
	})

	res := dsDoCreate(t, &maintenanceWindowResource{client: f.client()}, map[string]tftypes.Value{
		"instance_id": dsStr("i-1"), "day": dsStr("Monday"), "hour": num(22), "timezone": dsStr("Europe/Berlin"),
	})
	failOn(t, res.Diagnostics)

	sent := f.sent("PATCH " + maintenanceWindowPath)
	if len(sent) != 1 || sent[0].Body["day"] != float64(1) || sent[0].Body["hour"] != float64(22) ||
		sent[0].Body["timezone"] != "Europe/Berlin" {
		t.Fatalf("PATCH body = %+v", sent)
	}
	var got maintenanceWindowModel
	failOn(t, res.State.Get(context.Background(), &got))
	// The configured spelling of the same day is kept, so "Monday" does not
	// plan a change to "monday" on every refresh.
	if got.Day.ValueString() != "Monday" || got.ID.ValueString() != "i-1" || got.LengthHours.ValueInt64() != 4 {
		t.Errorf("state = %+v", got)
	}
	if len(got.Planned.Elements()) != 1 || got.NoticeHours.ValueInt64() != 48 {
		t.Errorf("planned maintenance not read: %v", got.Planned)
	}
}

func maintenanceStateVals() map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"id": dsStr("i-1"), "instance_id": dsStr("i-1"), "day": dsStr("monday"), "hour": num(22), "timezone": dsStr("Europe/Berlin"),
	}
}

func TestMaintenanceWindowRead(t *testing.T) {
	f := newFakeAPI(t)
	// Changed in the console to Saturday 04:00 UTC.
	f.answer("GET "+maintenancePath, 200, maintenanceJSON(6, 4, "UTC"))
	res := dsDoRead(t, &maintenanceWindowResource{client: f.client()}, maintenanceStateVals())
	failOn(t, res.Diagnostics)
	var got maintenanceWindowModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.Day.ValueString() != "saturday" || got.Hour.ValueInt64() != 4 || got.Timezone.ValueString() != "UTC" {
		t.Errorf("read did not refresh: %+v", got)
	}

	g := newFakeAPI(t)
	g.answer("GET "+maintenancePath, 404, `{"error":{"code":"not_found"}}`)
	res = dsDoRead(t, &maintenanceWindowResource{client: g.client()}, maintenanceStateVals())
	failOn(t, res.Diagnostics)
	if !res.State.Raw.IsNull() {
		t.Error("the window of a database that is gone must be removed from state")
	}
}

func TestMaintenanceWindowUpdate(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("PATCH "+maintenanceWindowPath, 200, maintenanceJSON(3, 1, "Asia/Kolkata"))
	plan := maintenanceStateVals()
	plan["day"], plan["hour"], plan["timezone"] = dsStr("wednesday"), num(1), dsStr("Asia/Kolkata")
	delete(plan, "id")
	res := dsDoUpdate(t, &maintenanceWindowResource{client: f.client()}, maintenanceStateVals(), plan)
	failOn(t, res.Diagnostics)
	if sent := f.sent("PATCH " + maintenanceWindowPath); sent[0].Body["day"] != float64(3) || sent[0].Body["timezone"] != "Asia/Kolkata" {
		t.Errorf("PATCH body = %+v", sent)
	}
}

// Delete puts the default window back: Sunday 03:00 UTC.
func TestMaintenanceWindowDeleteRestoresTheDefault(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("PATCH "+maintenanceWindowPath, 200, maintenanceJSON(0, 3, "UTC"))
	failOn(t, dsDoDelete(t, &maintenanceWindowResource{client: f.client()}, maintenanceStateVals()).Diagnostics)
	sent := f.sent("PATCH " + maintenanceWindowPath)
	if len(sent) != 1 || sent[0].Body["day"] != float64(0) || sent[0].Body["hour"] != float64(3) || sent[0].Body["timezone"] != "UTC" {
		t.Errorf("delete must restore Sunday 03:00 UTC, sent %+v", sent)
	}

	g := newFakeAPI(t)
	g.answer("PATCH "+maintenanceWindowPath, 404, `{"error":{"code":"not_found"}}`)
	failOn(t, dsDoDelete(t, &maintenanceWindowResource{client: g.client()}, maintenanceStateVals()).Diagnostics)
}

func TestMaintenanceWindowImport(t *testing.T) {
	res := dsDoImport(t, &maintenanceWindowResource{}, "i-1")
	failOn(t, res.Diagnostics)
	var got maintenanceWindowModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.ID.ValueString() != "i-1" || got.InstanceID.ValueString() != "i-1" {
		t.Errorf("import state = %+v", got)
	}
}

// The route validates with safeParse and answers zod's flattened issues with no
// message; the field and its complaint must still reach the practitioner.
func TestMaintenanceWindowRefusalNamesTheField(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("PATCH "+maintenanceWindowPath, 400, `{"error":{"code":"bad_request","issues":{"formErrors":[],
		"fieldErrors":{"timezone":["Not a known time zone (use an IANA name such as Europe/Berlin)."]}}}}`)
	res := dsDoCreate(t, &maintenanceWindowResource{client: f.client()}, map[string]tftypes.Value{
		"instance_id": dsStr("i-1"), "day": dsStr("monday"), "hour": num(3), "timezone": dsStr("Mars/Olympus"),
	})
	wantError(t, res.Diagnostics, "timezone: Not a known time zone (use an IANA name such as Europe/Berlin).")
}
