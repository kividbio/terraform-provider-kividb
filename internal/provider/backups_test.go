package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	testDestID   = "3e7a1c2b-4d5f-4a6b-8c9d-0e1f2a3b4c5d"
	bkInstanceID = "7d6c5b4a-3f2e-4d1c-9b0a-8f7e6d5c4b3a"
	testKdbID    = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
)

// fakeBackups is the backup half of the KiviDB API, in memory: the routes in
// export-destinations.ts and kdb-snapshots.ts, with their status codes and
// their refusals' shapes.
type fakeBackups struct {
	t  *testing.T
	mu sync.Mutex

	dests map[string]map[string]any

	// verifyFailFirst fails that many verifications before one passes; -1
	// fails every one.
	verifyFailFirst int
	verifyCalls     int

	instances map[string]map[string]any

	kdb map[string]map[string]any
	// kdbReadyAfter is how many reads a new export stays `creating` for; -1
	// is forever. kdbFails makes it end `failed` instead of `ready`.
	kdbReadyAfter int
	kdbFails      bool
	kdbReads      map[string]int

	// refuse answers a "METHOD /path" with this status and body instead.
	refuse map[string]bkRefusal

	calls  []string
	bodies map[string]map[string]any
}

type bkRefusal struct {
	status int
	body   string
}

func newFakeBackups(t *testing.T) (*fakeBackups, *Client) {
	t.Helper()
	f := &fakeBackups{
		t:         t,
		dests:     map[string]map[string]any{},
		instances: map[string]map[string]any{},
		kdb:       map[string]map[string]any{},
		kdbReads:  map[string]int{},
		refuse:    map[string]bkRefusal{},
		bodies:    map[string]map[string]any{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	// Waits in milliseconds rather than minutes.
	saved := []time.Duration{verifyRetryFor, verifyRetryEvery, kdbSettleFor, kdbSettleEvery, kdbDeleteWaitFor, kdbDeleteWaitEvery}
	verifyRetryFor, verifyRetryEvery = 200*time.Millisecond, time.Millisecond
	kdbSettleFor, kdbSettleEvery = 200*time.Millisecond, time.Millisecond
	kdbDeleteWaitFor, kdbDeleteWaitEvery = 200*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		verifyRetryFor, verifyRetryEvery, kdbSettleFor, kdbSettleEvery, kdbDeleteWaitFor, kdbDeleteWaitEvery =
			saved[0], saved[1], saved[2], saved[3], saved[4], saved[5]
	})
	return f, NewClient(srv.URL, "kvdb_test")
}

