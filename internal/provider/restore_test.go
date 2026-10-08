package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	testDiskSnapshotID = "5b2d7c1e-8f3a-4d6b-9e0c-1a2b3c4d5e6f"
	testKdbSnapshotID  = "9c8b7a6f-5e4d-4c3b-8a29-1f0e9d8c7b6a"
)

// What GET /disk-snapshots/:id/restore-plan answers for a snapshot in a
// connected account. Trimmed to the fields the provider reads, plus a couple it
// does not, so a field added on the server cannot break the decode.
const diskRestorePlanJSON = `{"snapshot_id":"` + testDiskSnapshotID + `","label":"nightly","cloud":"aws",
	"region":"eu-central-1","size_gb":"6.00","recommended_data_size_gb":8,
	"cloud_account_id":"` + testAccountID + `","cloud_account_name":"production",
	"eligible_instances":[],"can_restore_in_place":false}`

// --- planning where a database made from a snapshot goes --------------------

func TestPlanInheritsTheSnapshotsAccount(t *testing.T) {
	srv := restorePlanServer(t, "/disk-snapshots/"+testDiskSnapshotID+"/restore-plan", http.StatusOK, diskRestorePlanJSON)
	defer srv.Close()

	res := modifyCreatePlan(t, srv.URL, map[string]tftypes.Value{
		"restore_from_snapshot_id": tftypes.NewValue(tftypes.String, testDiskSnapshotID),
	})
	failOn(t, res.Diagnostics)

	got := plannedString(t, res.Plan, "cloud_account_id")
	if got.ValueString() != testAccountID {
		t.Errorf("an unset cloud_account_id must plan as the snapshot's account, got %v", got)
	}
	// The name is promised only once the API has answered with one.
	if name := plannedString(t, res.Plan, "cloud_account_name"); !name.IsUnknown() {
		t.Errorf("cloud_account_name must be unknown on a create, got %v", name)
	}
}

func TestPlanForASnapshotInKiviDBsCloudIsKiviDBsCloud(t *testing.T) {
	body := strings.Replace(diskRestorePlanJSON, `"cloud_account_id":"`+testAccountID+`","cloud_account_name":"production"`,
		`"cloud_account_id":null,"cloud_account_name":null`, 1)
	srv := restorePlanServer(t, "/disk-snapshots/"+testDiskSnapshotID+"/restore-plan", http.StatusOK, body)
	defer srv.Close()

	res := modifyCreatePlan(t, srv.URL, map[string]tftypes.Value{
		"restore_from_snapshot_id": tftypes.NewValue(tftypes.String, testDiskSnapshotID),
	})
	failOn(t, res.Diagnostics)
	if got := plannedString(t, res.Plan, "cloud_account_id"); !got.IsNull() {
		t.Errorf("a snapshot in KiviDB's cloud makes a database there: got %v", got)
	}
	if got := plannedString(t, res.Plan, "cloud_account_name"); !got.IsNull() {
		t.Errorf("no account, no account name: got %v", got)
	}
}

