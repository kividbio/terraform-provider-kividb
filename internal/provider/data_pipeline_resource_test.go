package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// fakePipelineAPI is the data integration routes as the API implements them
// (apps/api/src/routes/data-integration.ts): PUT is an upsert that creates a
// pipeline running, start/stop flip desired_state, DELETE drops it. Refusals
// can be queued per "METHOD path".
type fakePipelineAPI struct {
	t         *testing.T
	mu        sync.Mutex
	pipelines map[string]*DataPipeline
	calls     []string
	lastPut   map[string]any
	refuse    map[string]dpRefusal
	deleteOut string
}

type dpRefusal struct {
	status int
	body   string
}

func newFakePipelineAPI(t *testing.T) (*fakePipelineAPI, *httptest.Server) {
	f := &fakePipelineAPI{t: t, pipelines: map[string]*DataPipeline{}, refuse: map[string]dpRefusal{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakePipelineAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		call += "?" + r.URL.RawQuery
	}
	f.calls = append(f.calls, call)
	w.Header().Set("Content-Type", "application/json")

	if ref, ok := f.refuse[r.Method+" "+r.URL.Path]; ok {
		w.WriteHeader(ref.status)
		_, _ = w.Write([]byte(ref.body))
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/data-integration/")
	name, action, _ := strings.Cut(rest, "/")
	p := f.pipelines[name]
	notFound := func() {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"pipeline_not_found","message":"No such pipeline."}}`))
	}
	send := func(status int, p *DataPipeline) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"pipeline": p})
	}

	switch {
	case r.Method == http.MethodGet && action == "":
		if p == nil {
			notFound()
			return
		}
		send(200, p)
	case r.Method == http.MethodPut && action == "":
		raw := map[string]any{}
		buf, _ := readAll(r)
		_ = json.Unmarshal(buf, &raw)
		f.lastPut = raw
		var body PutDataPipelineRequest
		_ = json.Unmarshal(buf, &body)
		status := 200
		if p == nil {
			status = 201
			p = &DataPipeline{Name: name, DesiredState: "running", Status: "pending", CreatedAt: "2026-10-25T10:00:00Z"}
			f.pipelines[name] = p
		}
		p.Instance.ID = body.InstanceID
		instName := "orders"
		p.Instance.Name = &instName
		p.SourceType, p.SourceHost, p.SourcePort = body.SourceType, body.SourceHost, body.SourcePort
		p.SourceDatabase, p.SourceUser, p.SourceTLS = body.SourceDatabase, body.SourceUser, body.SourceTLS
		p.Tables = body.Tables
		send(status, p)
	case r.Method == http.MethodPost && (action == "start" || action == "stop" || action == "resnapshot"):
		if p == nil {
			notFound()
			return
		}
		switch action {
		case "start":
			p.DesiredState, p.Status = "running", "pending"
		case "stop":
			p.DesiredState, p.Status = "stopped", "stopped"
		case "resnapshot":
			p.SnapshotDone = false
		}
		send(200, p)
	case r.Method == http.MethodDelete && action == "":
		if p == nil {
			notFound()
			return
		}
		delete(f.pipelines, name)
		if f.deleteOut != "" {
			_, _ = w.Write([]byte(`{"deleted":true,"slot_left_in_source":"` + f.deleteOut + `"}`))
			return
		}
		w.WriteHeader(204)
	default:
		f.t.Errorf("unexpected %s", call)
		w.WriteHeader(500)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return []byte(b.String()), nil
		}
	}
}

func (f *fakePipelineAPI) took(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (f *fakePipelineAPI) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.lastPut = nil
}

func pipelineSchema(t *testing.T) rschema.Schema {
	t.Helper()
	var res resource.SchemaResponse
	NewDataPipelineResource().Schema(context.Background(), resource.SchemaRequest{}, &res)
	failOn(t, res.Diagnostics)
	failOn(t, res.Schema.ValidateImplementation(context.Background()))
	return res.Schema
}

func tablesValue(t *testing.T, tables ...pipelineTableModel) types.List {
	t.Helper()
	v, d := types.ListValueFrom(context.Background(), types.ObjectType{AttrTypes: pipelineTableAttrTypes}, tables)
	failOn(t, d)
	return v
}

func customersTable() pipelineTableModel {
	return pipelineTableModel{
		Table:      types.StringValue("public.customers"),
		Key:        types.StringValue("customer:{id}"),
		Type:       types.StringValue("hash"),
		Columns:    types.ListNull(types.StringType),
		Rename:     types.MapNull(types.StringType),
		TTLSeconds: types.Int64Null(),
	}
}

// plannedPipeline is what Terraform plans for a new pipeline: the configured
// values, defaults filled in, every computed attribute unknown.
func plannedPipeline(t *testing.T, enabled bool) dataPipelineModel {
	orders := pipelineTableModel{
		Table:      types.StringValue("public.orders"),
		Key:        types.StringValue("order:{id}"),
		Type:       types.StringValue("json"),
		Columns:    stringList([]string{"id", "total"}),
		Rename:     types.MapValueMust(types.StringType, map[string]attr.Value{"total": types.StringValue("amount")}),
		TTLSeconds: types.Int64Value(3600),
	}
	return dataPipelineModel{
		ID:                types.StringUnknown(),
		Name:              types.StringValue("orders-cdc"),
		InstanceID:        types.StringValue(testInstanceID),
		SourceType:        types.StringValue("postgres"),
		SourceHost:        types.StringValue("db.example.com"),
		SourcePort:        types.Int64Value(5432),
		SourceDatabase:    types.StringValue("shop"),
		SourceUser:        types.StringValue("kividb"),
		SourcePassword:    types.StringValue("s3cret"),
		SourceTLS:         types.StringValue("require"),
		Tables:            tablesValue(t, customersTable(), orders),
		Enabled:           types.BoolValue(enabled),
		ForceDelete:       types.BoolValue(false),
		ResnapshotTrigger: types.StringNull(),

		InstanceName:        types.StringUnknown(),
		Status:              types.StringUnknown(),
		SlotName:            types.StringUnknown(),
		SnapshotDone:        types.BoolUnknown(),
		RowsSnapshotted:     types.Int64Unknown(),
		ChangesApplied:      types.Int64Unknown(),
		LastChangeAt:        types.StringUnknown(),
		LagMS:               types.Int64Unknown(),
		LastError:           types.StringUnknown(),
		LastErrorAt:         types.StringUnknown(),
		ConsecutiveFailures: types.Int64Unknown(),
		CreatedAt:           types.StringUnknown(),
	}
}

func dpPlanOf(t *testing.T, sch rschema.Schema, m dataPipelineModel) tfsdk.Plan {
	t.Helper()
	p := tfsdk.Plan{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(context.Background()), nil)}
	failOn(t, p.Set(context.Background(), &m))
	return p
}

func dpEmptyState(sch rschema.Schema) tfsdk.State {
	return tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(context.Background()), nil)}
}

func createPipeline(t *testing.T, r *dataPipelineResource, sch rschema.Schema, m dataPipelineModel) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	res := resource.CreateResponse{State: dpEmptyState(sch)}
	r.Create(context.Background(), resource.CreateRequest{Plan: dpPlanOf(t, sch, m)}, &res)
	return res.State, res.Diagnostics
}

func stateModel(t *testing.T, s tfsdk.State) dataPipelineModel {
	t.Helper()
	var m dataPipelineModel
	failOn(t, s.Get(context.Background(), &m))
	return m
}

func dpDiagText(d diag.Diagnostics) string {
	var b strings.Builder
	for _, e := range d {
		b.WriteString(e.Summary() + ": " + e.Detail() + "\n")
	}
	return b.String()
}

// --- create ---------------------------------------------------------------

func TestPipelineCreateSendsWhatTheApiParses(t *testing.T) {
	f, srv := newFakePipelineAPI(t)
	r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
	sch := pipelineSchema(t)

	state, d := createPipeline(t, r, sch, plannedPipeline(t, true))
	failOn(t, d)

	if !f.took("GET /data-integration/orders-cdc") || !f.took("PUT /data-integration/orders-cdc") {
		t.Fatalf("create must look for the name, then PUT: %v", f.calls)
	}
	if f.took("POST /data-integration/orders-cdc/stop") {
		t.Errorf("an enabled pipeline must not be stopped: %v", f.calls)
	}
	body := f.lastPut
	if body["source_password"] != "s3cret" || body["instance_id"] != testInstanceID || body["source_port"] != float64(5432) {
		t.Errorf("PUT body wrong: %v", body)
	}
	if _, present := body["name"]; present {
		t.Errorf("the name travels in the path, not the body: %v", body)
	}
	tables := body["tables"].([]any)
	first := tables[0].(map[string]any)
	if first["type"] != "hash" {
		t.Errorf("type default not sent: %v", first)
	}
	for _, k := range []string{"columns", "rename", "ttl_seconds"} {
		if _, present := first[k]; present {
			t.Errorf("unset %s must be left out, not sent as null: %v", k, first)
		}
	}
	second := tables[1].(map[string]any)
	if second["ttl_seconds"] != float64(3600) || second["rename"].(map[string]any)["total"] != "amount" {
		t.Errorf("second table wrong: %v", second)
	}

	got := stateModel(t, state)
	if got.ID.ValueString() != "orders-cdc" || got.Status.ValueString() != "pending" || !got.Enabled.ValueBool() {
		t.Errorf("state wrong: %+v", got)
	}
	if got.SourcePassword.ValueString() != "s3cret" {
		t.Errorf("the API never returns the password; the configured one must stay in state, got %v", got.SourcePassword)
	}
	if !got.SlotName.IsNull() || got.InstanceName.ValueString() != "orders" {
		t.Errorf("computed attributes not read: %+v", got)
	}
}

func TestPipelineCreatedDisabledIsStopped(t *testing.T) {
	f, srv := newFakePipelineAPI(t)
	r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
	sch := pipelineSchema(t)

	state, d := createPipeline(t, r, sch, plannedPipeline(t, false))
	failOn(t, d)
	if !f.took("POST /data-integration/orders-cdc/stop") {
		t.Errorf("enabled = false must stop the new pipeline: %v", f.calls)
	}
	if got := stateModel(t, state); got.Enabled.ValueBool() || got.Status.ValueString() != "stopped" {
		t.Errorf("state = %+v", got)
	}
}

// PUT is an upsert. A create must not silently rewrite a pipeline that already
// has the name.
func TestPipelineCreateRefusesAnExistingName(t *testing.T) {
	f, srv := newFakePipelineAPI(t)
	f.pipelines["orders-cdc"] = &DataPipeline{Name: "orders-cdc", DesiredState: "running"}
	r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
	sch := pipelineSchema(t)

	_, d := createPipeline(t, r, sch, plannedPipeline(t, true))
	if !d.HasError() || !strings.Contains(dpDiagText(d), "terraform import") {
		t.Fatalf("expected a dpRefusal pointing at import, got %v", d)
	}
	if f.took("PUT /data-integration/orders-cdc") {
		t.Errorf("the existing pipeline was overwritten")
	}
}

func TestPipelineCreateRefusalsKeepTheServersWords(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"credit", `{"error":{"code":"insufficient_credits","message":"A pipeline costs about $29.20 a month at $0.04 an hour. Add $12.00 in credits to run it."}}`,
			"Add $12.00 in credits to run it.", 402},
		{"source", `{"error":{"code":"source_check_failed","message":"The source has wal_level = replica; it must be logical."}}`,
			"The source has wal_level = replica; it must be logical.", 422},
		{"address", `{"error":{"code":"source_not_allowed","message":"The source host db.internal resolves to 10.0.0.5, a private or reserved address. Give the database's public address."}}`,
			"resolves to 10.0.0.5, a private or reserved address", 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakePipelineAPI(t)
			f.refuse["PUT /data-integration/orders-cdc"] = dpRefusal{tc.status, tc.body}
			r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
			state, d := createPipeline(t, r, pipelineSchema(t), plannedPipeline(t, true))
			if !d.HasError() || !strings.Contains(dpDiagText(d), tc.want) {
				t.Fatalf("the API's message was lost: %s", dpDiagText(d))
			}
			if !state.Raw.IsNull() {
				t.Errorf("a refused create must write no state")
			}
		})
	}
}

// --- read -----------------------------------------------------------------

func TestPipelineReadKeepsThePasswordAndDropsAGonePipeline(t *testing.T) {
	f, srv := newFakePipelineAPI(t)
	r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
	sch := pipelineSchema(t)
	state, d := createPipeline(t, r, sch, plannedPipeline(t, true))
	failOn(t, d)

	// Progress moves on between refreshes.
	lag := int64(850)
	f.pipelines["orders-cdc"].Status = "streaming"
	f.pipelines["orders-cdc"].ChangesApplied = 42
	f.pipelines["orders-cdc"].LagMS = &lag

	res := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &res)
	failOn(t, res.Diagnostics)
	got := stateModel(t, res.State)
	if got.Status.ValueString() != "streaming" || got.ChangesApplied.ValueInt64() != 42 || got.LagMS.ValueInt64() != 850 {
		t.Errorf("progress not read: %+v", got)
	}
	if got.SourcePassword.ValueString() != "s3cret" {
		t.Errorf("a refresh must keep the configured password, got %v", got.SourcePassword)
	}

	// Stopped in the console: the refresh says so, and the plan will start it.
	f.pipelines["orders-cdc"].DesiredState = "stopped"
	r.Read(context.Background(), resource.ReadRequest{State: res.State}, &res)
	failOn(t, res.Diagnostics)
	if stateModel(t, res.State).Enabled.ValueBool() {
		t.Errorf("enabled must follow desired_state")
	}

	delete(f.pipelines, "orders-cdc")
	r.Read(context.Background(), resource.ReadRequest{State: res.State}, &res)
	failOn(t, res.Diagnostics)
	if !res.State.Raw.IsNull() {
		t.Errorf("a pipeline that is gone must be removed from state")
	}
}

// --- update ---------------------------------------------------------------

func updatePipeline(t *testing.T, r *dataPipelineResource, sch rschema.Schema, prior tfsdk.State, plan dataPipelineModel) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	// The framework hands Update a null state, not the prior one.
	res := resource.UpdateResponse{State: dpEmptyState(sch)}
	r.Update(context.Background(), resource.UpdateRequest{Plan: dpPlanOf(t, sch, plan), State: prior}, &res)
	return res.State, res.Diagnostics
}

// nextPlan is the plan for an existing pipeline: the state with the computed
// progress attributes unknown, as Terraform plans them.
func nextPlan(t *testing.T, s tfsdk.State) dataPipelineModel {
	m := stateModel(t, s)
	m.Status, m.SlotName, m.LastChangeAt = types.StringUnknown(), types.StringUnknown(), types.StringUnknown()
	m.LastError, m.LastErrorAt, m.InstanceName = types.StringUnknown(), types.StringUnknown(), types.StringUnknown()
	m.SnapshotDone = types.BoolUnknown()
	m.RowsSnapshotted, m.ChangesApplied, m.LagMS, m.ConsecutiveFailures =
		types.Int64Unknown(), types.Int64Unknown(), types.Int64Unknown(), types.Int64Unknown()
	return m
}

func TestPipelineUpdateDoesOnlyWhatChanged(t *testing.T) {
	f, srv := newFakePipelineAPI(t)
	r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
	sch := pipelineSchema(t)
	state, d := createPipeline(t, r, sch, plannedPipeline(t, true))
	failOn(t, d)

	// Disabling stops it, and sends nothing else: a PUT re-checks the source.
	f.reset()
	plan := nextPlan(t, state)
	plan.Enabled = types.BoolValue(false)
	state, d = updatePipeline(t, r, sch, state, plan)
	failOn(t, d)
	if !f.took("POST /data-integration/orders-cdc/stop") || f.took("PUT /data-integration/orders-cdc") {
		t.Errorf("disable should only stop: %v", f.calls)
	}
	if stateModel(t, state).Enabled.ValueBool() {
		t.Errorf("state still enabled")
	}

	// Enabling starts it.
	f.reset()
	plan = nextPlan(t, state)
	plan.Enabled = types.BoolValue(true)
	state, d = updatePipeline(t, r, sch, state, plan)
	failOn(t, d)
	if !f.took("POST /data-integration/orders-cdc/start") || f.took("PUT /data-integration/orders-cdc") {
		t.Errorf("enable should only start: %v", f.calls)
	}

	// A settings change is a PUT, with the password.
	f.reset()
	plan = nextPlan(t, state)
	plan.SourceHost = types.StringValue("db2.example.com")
	state, d = updatePipeline(t, r, sch, state, plan)
	failOn(t, d)
	if !f.took("PUT /data-integration/orders-cdc") || f.lastPut["source_host"] != "db2.example.com" || f.lastPut["source_password"] != "s3cret" {
		t.Errorf("settings change not PUT: %v %v", f.calls, f.lastPut)
	}
	if f.took("POST /data-integration/orders-cdc/start") || f.took("POST /data-integration/orders-cdc/stop") {
		t.Errorf("a settings change must not start or stop: %v", f.calls)
	}

	// The trigger asks for a fresh copy, and only that.
	f.reset()
	plan = nextPlan(t, state)
	plan.ResnapshotTrigger = types.StringValue("2026-10-25")
	state, d = updatePipeline(t, r, sch, state, plan)
	failOn(t, d)
	if !f.took("POST /data-integration/orders-cdc/resnapshot") || f.took("PUT /data-integration/orders-cdc") {
		t.Errorf("trigger should resnapshot only: %v", f.calls)
	}
	if stateModel(t, state).ResnapshotTrigger.ValueString() != "2026-10-25" {
		t.Errorf("trigger not kept in state")
	}

	// force_delete is local: nothing is sent.
	f.reset()
	plan = nextPlan(t, state)
	plan.ForceDelete = types.BoolValue(true)
	state, d = updatePipeline(t, r, sch, state, plan)
	failOn(t, d)
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("force_delete changed and the API was asked to change something: %v", f.calls)
		}
	}
	if !stateModel(t, state).ForceDelete.ValueBool() {
		t.Errorf("force_delete not kept")
	}
}

func TestPipelineStartRefusedForCreditKeepsTheServersWords(t *testing.T) {
	f, srv := newFakePipelineAPI(t)
	r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
	sch := pipelineSchema(t)
	state, d := createPipeline(t, r, sch, plannedPipeline(t, false))
	failOn(t, d)

	msg := "A pipeline costs about $29.20 a month at $0.04 an hour, on top of $10.00 a month already committed to your running databases and pipelines, and your balance is $5.00. Add $34.20 in credits to run it."
	f.refuse["POST /data-integration/orders-cdc/start"] = dpRefusal{402, `{"error":{"code":"insufficient_credits","message":"` + msg + `"}}`}
	plan := nextPlan(t, state)
	plan.Enabled = types.BoolValue(true)
	state, d = updatePipeline(t, r, sch, state, plan)
	if !d.HasError() || !strings.Contains(dpDiagText(d), msg) {
		t.Fatalf("credit dpRefusal lost: %s", dpDiagText(d))
	}
	if stateModel(t, state).Enabled.ValueBool() {
		t.Errorf("a refused start must leave state saying stopped, so the next plan tries again")
	}
}

// --- delete ---------------------------------------------------------------

func deletePipeline(r *dataPipelineResource, s tfsdk.State) resource.DeleteResponse {
	res := resource.DeleteResponse{State: s}
	r.Delete(context.Background(), resource.DeleteRequest{State: s}, &res)
	return res
}

func TestPipelineDelete(t *testing.T) {
	f, srv := newFakePipelineAPI(t)
	r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
	sch := pipelineSchema(t)
	state, d := createPipeline(t, r, sch, plannedPipeline(t, true))
	failOn(t, d)

	res := deletePipeline(r, state)
	failOn(t, res.Diagnostics)
	if !f.took("DELETE /data-integration/orders-cdc") || len(f.pipelines) != 0 {
		t.Errorf("not deleted: %v", f.calls)
	}

	// Already gone is not a failure.
	res = deletePipeline(r, state)
	failOn(t, res.Diagnostics)
}

func TestPipelineDeleteRefusedForCleanupKeepsTheServersWords(t *testing.T) {
	f, srv := newFakePipelineAPI(t)
	r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
	sch := pipelineSchema(t)
	state, d := createPipeline(t, r, sch, plannedPipeline(t, true))
	failOn(t, d)

	msg := "Could not drop the replication slot kividb_0123456789abcdef in the source: connect ETIMEDOUT. The pipeline is stopped. Fix the connection and delete again, or delete with ?force=true: KiviDB then keeps trying to drop it for about four days and tells you if it cannot."
	f.refuse["DELETE /data-integration/orders-cdc"] = dpRefusal{409, `{"error":{"code":"source_cleanup_failed","message":"` + msg + `"}}`}
	res := deletePipeline(r, state)
	text := dpDiagText(res.Diagnostics)
	if !res.Diagnostics.HasError() || !strings.Contains(text, msg) || !strings.Contains(text, "force_delete = true") {
		t.Fatalf("cleanup dpRefusal not surfaced verbatim with the Terraform way out: %s", text)
	}
	if f.took("DELETE /data-integration/orders-cdc?force=true") {
		t.Errorf("force was sent without force_delete")
	}
}

func TestPipelineForceDeleteWarnsAboutTheSlotLeftBehind(t *testing.T) {
	f, srv := newFakePipelineAPI(t)
	f.deleteOut = "kividb_0123456789abcdef"
	r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
	sch := pipelineSchema(t)
	m := plannedPipeline(t, true)
	m.ForceDelete = types.BoolValue(true)
	state, d := createPipeline(t, r, sch, m)
	failOn(t, d)

	res := deletePipeline(r, state)
	failOn(t, res.Diagnostics)
	if !f.took("DELETE /data-integration/orders-cdc?force=true") {
		t.Errorf("force_delete did not send ?force=true: %v", f.calls)
	}
	if res.Diagnostics.WarningsCount() != 1 || !strings.Contains(dpDiagText(res.Diagnostics), "kividb_0123456789abcdef") {
		t.Errorf("the slot left in the source must be named in a warning: %s", dpDiagText(res.Diagnostics))
	}
}

// --- import ---------------------------------------------------------------

func TestPipelineImportByName(t *testing.T) {
	f, srv := newFakePipelineAPI(t)
	r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
	sch := pipelineSchema(t)
	_, d := createPipeline(t, r, sch, plannedPipeline(t, false))
	failOn(t, d)
	_ = f

	res := resource.ImportStateResponse{State: dpEmptyState(sch)}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "orders-cdc"}, &res)
	failOn(t, res.Diagnostics)
	got := stateModel(t, res.State)
	if got.ID.ValueString() != "orders-cdc" || got.InstanceID.ValueString() != testInstanceID || got.Enabled.ValueBool() {
		t.Errorf("import state wrong: %+v", got)
	}
	if !got.SourcePassword.IsNull() || got.ForceDelete.ValueBool() {
		t.Errorf("import must not invent a password, and force_delete starts false: %+v", got)
	}
	var tables []pipelineTableModel
	failOn(t, got.Tables.ElementsAs(context.Background(), &tables, false))
	if len(tables) != 2 || tables[1].TTLSeconds.ValueInt64() != 3600 || !tables[0].Columns.IsNull() {
		t.Errorf("tables not imported as stored: %+v", tables)
	}

	res = resource.ImportStateResponse{State: dpEmptyState(sch)}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "nope"}, &res)
	if !res.Diagnostics.HasError() || !strings.Contains(dpDiagText(res.Diagnostics), "No such pipeline.") {
		t.Errorf("importing a missing pipeline must say so: %s", dpDiagText(res.Diagnostics))
	}
}

// --- validation -----------------------------------------------------------

func validatePipeline(t *testing.T, m dataPipelineModel) diag.Diagnostics {
	t.Helper()
	sch := pipelineSchema(t)
	cfg := tfsdk.Config{Schema: sch, Raw: dpPlanOf(t, sch, m).Raw}
	var res resource.ValidateConfigResponse
	(&dataPipelineResource{}).ValidateConfig(context.Background(), resource.ValidateConfigRequest{Config: cfg}, &res)
	return res.Diagnostics
}

func TestPipelineValidationMirrorsTheApi(t *testing.T) {
	if d := validatePipeline(t, plannedPipeline(t, true)); d.HasError() {
		t.Fatalf("a valid pipeline was refused: %s", dpDiagText(d))
	}

	dup := plannedPipeline(t, true)
	dup.Tables = tablesValue(t, customersTable(), customersTable())
	if d := validatePipeline(t, dup); !strings.Contains(dpDiagText(d), "listed more than once") {
		t.Errorf("duplicate table not caught: %s", dpDiagText(d))
	}

	my := plannedPipeline(t, true)
	my.SourceType = types.StringValue("mysql")
	my.Tables = tablesValue(t, customersTable())
	if d := validatePipeline(t, my); !strings.Contains(dpDiagText(d), "without a database") {
		t.Errorf("dotted MySQL table not caught: %s", dpDiagText(d))
	}

	cols := plannedPipeline(t, true)
	tbl := customersTable()
	tbl.Columns = stringList([]string{"name"})
	cols.Tables = tablesValue(t, tbl)
	if d := validatePipeline(t, cols); !strings.Contains(dpDiagText(d), "{id} is not one of the columns") {
		t.Errorf("placeholder outside columns not caught: %s", dpDiagText(d))
	}
}

func TestPipelineAttributeValidators(t *testing.T) {
	sch := pipelineSchema(t)
	check := func(attrName, value string, ok bool) {
		t.Helper()
		a := sch.Attributes[attrName].(rschema.StringAttribute)
		d := runStringValidators(a.Validators, value)
		if d.HasError() == ok {
			t.Errorf("%s = %q: ok=%v, diagnostics %v", attrName, value, ok, d)
		}
	}
	check("name", "orders-cdc", true)
	check("name", "Orders", false)
	check("name", "-orders", false)
	check("source_host", "db.example.com", true)
	check("source_host", "db example", false)
	check("source_user", " kividb", false)
	check("source_tls", "verify", true)
	check("source_tls", "prefer", false)
	check("instance_id", testInstanceID, true)
	check("instance_id", "orders", false)

	nested := sch.Attributes["tables"].(rschema.ListNestedAttribute).NestedObject.Attributes
	key := nested["key"].(rschema.StringAttribute)
	if runStringValidators(key.Validators, "customer:{id}").HasError() {
		t.Errorf("a good key was refused")
	}
	for _, bad := range []string{"customer", "customer: {id}", "{}"} {
		if !runStringValidators(key.Validators, bad).HasError() {
			t.Errorf("key %q should be refused", bad)
		}
	}
}

// Changing what a pipeline is replaces it; the same database id in another
// case does not.
func TestPipelineReplacement(t *testing.T) {
	sch := pipelineSchema(t)
	for _, n := range []string{"name", "source_type"} {
		if !hasRequiresReplace(sch.Attributes[n].(rschema.StringAttribute).PlanModifiers) {
			t.Errorf("%s must force replacement", n)
		}
	}
	for _, n := range []string{"source_host", "source_password", "source_user", "source_database"} {
		if len(sch.Attributes[n].(rschema.StringAttribute).PlanModifiers) != 0 {
			t.Errorf("%s changes in place", n)
		}
	}

	mod := replaceUnlessSameID()
	run := func(old, new string) bool {
		res := planmodifier.StringResponse{PlanValue: types.StringValue(new)}
		state := tfsdk.State{Schema: sch, Raw: dpPlanOf(t, sch, plannedPipeline(t, true)).Raw}
		plan := tfsdk.Plan{Schema: sch, Raw: state.Raw}
		mod.PlanModifyString(context.Background(), planmodifier.StringRequest{
			StateValue: types.StringValue(old), PlanValue: types.StringValue(new), ConfigValue: types.StringValue(new),
			State: state, Plan: plan,
		}, &res)
		return res.RequiresReplace
	}
	if run(testInstanceID, strings.ToUpper(testInstanceID)) {
		t.Errorf("the same id in upper case must not replace the pipeline")
	}
	if !run(testInstanceID, testAccountID) {
		t.Errorf("another database must replace the pipeline")
	}
}

func hasRequiresReplace(mods []planmodifier.String) bool {
	for _, m := range mods {
		if strings.Contains(m.Description(context.Background()), "replace") ||
			strings.Contains(m.Description(context.Background()), "destroy and recreate") {
			return true
		}
	}
	return false
}

// A refused change keeps the state as it was, so the next plan proposes the
// change again rather than believing it happened.
func TestPipelineRefusedChangeKeepsTheState(t *testing.T) {
	f, srv := newFakePipelineAPI(t)
	r := &dataPipelineResource{client: NewClient(srv.URL, "k")}
	sch := pipelineSchema(t)
	state, d := createPipeline(t, r, sch, plannedPipeline(t, true))
	failOn(t, d)

	msg := "connect ECONNREFUSED 203.0.113.7:5432"
	f.refuse["PUT /data-integration/orders-cdc"] = dpRefusal{422, `{"error":{"code":"source_check_failed","message":"` + msg + `"}}`}
	plan := nextPlan(t, state)
	plan.SourceHost = types.StringValue("db2.example.com")
	after, d := updatePipeline(t, r, sch, state, plan)
	if !d.HasError() || !strings.Contains(dpDiagText(d), msg) {
		t.Fatalf("source check dpRefusal lost: %s", dpDiagText(d))
	}
	if got := stateModel(t, after); got.SourceHost.ValueString() != "db.example.com" {
		t.Errorf("state took the refused host: %v", got.SourceHost)
	}
}