func (f *fakeBackups) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.bodies[key] = body
	w.Header().Set("Content-Type", "application/json")

	if rf, ok := f.refuse[key]; ok {
		w.WriteHeader(rf.status)
		_, _ = w.Write([]byte(rf.body))
		return
	}
	send := func(status int, v any) {
		w.WriteHeader(status)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	notFound := func() { send(404, map[string]any{"error": map[string]any{"code": "not_found"}}) }
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	switch {
	case key == "GET /export-destinations":
		list := []any{}
		for _, d := range f.dests {
			list = append(list, d)
		}
		send(200, map[string]any{"destinations": list})

	case key == "POST /export-destinations":
		role := fmt.Sprintf("KiviDBManagement-Exports-%s", testDestID[:8])
		if v, ok := body["role_name"].(string); ok {
			role = v
		}
		prefix := "kividb/"
		if v, ok := body["prefix"].(string); ok {
			prefix = v
		}
		d := map[string]any{
			"id": testDestID, "name": body["name"], "cloud": "aws", "aws_account_id": body["aws_account_id"],
			"bucket": body["bucket"], "bucket_region": nil, "prefix": prefix, "status": "pending",
			"verified_at": nil, "last_error": nil, "created_at": "2026-10-25T10:00:00.000Z",
			"setup": map[string]any{
				"role_name":   role,
				"external_id": "kvx-0123456789abcdef",
				"trust_policy": map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
					"Effect": "Allow", "Principal": map[string]any{"AWS": []any{"arn:aws:iam::111111111111:role/kividb-api"}},
					"Action": "sts:AssumeRole", "Condition": map[string]any{"StringEquals": map[string]any{"sts:ExternalId": "kvx-0123456789abcdef"}},
				}}},
				"permission_policy": map[string]any{"Version": "2012-10-17", "Statement": []any{}},
			},
		}
		f.dests[testDestID] = d
		send(201, map[string]any{"destination": d})

	case len(parts) == 3 && parts[0] == "export-destinations" && parts[2] == "verify" && r.Method == http.MethodPost:
		d, ok := f.dests[parts[1]]
		if !ok {
			notFound()
			return
		}
		f.verifyCalls++
		if f.verifyFailFirst < 0 || f.verifyCalls <= f.verifyFailFirst {
			msg := "KiviDB could not assume arn:aws:iam::123456789012:role/KiviDBManagement-Exports-3e7a1c2b. Check the role exists."
			d["status"], d["last_error"] = "failed", "assume_role: "+msg
			send(422, map[string]any{"destination": d, "error": map[string]any{"code": "verification_failed", "step": "assume_role", "message": msg}})
			return
		}
		d["status"], d["last_error"], d["verified_at"], d["bucket_region"] = "verified", nil, "2026-10-25T10:01:00.000Z", "eu-central-1"
		send(200, map[string]any{"destination": d})

	case len(parts) == 2 && parts[0] == "export-destinations" && r.Method == http.MethodDelete:
		if _, ok := f.dests[parts[1]]; !ok {
			notFound()
			return
		}
		delete(f.dests, parts[1])
		for _, inst := range f.instances {
			if inst["export_destination_id"] == parts[1] {
				inst["export_destination_id"], inst["kdb_export_cadence"] = nil, "off"
			}
		}
		send(204, nil)

	case len(parts) == 2 && parts[0] == "instances" && r.Method == http.MethodGet:
		inst, ok := f.instances[parts[1]]
		if !ok {
			notFound()
			return
		}
		send(200, map[string]any{"instance": inst, "infra": nil, "credentials": nil})

	case len(parts) == 3 && parts[0] == "instances" && parts[2] == "export-config" && r.Method == http.MethodPatch:
		inst, ok := f.instances[parts[1]]
		if !ok {
			notFound()
			return
		}
		dest := inst["export_destination_id"]
		if v, sent := body["export_destination_id"]; sent {
			dest = v
		}
		cadence := inst["kdb_export_cadence"]
		if v, sent := body["kdb_export_cadence"]; sent {
			cadence = v
		}
		if dest != nil {
			if d, ok := f.dests[dest.(string)]; !ok || d["status"] != "verified" {
				send(422, map[string]any{"error": map[string]any{"code": "destination_not_verified", "message": "Choose a verified destination of this organization."}})
				return
			}
		}
		if cadence != "off" && dest == nil {
			send(422, map[string]any{"error": map[string]any{"code": "destination_required", "message": "Scheduled exports are delivered to a destination; choose one first."}})
			return
		}
		inst["export_destination_id"], inst["kdb_export_cadence"] = dest, cadence
		send(200, map[string]any{"export_destination_id": dest, "kdb_export_cadence": cadence})

	case len(parts) == 4 && parts[0] == "instances" && parts[2] == "kdb-snapshots" && parts[3] == "export" && r.Method == http.MethodPost:
		if _, ok := f.instances[parts[1]]; !ok {
			notFound()
			return
		}
		label := "kdb-export-abc"
		if v, ok := body["label"].(string); ok {
			label = v
		}
		f.kdb[testKdbID] = map[string]any{
			"id": testKdbID, "instance_id": parts[1], "instance_name": "orders", "label": label,
			"size_bytes": nil, "status": "creating", "triggered_by": "manual", "cloud": "aws", "region": "eu-central-1",
			"ttd": nil, "created_at": "2026-10-25T10:00:00.000Z", "completed_at": nil, "error_message": nil,
			"cloud_account_id": nil, "cloud_account_name": nil, "destination_id": nil, "delivery_status": nil,
			"delivered_uri": nil, "delivery_error": nil,
		}
		send(202, map[string]any{"kdb_snapshot_id": testKdbID, "job_id": "j-1"})

	case key == "GET /kdb-snapshots" || (len(parts) == 3 && parts[0] == "instances" && parts[2] == "kdb-snapshots" && r.Method == http.MethodGet):
		list := []any{}
		for id, s := range f.kdb {
			if len(parts) == 3 && s["instance_id"] != parts[1] {
				continue
			}
			f.kdbReads[id]++
			if s["status"] == "creating" && f.kdbReadyAfter >= 0 && f.kdbReads[id] > f.kdbReadyAfter {
				if f.kdbFails {
					s["status"], s["error_message"] = "failed", "BGSAVE failed: disk full"
				} else {
					s["status"], s["size_bytes"], s["completed_at"] = "ready", "1048576", "2026-10-25T10:02:00.000Z"
				}
			}
			list = append(list, s)
		}
		send(200, map[string]any{"snapshots": list})

	case len(parts) == 2 && parts[0] == "kdb-snapshots" && r.Method == http.MethodDelete:
		s, ok := f.kdb[parts[1]]
		if !ok {
			notFound()
			return
		}
		if s["status"] == "creating" {
			f.kdbReads[parts[1]]++ // time passes for it too
			if f.kdbReadyAfter >= 0 && f.kdbReads[parts[1]] > f.kdbReadyAfter {
				s["status"] = "ready"
			}
			send(409, map[string]any{"error": map[string]any{"code": "snapshot_in_progress"}})
			return
		}
		delete(f.kdb, parts[1])
		send(204, nil)

	default:
		f.t.Errorf("unexpected %s", key)
		send(500, nil)
	}
}