func TestPlanRefusesASnapshotsWrongPlacement(t *testing.T) {
	srv := restorePlanServer(t, "/disk-snapshots/"+testDiskSnapshotID+"/restore-plan", http.StatusOK, diskRestorePlanJSON)
	defer srv.Close()

	for _, tc := range []struct {
		name     string
		vals     map[string]tftypes.Value
		wantPath string
		wantText []string
	}{
		{
			name: "another account",
			vals: map[string]tftypes.Value{
				"cloud_account_id": tftypes.NewValue(tftypes.String, testOtherID),
			},
			wantPath: "cloud_account_id",
			wantText: []string{`"production"`, testAccountID, "can only be restored there"},
		},
		{
			name: "another cloud",
			vals: map[string]tftypes.Value{
				"cloud": tftypes.NewValue(tftypes.String, "azure"),
			},
			wantPath: "cloud",
			wantText: []string{"is on aws", `cloud = "aws"`},
		},
		{
			name: "another region",
			vals: map[string]tftypes.Value{
				"region": tftypes.NewValue(tftypes.String, "us-east-1"),
			},
			wantPath: "region",
			wantText: []string{"is in eu-central-1", `region = "eu-central-1"`, "move the snapshot"},
		},
		{
			// The account is checked first: no move can fix it, and a message
			// about the region would send somebody the wrong way.
			name: "account and region both wrong",
			vals: map[string]tftypes.Value{
				"cloud_account_id": tftypes.NewValue(tftypes.String, testOtherID),
				"region":           tftypes.NewValue(tftypes.String, "us-east-1"),
			},
			wantPath: "cloud_account_id",
			wantText: []string{"different cloud account"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vals := map[string]tftypes.Value{
				"restore_from_snapshot_id": tftypes.NewValue(tftypes.String, testDiskSnapshotID),
			}
			for k, v := range tc.vals {
				vals[k] = v
			}
			res := modifyCreatePlan(t, srv.URL, vals)
			if !res.Diagnostics.HasError() {
				t.Fatal("expected the plan to be refused")
			}
			if n := res.Diagnostics.ErrorsCount(); n != 1 {
				t.Errorf("one refusal at a time, got %d: %v", n, res.Diagnostics.Errors())
			}
			e := res.Diagnostics.Errors()[0]
			withPath, ok := e.(diag.DiagnosticWithPath)
			if !ok || !withPath.Path().Equal(path.Root(tc.wantPath)) {
				t.Errorf("the error must point at %s: %v", tc.wantPath, e)
			}
			for _, want := range tc.wantText {
				if !strings.Contains(e.Summary()+" "+e.Detail(), want) {
					t.Errorf("error does not say %q: %s / %s", want, e.Summary(), e.Detail())
				}
			}
		})
	}
}

func TestPlanRefusesAnAccountForASnapshotInKiviDBsCloud(t *testing.T) {
	body := strings.Replace(diskRestorePlanJSON, `"cloud_account_id":"`+testAccountID+`"`, `"cloud_account_id":null`, 1)
	srv := restorePlanServer(t, "/disk-snapshots/"+testDiskSnapshotID+"/restore-plan", http.StatusOK, body)
	defer srv.Close()

	res := modifyCreatePlan(t, srv.URL, map[string]tftypes.Value{
		"restore_from_snapshot_id": tftypes.NewValue(tftypes.String, testDiskSnapshotID),
		"cloud_account_id":         tftypes.NewValue(tftypes.String, testAccountID),
	})
	if !res.Diagnostics.HasError() {
		t.Fatal("a snapshot in KiviDB's cloud cannot make a database in a connected account")
	}
	if d := res.Diagnostics.Errors()[0].Detail(); !strings.Contains(d, "KiviDB's cloud") || !strings.Contains(d, "Remove `cloud_account_id`") {
		t.Errorf("the error must say to remove the account: %q", d)
	}
}

// The configured account may be spelled in upper case; the API answers in lower.
func TestPlanAcceptsTheSnapshotsOwnAccountInAnyCase(t *testing.T) {
	srv := restorePlanServer(t, "/disk-snapshots/"+testDiskSnapshotID+"/restore-plan", http.StatusOK, diskRestorePlanJSON)
	defer srv.Close()
	res := modifyCreatePlan(t, srv.URL, map[string]tftypes.Value{
		"restore_from_snapshot_id": tftypes.NewValue(tftypes.String, testDiskSnapshotID),
		"cloud_account_id":         tftypes.NewValue(tftypes.String, strings.ToUpper(testAccountID)),
	})
	failOn(t, res.Diagnostics)
	if got := plannedString(t, res.Plan, "cloud_account_id"); got.ValueString() != strings.ToUpper(testAccountID) {
		t.Errorf("a configured value must be planned as written: %v", got)
	}
}

func TestPlanReadsAKdbSnapshotsPlacement(t *testing.T) {
	body := strings.NewReplacer(testDiskSnapshotID, testKdbSnapshotID, `"cloud":"aws"`, `"cloud":"azure"`,
		`"region":"eu-central-1"`, `"region":"swedencentral"`).Replace(diskRestorePlanJSON)
	srv := restorePlanServer(t, "/kdb-snapshots/"+testKdbSnapshotID+"/restore-plan", http.StatusOK, body)
	defer srv.Close()

	res := modifyCreatePlan(t, srv.URL, map[string]tftypes.Value{
		"restore_from_kdb_snapshot_id": tftypes.NewValue(tftypes.String, testKdbSnapshotID),
		"cloud":                        tftypes.NewValue(tftypes.String, "azure"),
		"region":                       tftypes.NewValue(tftypes.String, "westeurope"),
	})
	if !res.Diagnostics.HasError() {
		t.Fatal("a KDB snapshot in swedencentral cannot make a database in westeurope")
	}
	if d := res.Diagnostics.Errors()[0].Detail(); !strings.HasPrefix(d, "KDB snapshot") || !strings.Contains(d, "swedencentral") {
		t.Errorf("the error must name the KDB snapshot and its region: %q", d)
	}
}

func TestPlanWhenTheSnapshotCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantError bool
		wantText  string
	}{
		{"missing", http.StatusNotFound, `{"error":{"code":"not_found"}}`, true, "No disk snapshot"},
		{"not finished", http.StatusConflict, `{"error":{"code":"snapshot_not_ready"}}`, false, "still being written"},
		{"server trouble", http.StatusInternalServerError, `{"error":{"code":"internal","message":"boom"}}`, false, "checked at apply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := restorePlanServer(t, "/disk-snapshots/"+testDiskSnapshotID+"/restore-plan", tc.status, tc.body)
			defer srv.Close()
			res := modifyCreatePlan(t, srv.URL, map[string]tftypes.Value{
				"restore_from_snapshot_id": tftypes.NewValue(tftypes.String, testDiskSnapshotID),
			})
			if res.Diagnostics.HasError() != tc.wantError {
				t.Fatalf("error = %v, want %v: %v", res.Diagnostics.HasError(), tc.wantError, res.Diagnostics)
			}
			all := ""
			for _, d := range res.Diagnostics {
				all += d.Summary() + " " + d.Detail() + "\n"
			}
			if !strings.Contains(all, tc.wantText) {
				t.Errorf("diagnostics do not say %q: %s", tc.wantText, all)
			}
			if !tc.wantError {
				// Not resolved: the API picks the snapshot's account at apply.
				if got := plannedString(t, res.Plan, "cloud_account_id"); !got.IsUnknown() {
					t.Errorf("an unread snapshot's account must stay unknown, got %v", got)
				}
			}
		})
	}
}

// A snapshot id from a resource in the same apply is unknown at plan time.
// Nothing is looked up, and the account is left for the API to choose.
func TestPlanWithAnUnknownSnapshotLooksNothingUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("nothing should be called, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	res := modifyCreatePlan(t, srv.URL, map[string]tftypes.Value{
		"restore_from_snapshot_id": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	})
	failOn(t, res.Diagnostics)
	if got := plannedString(t, res.Plan, "cloud_account_id"); !got.IsUnknown() {
		t.Errorf("cloud_account_id must be unknown, got %v", got)
	}
}

// A database not made from a snapshot is planned exactly as before the
// attribute became computed: no lookups, and an unset account is null.
func TestPlanForAnOrdinaryDatabaseIsUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("nothing should be called, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	res := modifyCreatePlan(t, srv.URL, nil)
	failOn(t, res.Diagnostics)
	if got := plannedString(t, res.Plan, "cloud_account_name"); !got.IsNull() {
		t.Errorf("a database in KiviDB's cloud has no account name: %v", got)
	}
}