func (f *fakeBackups) called(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

func (f *fakeBackups) body(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

func (f *fakeBackups) addInstance(freeTier bool) {
	f.instances[bkInstanceID] = map[string]any{
		"id": bkInstanceID, "name": "orders", "tier": "essentials", "cloud": "aws", "region": "eu-central-1",
		"status": "running", "free_tier": freeTier, "shard_count": 1,
		"export_destination_id": nil, "kdb_export_cadence": "off",
	}
}

func (f *fakeBackups) addVerifiedDestination() {
	f.dests[testDestID] = map[string]any{
		"id": testDestID, "name": "backups", "cloud": "aws", "aws_account_id": "123456789012",
		"bucket": "acme-kividb-exports", "bucket_region": "eu-central-1", "prefix": "kividb/", "status": "verified",
		"verified_at": "2026-10-25T10:01:00.000Z", "last_error": nil, "created_at": "2026-10-25T10:00:00.000Z",
		"setup": map[string]any{"role_name": "KiviDBManagement-Exports-3e7a1c2b", "external_id": "kvx-1",
			"trust_policy": map[string]any{}, "permission_policy": map[string]any{}},
	}
}

// --- driving a resource the way Terraform does ------------------------------

type harness struct {
	t   *testing.T
	r   resource.Resource
	sch rschema.Schema
	typ tftypes.Type
}

func newHarness(t *testing.T, r resource.Resource) *harness {
	t.Helper()
	var res resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &res)
	failOn(t, res.Diagnostics)
	return &harness{t: t, r: r, sch: res.Schema, typ: res.Schema.Type().TerraformType(context.Background())}
}

func (h *harness) plan(vals map[string]tftypes.Value) tfsdk.Plan {
	return tfsdk.Plan{Schema: h.sch, Raw: objectOf(h.t, h.typ, vals, true)}
}

func (h *harness) empty() tfsdk.State {
	return tfsdk.State{Schema: h.sch, Raw: tftypes.NewValue(h.typ, nil)}
}

func (h *harness) create(vals map[string]tftypes.Value) (tfsdk.State, diag.Diagnostics) {
	res := resource.CreateResponse{State: h.empty()}
	h.r.Create(context.Background(), resource.CreateRequest{Plan: h.plan(vals)}, &res)
	return res.State, res.Diagnostics
}

func (h *harness) read(state tfsdk.State) (tfsdk.State, diag.Diagnostics) {
	res := resource.ReadResponse{State: state}
	h.r.Read(context.Background(), resource.ReadRequest{State: state}, &res)
	return res.State, res.Diagnostics
}

func (h *harness) update(state tfsdk.State, vals map[string]tftypes.Value) (tfsdk.State, diag.Diagnostics) {
	res := resource.UpdateResponse{State: state}
	h.r.Update(context.Background(), resource.UpdateRequest{Plan: h.plan(vals), State: state}, &res)
	return res.State, res.Diagnostics
}

func (h *harness) destroy(state tfsdk.State) diag.Diagnostics {
	res := resource.DeleteResponse{State: state}
	h.r.Delete(context.Background(), resource.DeleteRequest{State: state}, &res)
	return res.Diagnostics
}

// importAndRead is `terraform import`: the id into an empty state, then a read.
func (h *harness) importAndRead(id string) (tfsdk.State, diag.Diagnostics) {
	res := resource.ImportStateResponse{State: tfsdk.State{Schema: h.sch, Raw: objectOf(h.t, h.typ, nil, false)}}
	h.r.(resource.ResourceWithImportState).ImportState(context.Background(), resource.ImportStateRequest{ID: id}, &res)
	if res.Diagnostics.HasError() {
		return res.State, res.Diagnostics
	}
	return h.read(res.State)
}

func (h *harness) bkStr(state tfsdk.State, name string) string {
	h.t.Helper()
	v := plannedStringOf(h.t, state.Raw, h.typ, name)
	return v
}

func plannedStringOf(t *testing.T, raw tftypes.Value, typ tftypes.Type, name string) string {
	t.Helper()
	var all map[string]tftypes.Value
	if err := raw.As(&all); err != nil {
		t.Fatalf("state is not an object: %v", err)
	}
	v, ok := all[name]
	if !ok {
		t.Fatalf("no attribute %q", name)
	}
	if v.IsNull() {
		return "<null>"
	}
	var s string
	if err := v.As(&s); err != nil {
		t.Fatalf("%s is not a string: %v", name, err)
	}
	return s
}

func configure(t *testing.T, r resource.Resource, c *Client) resource.Resource {
	t.Helper()
	res := resource.ConfigureResponse{}
	r.(resource.ResourceWithConfigure).Configure(context.Background(), resource.ConfigureRequest{ProviderData: c}, &res)
	failOn(t, res.Diagnostics)
	return r
}

func bkStr(v string) tftypes.Value { return tftypes.NewValue(tftypes.String, v) }
func null() tftypes.Value          { return tftypes.NewValue(tftypes.String, nil) }
func bkBoolean(v bool) tftypes.Value {
	return tftypes.NewValue(tftypes.Bool, v)
}

func errorText(d diag.Diagnostics) string {
	var b strings.Builder
	for _, e := range d.Errors() {
		b.WriteString(e.Summary() + ": " + e.Detail() + "\n")
	}
	return b.String()
}

// --- kividb_export_destination ----------------------------------------------

func destinationPlan() map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"name":           bkStr("backups"),
		"aws_account_id": bkStr("123456789012"),
		"bucket":         bkStr("acme-kividb-exports"),
		"prefix":         bkStr("kividb/"),
		"role_name":      tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	}
}