// Adding a snapshot to a database that already exists restores nothing and
// replaces nothing; the plan says so instead of staying silent.
func TestAddingASnapshotToAnExistingDatabaseWarns(t *testing.T) {
	ctx := context.Background()
	sch := instanceResourceSchema(t)
	typ := sch.Type().TerraformType(ctx)
	r := &instanceResource{}

	stateVals := existingStateVals()
	planVals := map[string]tftypes.Value{}
	for k, v := range stateVals {
		planVals[k] = v
	}
	planVals["restore_from_snapshot_id"] = tftypes.NewValue(tftypes.String, testDiskSnapshotID)

	cfgVals := map[string]tftypes.Value{
		"tier":                     stateVals["tier"],
		"cloud":                    stateVals["cloud"],
		"region":                   stateVals["region"],
		"data_size_gb":             stateVals["data_size_gb"],
		"restore_from_snapshot_id": tftypes.NewValue(tftypes.String, testDiskSnapshotID),
	}
	req := resource.ModifyPlanRequest{
		Config: tfsdk.Config{Schema: sch, Raw: objectOf(t, typ, cfgVals, false)},
		Plan:   tfsdk.Plan{Schema: sch, Raw: objectOf(t, typ, planVals, false)},
		State:  tfsdk.State{Schema: sch, Raw: objectOf(t, typ, stateVals, false)},
	}
	res := resource.ModifyPlanResponse{Plan: req.Plan}
	r.ModifyPlan(ctx, req, &res)
	failOn(t, res.Diagnostics)
	if res.Diagnostics.WarningsCount() != 1 ||
		!strings.Contains(res.Diagnostics.Warnings()[0].Detail(), "-replace") {
		t.Errorf("expected one warning pointing at -replace, got %v", res.Diagnostics)
	}
	// The account's name is carried over: nothing about the account changed.
	if got := plannedString(t, res.Plan, "cloud_account_name"); got.ValueString() != "production" {
		t.Errorf("cloud_account_name must be kept from state, got %v", got)
	}
}

// --- the attribute plan modifiers ---------------------------------------------

func TestUnsetCloudAccountIsPlannedFromSnapshotOrState(t *testing.T) {
	ctx := context.Background()
	sch := instanceResourceSchema(t)
	typ := sch.Type().TerraformType(ctx)

	run := func(planVals map[string]tftypes.Value, state tfsdk.State, stateValue types.String) types.String {
		req := planmodifier.StringRequest{
			Plan:        tfsdk.Plan{Schema: sch, Raw: objectOf(t, typ, planVals, false)},
			State:       state,
			ConfigValue: types.StringNull(),
			PlanValue:   types.StringUnknown(),
			StateValue:  stateValue,
		}
		res := planmodifier.StringResponse{PlanValue: req.PlanValue}
		cloudAccountFromSnapshotOrState().PlanModifyString(ctx, req, &res)
		return res.PlanValue
	}
	noState := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(typ, nil)}
	existing := tfsdk.State{Schema: sch, Raw: objectOf(t, typ, existingStateVals(), false)}
	snap := map[string]tftypes.Value{"restore_from_snapshot_id": tftypes.NewValue(tftypes.String, testDiskSnapshotID)}

	if got := run(map[string]tftypes.Value{}, noState, types.StringNull()); !got.IsNull() {
		t.Errorf("no snapshot, no account: KiviDB's cloud, planned null; got %v", got)
	}
	if got := run(snap, noState, types.StringNull()); !got.IsUnknown() {
		t.Errorf("a create from a snapshot leaves the account to ModifyPlan; got %v", got)
	}
	if got := run(snap, existing, types.StringValue(testAccountID)); got.ValueString() != testAccountID {
		t.Errorf("an existing database keeps its account; got %v", got)
	}
	// Found by planning against a real Terraform: a database whose account came
	// from its snapshot, with restore_from_snapshot_id then removed from the
	// configuration, planned into KiviDB's cloud -- a replacement. Unsetting
	// the attribute on a database that exists must never move it.
	if got := run(map[string]tftypes.Value{}, existing, types.StringValue(testAccountID)); got.ValueString() != testAccountID {
		t.Errorf("removing the snapshot must not move the database out of its account; got %v", got)
	}
	unknownSnap := map[string]tftypes.Value{"restore_from_kdb_snapshot_id": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}
	if got := run(unknownSnap, noState, types.StringNull()); !got.IsUnknown() {
		t.Errorf("an unknown snapshot id is still a restore; got %v", got)
	}

	// A configured value is never touched.
	req := planmodifier.StringRequest{
		Plan:        tfsdk.Plan{Schema: sch, Raw: objectOf(t, typ, map[string]tftypes.Value{}, false)},
		State:       noState,
		ConfigValue: types.StringValue(testAccountID),
		PlanValue:   types.StringValue(testAccountID),
		StateValue:  types.StringNull(),
	}
	res := planmodifier.StringResponse{PlanValue: req.PlanValue}
	cloudAccountFromSnapshotOrState().PlanModifyString(ctx, req, &res)
	if res.PlanValue.ValueString() != testAccountID {
		t.Errorf("a configured account must be planned as configured, got %v", res.PlanValue)
	}
}