func TestExportDestinationCreateReadsTheSetupBack(t *testing.T) {
	f, c := newFakeBackups(t)
	h := newHarness(t, configure(t, NewExportDestinationResource(), c))

	state, d := h.create(destinationPlan())
	failOn(t, d)

	sent := f.body("POST /export-destinations")
	if sent["name"] != "backups" || sent["aws_account_id"] != "123456789012" || sent["bucket"] != "acme-kividb-exports" || sent["prefix"] != "kividb/" {
		t.Errorf("create did not send what the API parses: %v", sent)
	}
	if _, ok := sent["role_name"]; ok {
		t.Errorf("an unset role_name must be left to the API's default: %v", sent)
	}
	if f.called("POST /export-destinations/"+testDestID+"/verify") != 0 {
		t.Errorf("creating a destination must not verify it: the role cannot exist yet")
	}
	for name, want := range map[string]string{
		"id":          testDestID,
		"role_name":   "KiviDBManagement-Exports-3e7a1c2b",
		"role_arn":    "arn:aws:iam::123456789012:role/KiviDBManagement-Exports-3e7a1c2b",
		"external_id": "kvx-0123456789abcdef",
		"status":      "pending",
		"cloud":       "aws",
		"last_error":  "<null>",
	} {
		if got := h.bkStr(state, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	trust := h.bkStr(state, "trust_policy")
	if strings.ContainsAny(trust, "\n ") || !strings.Contains(trust, `"sts:ExternalId":"kvx-0123456789abcdef"`) {
		t.Errorf("trust_policy must be the API's document, compact: %s", trust)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(trust), &doc); err != nil {
		t.Errorf("trust_policy must be JSON: %v", err)
	}
}

func TestExportDestinationSendsAChosenRoleName(t *testing.T) {
	f, c := newFakeBackups(t)
	h := newHarness(t, configure(t, NewExportDestinationResource(), c))
	vals := destinationPlan()
	vals["role_name"] = bkStr("KiviDBManagement-Exports-prod")
	state, d := h.create(vals)
	failOn(t, d)
	if f.body("POST /export-destinations")["role_name"] != "KiviDBManagement-Exports-prod" {
		t.Errorf("role_name not sent")
	}
	if got := h.bkStr(state, "role_arn"); got != "arn:aws:iam::123456789012:role/KiviDBManagement-Exports-prod" {
		t.Errorf("role_arn = %q", got)
	}
}

func TestExportDestinationReadRefreshesAndForgetsWhenGone(t *testing.T) {
	f, c := newFakeBackups(t)
	h := newHarness(t, configure(t, NewExportDestinationResource(), c))
	state, d := h.create(destinationPlan())
	failOn(t, d)

	f.dests[testDestID]["status"] = "failed"
	f.dests[testDestID]["last_error"] = "write: The role may not write under acme-kividb-exports/kividb/"
	state, d = h.read(state)
	failOn(t, d)
	if h.bkStr(state, "status") != "failed" || !strings.HasPrefix(h.bkStr(state, "last_error"), "write:") {
		t.Errorf("read did not refresh the verification state: %s / %s", h.bkStr(state, "status"), h.bkStr(state, "last_error"))
	}

	delete(f.dests, testDestID)
	state, d = h.read(state)
	failOn(t, d)
	if !state.Raw.IsNull() {
		t.Errorf("a destination deleted outside Terraform must leave state")
	}
}

func TestExportDestinationUpdateDeleteAndImport(t *testing.T) {
	f, c := newFakeBackups(t)
	h := newHarness(t, configure(t, NewExportDestinationResource(), c))
	state, d := h.create(destinationPlan())
	failOn(t, d)

	state, d = h.update(state, destinationPlan())
	failOn(t, d)
	if h.bkStr(state, "id") != testDestID {
		t.Errorf("update lost the id")
	}

	imported, d := h.importAndRead(testDestID)
	failOn(t, d)
	if h.bkStr(imported, "bucket") != "acme-kividb-exports" || h.bkStr(imported, "external_id") != "kvx-0123456789abcdef" {
		t.Errorf("import did not read the destination: bucket=%s", h.bkStr(imported, "bucket"))
	}

	failOn(t, h.destroy(state))
	if f.called("DELETE /export-destinations/"+testDestID) != 1 || len(f.dests) != 0 {
		t.Errorf("delete did not remove the destination")
	}
	// Already gone is done, not an error.
	failOn(t, h.destroy(state))
}

func TestExportDestinationRefusalIsVerbatim(t *testing.T) {
	f, c := newFakeBackups(t)
	h := newHarness(t, configure(t, NewExportDestinationResource(), c))
	f.refuse["POST /export-destinations"] = bkRefusal{409,
		`{"error":{"code":"already_exists","message":"This organization already has a destination for that bucket and prefix."}}`}
	state, d := h.create(destinationPlan())
	if !d.HasError() || !strings.Contains(errorText(d), "This organization already has a destination for that bucket and prefix.") {
		t.Errorf("the API's bkRefusal must be shown as written: %s", errorText(d))
	}
	if !state.Raw.IsNull() {
		t.Errorf("a refused create must record nothing")
	}
}

func TestExportDestinationValidatorsMirrorTheApi(t *testing.T) {
	h := newHarness(t, NewExportDestinationResource())
	attr := func(name string) rschema.StringAttribute { return h.sch.Attributes[name].(rschema.StringAttribute) }
	for _, tc := range []struct {
		attr, value string
		ok          bool
	}{
		{"aws_account_id", "123456789012", true},
		{"aws_account_id", "12345678901", false},
		{"bucket", "acme-kividb-exports", true},
		{"bucket", "Acme_Bucket", false},
		{"prefix", "", true},
		{"prefix", "kividb/", true},
		{"prefix", "a/b/c/", true},
		{"prefix", "kividb", false},
		{"role_name", "KiviDBManagement-Exports-prod", true},
		{"role_name", "MyExportRole", false},
		{"role_name", "KiviDBManagement-Exports-" + strings.Repeat("x", 40), false},
		{"name", "backups", true},
		{"name", " backups", false},
		{"name", strings.Repeat("n", 81), false},
	} {
		d := runStringValidators(attr(tc.attr).Validators, tc.value)
		if d.HasError() == tc.ok {
			t.Errorf("%s = %q: ok=%v, got errors %v", tc.attr, tc.value, tc.ok, d.Errors())
		}
	}
}

// --- kividb_export_destination_verification ---------------------------------

func TestVerificationRetriesWhileTheRolePropagates(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addVerifiedDestination()
	f.dests[testDestID]["status"] = "pending"
	f.verifyFailFirst = 2
	h := newHarness(t, configure(t, NewExportDestinationVerificationResource(), c))

	state, d := h.create(map[string]tftypes.Value{"destination_id": bkStr(testDestID), "triggers": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)})
	failOn(t, d)
	if f.verifyCalls != 3 {
		t.Errorf("verification should be retried until it passes: %d calls", f.verifyCalls)
	}
	if h.bkStr(state, "status") != "verified" || h.bkStr(state, "bucket_region") != "eu-central-1" || h.bkStr(state, "id") != testDestID {
		t.Errorf("verified state not recorded: %s %s", h.bkStr(state, "status"), h.bkStr(state, "bucket_region"))
	}
}

func TestVerificationFailureFailsTheApplyAndLeavesNoOrphan(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addVerifiedDestination()
	f.dests[testDestID]["status"] = "pending"
	f.verifyFailFirst = -1
	h := newHarness(t, configure(t, NewExportDestinationVerificationResource(), c))

	state, d := h.create(map[string]tftypes.Value{"destination_id": bkStr(testDestID), "triggers": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)})
	text := errorText(d)
	if !d.HasError() || !strings.Contains(text, "KiviDB could not assume arn:aws:iam::123456789012:role/KiviDBManagement-Exports-3e7a1c2b. Check the role exists.") ||
		!strings.Contains(text, "assume_role") {
		t.Errorf("the API's reason and step must be in the error: %s", text)
	}
	if !state.Raw.IsNull() {
		t.Errorf("a verification that did not pass must not be recorded as one")
	}
	if f.verifyCalls < 2 {
		t.Errorf("a failed verification is retried within the window: %d calls", f.verifyCalls)
	}
	// The destination is untouched by the failure, still there and readable,
	// now saying why.
	if f.dests[testDestID]["status"] != "failed" {
		t.Errorf("the destination should read failed: %v", f.dests[testDestID]["status"])
	}
}

func TestVerificationDoesNotRetryOtherRefusals(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addVerifiedDestination()
	f.refuse["POST /export-destinations/"+testDestID+"/verify"] = bkRefusal{403,
		`{"error":{"code":"forbidden","message":"This API key cannot change databases."}}`}
	h := newHarness(t, configure(t, NewExportDestinationVerificationResource(), c))
	_, d := h.create(map[string]tftypes.Value{"destination_id": bkStr(testDestID), "triggers": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)})
	if !strings.Contains(errorText(d), "This API key cannot change databases.") {
		t.Errorf("bkRefusal not surfaced: %s", errorText(d))
	}
	if n := f.called("POST /export-destinations/" + testDestID + "/verify"); n != 1 {
		t.Errorf("a bkRefusal that waiting cannot fix must not be retried: %d calls", n)
	}
}

func TestVerificationReadForgetsAnUnverifiedOrMissingDestination(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addVerifiedDestination()
	h := newHarness(t, configure(t, NewExportDestinationVerificationResource(), c))

	state, d := h.importAndRead(testDestID)
	failOn(t, d)
	if h.bkStr(state, "destination_id") != testDestID || h.bkStr(state, "status") != "verified" {
		t.Errorf("import of a verified destination: %s %s", h.bkStr(state, "destination_id"), h.bkStr(state, "status"))
	}
	state, d = h.update(state, map[string]tftypes.Value{"destination_id": bkStr(testDestID), "triggers": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)})
	failOn(t, d)

	f.dests[testDestID]["status"] = "failed"
	gone, d := h.read(state)
	failOn(t, d)
	if !gone.Raw.IsNull() {
		t.Errorf("a destination no longer verified must drop the verification, so the next apply verifies again")
	}

	delete(f.dests, testDestID)
	gone, d = h.read(state)
	failOn(t, d)
	if !gone.Raw.IsNull() {
		t.Errorf("a deleted destination must drop the verification")
	}

	failOn(t, h.destroy(state))
	if n := len(f.calls); f.called("DELETE /export-destinations/"+testDestID) != 0 {
		t.Errorf("destroying a verification must not delete the destination (%d calls)", n)
	}
}

// --- kividb_instance_export -------------------------------------------------

func TestInstanceExportSetsUpdatesAndTurnsOff(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(false)
	f.addVerifiedDestination()
	h := newHarness(t, configure(t, NewInstanceExportResource(), c))

	state, d := h.create(map[string]tftypes.Value{
		"instance_id": bkStr(bkInstanceID), "export_destination_id": bkStr(strings.ToUpper(testDestID)), "cadence": bkStr("daily"),
	})
	failOn(t, d)
	if got := f.body("PATCH /instances/" + bkInstanceID + "/export-config"); got["kdb_export_cadence"] != "daily" ||
		got["export_destination_id"] != testDestID {
		t.Errorf("create sent %v", got)
	}
	if h.bkStr(state, "export_destination_id") != strings.ToUpper(testDestID) {
		t.Errorf("the configured spelling must be kept: %s", h.bkStr(state, "export_destination_id"))
	}
	if h.bkStr(state, "id") != bkInstanceID {
		t.Errorf("id = %s", h.bkStr(state, "id"))
	}
	if len(d.Warnings()) != 0 {
		t.Errorf("no warning for a paid database: %v", d.Warnings())
	}

	// Dropping the destination from configuration must send an explicit null:
	// an omitted field is kept by the API.
	f.instances[bkInstanceID]["kdb_export_cadence"] = "off"
	state, d = h.update(state, map[string]tftypes.Value{
		"instance_id": bkStr(bkInstanceID), "export_destination_id": null(), "cadence": bkStr("off"),
	})
	failOn(t, d)
	sent := f.body("PATCH /instances/" + bkInstanceID + "/export-config")
	if v, ok := sent["export_destination_id"]; !ok || v != nil {
		t.Errorf("an unset destination must be sent as null: %v", sent)
	}
	if f.instances[bkInstanceID]["export_destination_id"] != nil {
		t.Errorf("destination not cleared")
	}

	f.instances[bkInstanceID]["export_destination_id"] = testDestID
	f.instances[bkInstanceID]["kdb_export_cadence"] = "weekly"
	state, d = h.read(state)
	failOn(t, d)
	if h.bkStr(state, "cadence") != "weekly" || h.bkStr(state, "export_destination_id") != testDestID {
		t.Errorf("read must show a change made outside Terraform: %s %s", h.bkStr(state, "cadence"), h.bkStr(state, "export_destination_id"))
	}

	failOn(t, h.destroy(state))
	if f.instances[bkInstanceID]["export_destination_id"] != nil || f.instances[bkInstanceID]["kdb_export_cadence"] != "off" {
		t.Errorf("destroy must turn exports off: %v", f.instances[bkInstanceID])
	}
}

func TestInstanceExportImportAndGone(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(false)
	f.addVerifiedDestination()
	f.instances[bkInstanceID]["export_destination_id"] = testDestID
	f.instances[bkInstanceID]["kdb_export_cadence"] = "daily"
	h := newHarness(t, configure(t, NewInstanceExportResource(), c))

	state, d := h.importAndRead(bkInstanceID)
	failOn(t, d)
	if h.bkStr(state, "instance_id") != bkInstanceID || h.bkStr(state, "cadence") != "daily" || h.bkStr(state, "export_destination_id") != testDestID {
		t.Errorf("import: %s %s %s", h.bkStr(state, "instance_id"), h.bkStr(state, "cadence"), h.bkStr(state, "export_destination_id"))
	}

	delete(f.instances, bkInstanceID)
	gone, d := h.read(state)
	failOn(t, d)
	if !gone.Raw.IsNull() {
		t.Errorf("a deleted database's export configuration must leave state")
	}
	// And destroying it then has nothing to turn off.
	failOn(t, h.destroy(state))
}