func TestOnlyADifferentSnapshotReplacesTheDatabase(t *testing.T) {
	ctx := context.Background()
	sch := instanceResourceSchema(t)
	typ := sch.Type().TerraformType(ctx)
	state := tfsdk.State{Schema: sch, Raw: objectOf(t, typ, existingStateVals(), false)}
	plan := tfsdk.Plan{Schema: sch, Raw: objectOf(t, typ, existingStateVals(), false)}

	for _, tc := range []struct {
		name        string
		from, to    types.String
		wantReplace bool
	}{
		{"added after an import", types.StringNull(), types.StringValue(testDiskSnapshotID), false},
		{"removed after the create", types.StringValue(testDiskSnapshotID), types.StringNull(), false},
		{"a different snapshot", types.StringValue(testDiskSnapshotID), types.StringValue(testKdbSnapshotID), true},
		{"the same, in upper case", types.StringValue(testDiskSnapshotID), types.StringValue(strings.ToUpper(testDiskSnapshotID)), false},
		{"unknown", types.StringValue(testDiskSnapshotID), types.StringUnknown(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := planmodifier.StringRequest{
				Plan: plan, State: state, Config: tfsdk.Config{Schema: sch, Raw: plan.Raw},
				StateValue: tc.from, PlanValue: tc.to, ConfigValue: tc.to,
			}
			res := planmodifier.StringResponse{PlanValue: req.PlanValue}
			replaceWhenSnapshotChanges().PlanModifyString(ctx, req, &res)
			if res.RequiresReplace != tc.wantReplace {
				t.Errorf("RequiresReplace = %v, want %v", res.RequiresReplace, tc.wantReplace)
			}
		})
	}
}

// The same account id in another case is the same account. Found by writing
// out an inherited (lower-case) account in upper case, which planned a rebuild.
func TestOnlyADifferentAccountReplacesTheDatabase(t *testing.T) {
	ctx := context.Background()
	sch := instanceResourceSchema(t)
	typ := sch.Type().TerraformType(ctx)
	state := tfsdk.State{Schema: sch, Raw: objectOf(t, typ, existingStateVals(), false)}
	plan := tfsdk.Plan{Schema: sch, Raw: objectOf(t, typ, existingStateVals(), false)}

	for _, tc := range []struct {
		name        string
		to          types.String
		wantReplace bool
	}{
		{"the same, in upper case", types.StringValue(strings.ToUpper(testAccountID)), false},
		{"another account", types.StringValue(testOtherID), true},
		{"KiviDB's cloud", types.StringNull(), true},
		{"not yet known", types.StringUnknown(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := planmodifier.StringRequest{
				Plan: plan, State: state, Config: tfsdk.Config{Schema: sch, Raw: plan.Raw},
				StateValue: types.StringValue(testAccountID), PlanValue: tc.to, ConfigValue: tc.to,
			}
			res := planmodifier.StringResponse{PlanValue: req.PlanValue}
			replaceWhenAccountChanges().PlanModifyString(ctx, req, &res)
			if res.RequiresReplace != tc.wantReplace {
				t.Errorf("RequiresReplace = %v, want %v", res.RequiresReplace, tc.wantReplace)
			}
		})
	}
}