func TestInstanceExportRefusalIsVerbatim(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(false)
	f.addVerifiedDestination()
	f.dests[testDestID]["status"] = "pending"
	h := newHarness(t, configure(t, NewInstanceExportResource(), c))
	state, d := h.create(map[string]tftypes.Value{
		"instance_id": bkStr(bkInstanceID), "export_destination_id": bkStr(testDestID), "cadence": bkStr("off"),
	})
	if !strings.Contains(errorText(d), "Choose a verified destination of this organization.") {
		t.Errorf("bkRefusal not shown as written: %s", errorText(d))
	}
	if !state.Raw.IsNull() {
		t.Errorf("a refused create must record nothing")
	}
}

func TestInstanceExportWarnsThatFreeTierHasNoSchedule(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(true)
	f.addVerifiedDestination()
	h := newHarness(t, configure(t, NewInstanceExportResource(), c))
	_, d := h.create(map[string]tftypes.Value{
		"instance_id": bkStr(bkInstanceID), "export_destination_id": bkStr(testDestID), "cadence": bkStr("daily"),
	})
	failOn(t, d)
	if len(d.Warnings()) != 1 || !strings.Contains(d.Warnings()[0].Summary(), "free-tier") {
		t.Errorf("a schedule on a free-tier database must warn that it will not run: %v", d.Warnings())
	}
}

func TestInstanceExportScheduleNeedsADestinationAtPlan(t *testing.T) {
	h := newHarness(t, NewInstanceExportResource())
	for _, tc := range []struct {
		dest, cadence tftypes.Value
		wantErr       bool
	}{
		{null(), bkStr("daily"), true},
		{null(), bkStr("off"), false},
		{bkStr(testDestID), bkStr("weekly"), false},
		{tftypes.NewValue(tftypes.String, tftypes.UnknownValue), bkStr("daily"), false},
	} {
		cfg := tfsdk.Config{Schema: h.sch, Raw: objectOf(t, h.typ, map[string]tftypes.Value{
			"instance_id": bkStr(bkInstanceID), "export_destination_id": tc.dest, "cadence": tc.cadence,
		}, false)}
		res := resource.ValidateConfigResponse{}
		h.r.(resource.ResourceWithValidateConfig).ValidateConfig(context.Background(), resource.ValidateConfigRequest{Config: cfg}, &res)
		if res.Diagnostics.HasError() != tc.wantErr {
			t.Errorf("dest=%v cadence=%v: error=%v, want %v", tc.dest, tc.cadence, res.Diagnostics.HasError(), tc.wantErr)
		}
	}
}

// --- kividb_kdb_snapshot -----------------------------------------------------

func kdbPlan(waitForReady tftypes.Value) map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"instance_id":    bkStr(bkInstanceID),
		"label":          bkStr("before-migration"),
		"deliver":        bkBoolean(true),
		"wait_for_ready": waitForReady,
	}
}

func TestKdbSnapshotCreateWaitsUntilWritten(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(false)
	f.kdbReadyAfter = 2
	h := newHarness(t, configure(t, NewKdbSnapshotResource(), c))

	state, d := h.create(kdbPlan(tftypes.NewValue(tftypes.Bool, nil)))
	failOn(t, d)
	sent := f.body("POST /instances/" + bkInstanceID + "/kdb-snapshots/export")
	if sent["label"] != "before-migration" || sent["deliver"] != true {
		t.Errorf("export sent %v", sent)
	}
	if h.bkStr(state, "status") != "ready" || h.bkStr(state, "id") != testKdbID {
		t.Errorf("create must wait for the file: status=%s", h.bkStr(state, "status"))
	}
	var m kdbSnapshotModel
	failOn(t, state.Get(context.Background(), &m))
	if m.SizeBytes.ValueInt64() != 1048576 {
		t.Errorf("size_bytes arrives as a string from a BIGINT and must still read: %v", m.SizeBytes)
	}
	if !m.DeliveryStatus.IsNull() || !m.ExpiresAt.IsNull() {
		t.Errorf("no destination, no delivery; manual, no expiry: %v %v", m.DeliveryStatus, m.ExpiresAt)
	}
}

func TestKdbSnapshotWithoutWaiting(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(false)
	f.kdbReadyAfter = -1
	h := newHarness(t, configure(t, NewKdbSnapshotResource(), c))
	state, d := h.create(kdbPlan(bkBoolean(false)))
	failOn(t, d)
	if h.bkStr(state, "status") != "creating" {
		t.Errorf("wait_for_ready = false returns at once: %s", h.bkStr(state, "status"))
	}
}

func TestKdbSnapshotThatFailsIsRemovedAndReported(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(false)
	f.kdbReadyAfter, f.kdbFails = 1, true
	h := newHarness(t, configure(t, NewKdbSnapshotResource(), c))

	state, d := h.create(kdbPlan(tftypes.NewValue(tftypes.Bool, nil)))
	if !strings.Contains(errorText(d), "BGSAVE failed: disk full") {
		t.Errorf("the export's own error must be shown: %s", errorText(d))
	}
	if !state.Raw.IsNull() {
		t.Errorf("a failed export is deleted, so nothing is recorded")
	}
	if len(f.kdb) != 0 {
		t.Errorf("the failed export must not be left behind: %v", f.kdb)
	}
}

func TestKdbSnapshotStillWritingAtTheDeadlineIsKeptInState(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(false)
	f.kdbReadyAfter = -1
	h := newHarness(t, configure(t, NewKdbSnapshotResource(), c))

	state, d := h.create(kdbPlan(tftypes.NewValue(tftypes.Bool, nil)))
	if !d.HasError() || !strings.Contains(errorText(d), "its id is in state") {
		t.Errorf("a timeout must say the export is kept: %s", errorText(d))
	}
	if state.Raw.IsNull() || h.bkStr(state, "id") != testKdbID {
		t.Errorf("an export still being written exists and must be in state (tainted)")
	}
}

func TestKdbSnapshotReadImportUpdateAndDelete(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(false)
	f.kdbReadyAfter = 0
	h := newHarness(t, configure(t, NewKdbSnapshotResource(), c))
	state, d := h.create(kdbPlan(tftypes.NewValue(tftypes.Bool, nil)))
	failOn(t, d)

	f.kdb[testKdbID]["delivery_status"] = "delivered"
	f.kdb[testKdbID]["delivered_uri"] = "s3://acme-kividb-exports/kividb/orders/kdb-export.kdb"
	state, d = h.read(state)
	failOn(t, d)
	if h.bkStr(state, "delivery_status") != "delivered" || !strings.HasPrefix(h.bkStr(state, "delivered_uri"), "s3://") {
		t.Errorf("delivery not read: %s", h.bkStr(state, "delivery_status"))
	}

	state, d = h.update(state, kdbPlan(bkBoolean(false)))
	failOn(t, d)
	if h.bkStr(state, "id") != testKdbID {
		t.Errorf("update lost the id")
	}

	imported, d := h.importAndRead(testKdbID)
	failOn(t, d)
	if h.bkStr(imported, "instance_id") != bkInstanceID || h.bkStr(imported, "label") != "before-migration" {
		t.Errorf("import: %s %s", h.bkStr(imported, "instance_id"), h.bkStr(imported, "label"))
	}

	// The database is deleted: the export stays, with no instance_id. It is
	// still this resource, not "gone".
	f.kdb[testKdbID]["instance_id"] = nil
	state, d = h.read(state)
	failOn(t, d)
	if state.Raw.IsNull() || h.bkStr(state, "instance_id") != bkInstanceID {
		t.Errorf("an export outlives its database and keeps instance_id")
	}

	failOn(t, h.destroy(state))
	if len(f.kdb) != 0 {
		t.Errorf("destroy must delete the export")
	}
	gone, d := h.read(state)
	failOn(t, d)
	if !gone.Raw.IsNull() {
		t.Errorf("a deleted export must leave state")
	}
	failOn(t, h.destroy(state))
}

func TestKdbSnapshotDeleteWaitsOutAnExportInProgress(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(false)
	f.kdbReadyAfter = -1
	h := newHarness(t, configure(t, NewKdbSnapshotResource(), c))
	state, _ := h.create(kdbPlan(bkBoolean(false)))

	f.kdbReadyAfter = f.kdbReads[testKdbID] + 2
	failOn(t, h.destroy(state))
	if n := f.called("DELETE /kdb-snapshots/" + testKdbID); n < 2 || len(f.kdb) != 0 {
		t.Errorf("delete should retry past snapshot_in_progress, then delete: %d calls", n)
	}

	// One that never finishes gives up with the reason.
	f.kdbReadyAfter = -1
	f.kdb[testKdbID] = map[string]any{"id": testKdbID, "status": "creating"}
	d := h.destroy(state)
	if !strings.Contains(errorText(d), "still being written") {
		t.Errorf("a bounded wait must end with the reason: %s", errorText(d))
	}
}

func TestKdbSnapshotRefusalsAreExplainedOrVerbatim(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(false)
	h := newHarness(t, configure(t, NewKdbSnapshotResource(), c))
	key := "POST /instances/" + bkInstanceID + "/kdb-snapshots/export"

	f.refuse[key] = bkRefusal{409, `{"error":{"code":"instance_not_running"}}`}
	_, d := h.create(kdbPlan(tftypes.NewValue(tftypes.Bool, nil)))
	if !strings.Contains(errorText(d), "The database is not running") {
		t.Errorf("a bare code must be explained: %s", errorText(d))
	}

	f.refuse[key] = bkRefusal{422, `{"error":{"code":"sharded_export_unsupported_on_cloud","message":"Exporting a sharded database is available on AWS only for now."}}`}
	_, d = h.create(kdbPlan(tftypes.NewValue(tftypes.Bool, nil)))
	if !strings.Contains(errorText(d), "Exporting a sharded database is available on AWS only for now.") {
		t.Errorf("a message must be shown as written: %s", errorText(d))
	}
}

func TestKdbSnapshotLabelValidation(t *testing.T) {
	h := newHarness(t, NewKdbSnapshotResource())
	vs := h.sch.Attributes["label"].(rschema.StringAttribute).Validators
	if runStringValidators(vs, "nightly").HasError() {
		t.Errorf("a plain label is fine")
	}
	for _, bad := range []string{"", " padded ", strings.Repeat("l", 129)} {
		if !runStringValidators(vs, bad).HasError() {
			t.Errorf("label %q should be refused", bad)
		}
	}
}

// --- data sources ------------------------------------------------------------

func bkReadDataSource(t *testing.T, ds datasource.DataSource, c *Client, vals map[string]tftypes.Value) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	cres := datasource.ConfigureResponse{}
	ds.(datasource.DataSourceWithConfigure).Configure(context.Background(), datasource.ConfigureRequest{ProviderData: c}, &cres)
	failOn(t, cres.Diagnostics)
	sch := dataSourceSchema(t, ds)
	typ := sch.Type().TerraformType(context.Background())
	res := datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(typ, nil)}}
	ds.Read(context.Background(), datasource.ReadRequest{Config: tfsdk.Config{Schema: sch, Raw: objectOf(t, typ, vals, false)}}, &res)
	return res.State, res.Diagnostics
}