func TestCloudAccountIsComputedAndSnapshotsConflict(t *testing.T) {
	ctx := context.Background()
	sch := instanceResourceSchema(t)
	acct := sch.Attributes["cloud_account_id"].(rschema.StringAttribute)
	if !acct.Optional || !acct.Computed {
		t.Error("cloud_account_id must be optional and computed, to be inherited from a snapshot")
	}
	name := sch.Attributes["cloud_account_name"].(rschema.StringAttribute)
	if !name.Computed || name.Optional || name.Required {
		t.Error("cloud_account_name must be computed only")
	}

	disk := sch.Attributes["restore_from_snapshot_id"].(rschema.StringAttribute)
	typ := sch.Type().TerraformType(ctx)
	cfg := tfsdk.Config{Schema: sch, Raw: objectOf(t, typ, map[string]tftypes.Value{
		"restore_from_snapshot_id":     tftypes.NewValue(tftypes.String, testDiskSnapshotID),
		"restore_from_kdb_snapshot_id": tftypes.NewValue(tftypes.String, testKdbSnapshotID),
	}, false)}
	var all diag.Diagnostics
	for _, v := range disk.Validators {
		req := validator.StringRequest{
			Path: path.Root("restore_from_snapshot_id"), PathExpression: path.MatchRoot("restore_from_snapshot_id"),
			Config: cfg, ConfigValue: types.StringValue(testDiskSnapshotID),
		}
		var res validator.StringResponse
		v.ValidateString(ctx, req, &res)
		all.Append(res.Diagnostics...)
	}
	if !all.HasError() {
		t.Error("a disk snapshot and a KDB snapshot together must be refused at plan time")
	}
	for _, bad := range []string{"nightly", testDiskSnapshotID + "0"} {
		if d := runStringValidators(disk.Validators[:1], bad); !d.HasError() {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

// --- creating, and what the API says when it refuses --------------------------

func TestCreateFromASnapshotSendsItAndReadsTheAccountBack(t *testing.T) {
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/instances":
			_ = json.NewDecoder(r.Body).Decode(&created)
			_, _ = w.Write([]byte(`{"instance_id":"i-1","job_id":"j-1","status":"creating"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/instances/i-1":
			_, _ = w.Write([]byte(strings.Replace(byocInstanceJSON, `"cloud_account_id":`,
				`"cloud_account_name":"production","cloud_account_id":`, 1)))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	r := &instanceResource{client: NewClient(srv.URL, "k")}
	sch := instanceResourceSchema(t)
	vals := createConfigVals()
	vals["restore_from_snapshot_id"] = tftypes.NewValue(tftypes.String, strings.ToUpper(testDiskSnapshotID))
	vals["restore_from_kdb_snapshot_id"] = tftypes.NewValue(tftypes.String, nil)
	vals["cloud_account_id"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	plan := tfsdk.Plan{Schema: sch, Raw: objectOf(t, sch.Type().TerraformType(ctx), vals, true)}

	res := resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &res)
	failOn(t, res.Diagnostics)

	if created["restore_from_snapshot_id"] != testDiskSnapshotID {
		t.Errorf("create did not send the snapshot: %v", created)
	}
	if _, sent := created["cloud_account_id"]; sent {
		t.Errorf("an unresolved account must be left to the API, which uses the snapshot's: %v", created)
	}
	if _, sent := created["restore_from_kdb_snapshot_id"]; sent {
		t.Errorf("an unset KDB snapshot must not be sent: %v", created)
	}

	var got instanceModel
	failOn(t, res.State.Get(ctx, &got))
	if got.CloudAccountID.ValueString() != testAccountID || got.CloudAccountName.ValueString() != "production" {
		t.Errorf("the inherited account must be read back: id=%v name=%v", got.CloudAccountID, got.CloudAccountName)
	}
	// Kept as configured: the API has no record of it to read back.
	if got.RestoreFromSnapshotID.ValueString() != strings.ToUpper(testDiskSnapshotID) {
		t.Errorf("restore_from_snapshot_id must be kept as configured: %v", got.RestoreFromSnapshotID)
	}
}

func TestSnapshotLockRefusalsAreExplained(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{
			name: "account",
			body: `{"error":{"code":"snapshot_account_mismatch","message":"This snapshot lives in production; it can only be restored there.",
				"snapshot_cloud":"aws","snapshot_region":"eu-central-1","snapshot_cloud_account_id":"` + testAccountID + `"}}`,
			want: []string{"This snapshot lives in production", `cloud_account_id = "` + testAccountID + `"`},
		},
		{
			name: "account, snapshot in KiviDB's cloud",
			body: `{"error":{"code":"snapshot_account_mismatch","message":"This snapshot lives in KiviDB's cloud; it can only be restored there.",
				"snapshot_cloud":"aws","snapshot_region":"eu-central-1","snapshot_cloud_account_id":null}}`,
			want: []string{"remove `cloud_account_id`"},
		},
		{
			name: "cloud",
			body: `{"error":{"code":"snapshot_cloud_mismatch","message":"This snapshot is on AWS and the target is on AZURE.",
				"snapshot_cloud":"aws","snapshot_region":"eu-central-1","snapshot_cloud_account_id":null}}`,
			want: []string{"is on AWS", `cloud = "aws"`},
		},
		{
			name: "region",
			body: `{"error":{"code":"snapshot_region_mismatch","message":"This snapshot is in eu-central-1 and the target is in us-east-1.",
				"snapshot_cloud":"aws","snapshot_region":"eu-central-1","snapshot_cloud_account_id":null}}`,
			want: []string{"us-east-1", `region = "eu-central-1"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL, "k").CreateInstance(context.Background(), CreateInstanceRequest{}, "")
			if err == nil {
				t.Fatal("expected a refusal")
			}
			msg := explain(err)
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("explanation does not say %q: %q", want, msg)
				}
			}
		})
	}
}