func TestKdbSnapshotsListsAndFilters(t *testing.T) {
	f, c := newFakeBackups(t)
	f.addInstance(false)
	f.kdbReadyAfter = -1
	f.kdb[testKdbID] = map[string]any{"id": testKdbID, "instance_id": bkInstanceID, "label": "a", "status": "ready",
		"size_bytes": "2048", "triggered_by": "schedule", "created_at": "2026-10-25T10:00:00.000Z", "ttd": "2026-11-01T10:00:00.000Z",
		"delivery_status": "delivered", "delivered_uri": "s3://b/kividb/a.kdb"}
	f.kdb["other"] = map[string]any{"id": "other", "instance_id": nil, "label": "b", "status": "failed",
		"triggered_by": "manual", "created_at": "2026-10-24T10:00:00.000Z"}

	state, d := bkReadDataSource(t, NewKdbSnapshotsDataSource(), c, map[string]tftypes.Value{"status": bkStr("ready")})
	failOn(t, d)
	var m kdbSnapshotsModel
	failOn(t, state.Get(context.Background(), &m))
	if f.called("GET /kdb-snapshots") != 1 || len(m.Snapshots) != 1 || m.Snapshots[0].ID.ValueString() != testKdbID {
		t.Fatalf("org-wide list filtered to ready: %+v", m.Snapshots)
	}
	s := m.Snapshots[0]
	if s.SizeBytes.ValueInt64() != 2048 || s.ExpiresAt.ValueString() != "2026-11-01T10:00:00.000Z" || s.DeliveryStatus.ValueString() != "delivered" {
		t.Errorf("fields not read: %+v", s)
	}

	_, d = bkReadDataSource(t, NewKdbSnapshotsDataSource(), c, map[string]tftypes.Value{"instance_id": bkStr(bkInstanceID)})
	failOn(t, d)
	if f.called("GET /instances/"+bkInstanceID+"/kdb-snapshots") != 1 {
		t.Errorf("instance_id must use the database's list")
	}

	f.refuse["GET /kdb-snapshots"] = bkRefusal{403, `{"error":{"code":"forbidden","message":"This key may not read snapshots."}}`}
	_, d = bkReadDataSource(t, NewKdbSnapshotsDataSource(), c, nil)
	if !strings.Contains(errorText(d), "This key may not read snapshots.") {
		t.Errorf("bkRefusal not surfaced: %s", errorText(d))
	}
}

func TestDiskSnapshotsReadsBothListings(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/disk-snapshots" {
			// The organization's list calls the label snapshot_label.
			_, _ = w.Write([]byte(`{"snapshots":[
				{"id":"d-1","instance_id":"i-1","instance_name":"orders","resolved_instance_name":"orders-renamed",
				 "snapshot_label":"nightly","cloud":"aws","region":"eu-central-1","size_gb":"8.00","status":"ready",
				 "triggered_by":"schedule","ttd":"2026-11-01T00:00:00.000Z","created_at":"2026-10-25T00:00:00.000Z",
				 "completed_at":"2026-10-25T00:05:00.000Z","error_message":null,"cloud_account_id":null,"cloud_account_name":null},
				{"id":"d-2","instance_id":null,"instance_name":"gone","snapshot_label":"x","cloud":"aws","region":"eu-central-1",
				 "size_gb":null,"status":"failed","triggered_by":"manual","created_at":"2026-10-24T00:00:00.000Z"}]}`))
			return
		}
		// The database's list calls it label.
		_, _ = w.Write([]byte(`{"snapshots":[{"id":"d-3","instance_id":"i-1","instance_name":"orders","label":"manual-1",
			"cloud":"aws","region":"eu-central-1","size_gb":4,"status":"ready","triggered_by":"manual","created_at":"2026-10-25T00:00:00.000Z"}]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "k")

	state, d := bkReadDataSource(t, NewDiskSnapshotsDataSource(), c, map[string]tftypes.Value{"status": bkStr("ready")})
	failOn(t, d)
	var m diskSnapshotsModel
	failOn(t, state.Get(context.Background(), &m))
	if len(m.Snapshots) != 1 {
		t.Fatalf("status filter: %+v", m.Snapshots)
	}
	s := m.Snapshots[0]
	if s.Label.ValueString() != "nightly" || s.SizeGB.ValueFloat64() != 8 || s.InstanceName.ValueString() != "orders-renamed" ||
		s.ExpiresAt.ValueString() != "2026-11-01T00:00:00.000Z" {
		t.Errorf("org listing not read: %+v", s)
	}

	state, d = bkReadDataSource(t, NewDiskSnapshotsDataSource(), c, map[string]tftypes.Value{"instance_id": bkStr("i-1")})
	failOn(t, d)
	failOn(t, state.Get(context.Background(), &m))
	if len(m.Snapshots) != 1 || m.Snapshots[0].Label.ValueString() != "manual-1" || m.Snapshots[0].SizeGB.ValueFloat64() != 4 {
		t.Errorf("instance listing not read: %+v", m.Snapshots)
	}
	if paths[len(paths)-1] != "/instances/i-1/disk-snapshots" {
		t.Errorf("instance_id must use the database's list: %v", paths)
	}
}

func TestBackupResourcesAreRegistered(t *testing.T) {
	p := New("test")()
	have := map[string]bool{}
	for _, r := range p.Resources(context.Background()) {
		var res resource.MetadataResponse
		r().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "kividb"}, &res)
		have[res.TypeName] = true
	}
	for _, d := range p.DataSources(context.Background()) {
		var res datasource.MetadataResponse
		d().Metadata(context.Background(), datasource.MetadataRequest{ProviderTypeName: "kividb"}, &res)
		have[res.TypeName] = true
	}
	for _, name := range []string{"kividb_export_destination", "kividb_export_destination_verification",
		"kividb_instance_export", "kividb_kdb_snapshot", "kividb_kdb_snapshots", "kividb_disk_snapshots"} {
		if !have[name] {
			t.Errorf("%s is not registered", name)
		}
	}
}