func TestIdempotencyKeyDiffersBySnapshot(t *testing.T) {
	base := instanceModel{
		Name: types.StringValue("orders"), Cloud: types.StringValue("aws"),
		Region: types.StringValue("eu-central-1"), Tier: types.StringValue("pro"),
		DataSizeGB: types.Int64Value(8), CloudAccountID: types.StringNull(),
		RestoreFromSnapshotID: types.StringNull(), RestoreFromKdbSnapshotID: types.StringNull(),
	}
	plain := idempotencyKeyFor(base)
	if plain != "tf:orders:aws:eu-central-1:pro:8" {
		t.Errorf("the key without a snapshot must not change: %q", plain)
	}
	base.RestoreFromSnapshotID = types.StringValue(testDiskSnapshotID)
	disk := idempotencyKeyFor(base)
	base.RestoreFromSnapshotID = types.StringNull()
	base.RestoreFromKdbSnapshotID = types.StringValue(testDiskSnapshotID)
	kdb := idempotencyKeyFor(base)
	if disk == plain || kdb == plain || disk == kdb {
		t.Errorf("each snapshot is a different create: %q %q %q", plain, disk, kdb)
	}
}

// --- reading the account's name -----------------------------------------------

func TestDiskSnapshotReadsItsAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"snapshot":{"id":"` + testDiskSnapshotID + `","instance_id":"i-1","label":"nightly",
			"cloud":"azure","region":"swedencentral","size_gb":"6.00","status":"ready","created_at":"2026-10-08T00:00:00Z",
			"cloud_account_id":"` + testAccountID + `","cloud_account_name":"emea"}}`))
	}))
	defer srv.Close()
	s, err := NewClient(srv.URL, "k").GetDiskSnapshot(context.Background(), testDiskSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var m diskSnapshotModel
	m.apply(s)
	if m.CloudAccountID.ValueString() != testAccountID || m.CloudAccountName.ValueString() != "emea" {
		t.Errorf("a snapshot's account must be read: %+v", m)
	}

	var hosted diskSnapshotModel
	hosted.apply(&DiskSnapshot{ID: "s-1"})
	if !hosted.CloudAccountID.IsNull() || !hosted.CloudAccountName.IsNull() {
		t.Errorf("a snapshot in KiviDB's cloud has a null account: %+v", hosted)
	}
}

func TestInstanceReadsItsAccountName(t *testing.T) {
	id, name := testAccountID, "production"
	var m instanceModel
	applyInstance(&m, &Instance{ID: "i-1", CloudAccountID: &id, CloudAccountName: &name})
	if m.CloudAccountName.ValueString() != "production" {
		t.Errorf("cloud_account_name = %v", m.CloudAccountName)
	}
	applyInstance(&m, &Instance{ID: "i-1"})
	if !m.CloudAccountName.IsNull() {
		t.Errorf("a database in KiviDB's cloud has no account name: %v", m.CloudAccountName)
	}
}

// --- helpers ------------------------------------------------------------------

func restorePlanServer(t *testing.T, wantPath string, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != wantPath {
			t.Errorf("unexpected %s %s, want GET %s", r.Method, r.URL.Path, wantPath)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// createConfigVals is a Pro database in eu-central-1 on AWS, as configured.
func createConfigVals() map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"name":            tftypes.NewValue(tftypes.String, "orders"),
		"tier":            tftypes.NewValue(tftypes.String, "pro"),
		"cloud":           tftypes.NewValue(tftypes.String, "aws"),
		"region":          tftypes.NewValue(tftypes.String, "eu-central-1"),
		"data_size_gb":    tftypes.NewValue(tftypes.Number, 8),
		"throughput_mode": tftypes.NewValue(tftypes.String, "standard"),
		"replica_count":   tftypes.NewValue(tftypes.Number, 2),
		"aof_enabled":     tftypes.NewValue(tftypes.Bool, true),
		"lua_enabled":     tftypes.NewValue(tftypes.Bool, false),
		"tls_enabled":     tftypes.NewValue(tftypes.Bool, false),
		"tls_only":        tftypes.NewValue(tftypes.Bool, false),
		"wait_for_ready":  tftypes.NewValue(tftypes.Bool, true),
	}
}

// modifyCreatePlan runs ModifyPlan for a create of createConfigVals with
// `extra` on top, the way the framework calls it: after the attribute plan
// modifiers, with an unset cloud_account_id already planned by
// cloudAccountFromSnapshotOrState.
func modifyCreatePlan(t *testing.T, url string, extra map[string]tftypes.Value) resource.ModifyPlanResponse {
	t.Helper()
	ctx := context.Background()
	sch := instanceResourceSchema(t)
	typ := sch.Type().TerraformType(ctx)

	cfg := createConfigVals()
	for k, v := range extra {
		cfg[k] = v
	}
	plan := map[string]tftypes.Value{}
	for k, v := range cfg {
		plan[k] = v
	}
	// Optional, not computed: null when not configured, in the plan as well.
	for _, k := range []string{"restore_from_snapshot_id", "restore_from_kdb_snapshot_id"} {
		if _, ok := plan[k]; !ok {
			plan[k] = tftypes.NewValue(tftypes.String, nil)
		}
	}
	if _, ok := cfg["cloud_account_id"]; !ok {
		_, disk := extra["restore_from_snapshot_id"]
		_, kdb := extra["restore_from_kdb_snapshot_id"]
		if disk || kdb {
			plan["cloud_account_id"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
		} else {
			plan["cloud_account_id"] = tftypes.NewValue(tftypes.String, nil)
		}
	}

	req := resource.ModifyPlanRequest{
		Config: tfsdk.Config{Schema: sch, Raw: objectOf(t, typ, cfg, false)},
		Plan:   tfsdk.Plan{Schema: sch, Raw: objectOf(t, typ, plan, true)},
		State:  tfsdk.State{Schema: sch, Raw: tftypes.NewValue(typ, nil)},
	}
	res := resource.ModifyPlanResponse{Plan: req.Plan}
	r := &instanceResource{client: NewClient(url, "k")}
	r.ModifyPlan(ctx, req, &res)
	return res
}

// existingStateVals is a database in a connected account, as state holds it.
func existingStateVals() map[string]tftypes.Value {
	vals := createConfigVals()
	vals["id"] = tftypes.NewValue(tftypes.String, "i-1")
	vals["cloud_account_id"] = tftypes.NewValue(tftypes.String, testAccountID)
	vals["cloud_account_name"] = tftypes.NewValue(tftypes.String, "production")
	vals["status"] = tftypes.NewValue(tftypes.String, "running")
	return vals
}

func plannedString(t *testing.T, plan tfsdk.Plan, name string) types.String {
	t.Helper()
	var v types.String
	failOn(t, plan.GetAttribute(context.Background(), path.Root(name), &v))
	return v
}
