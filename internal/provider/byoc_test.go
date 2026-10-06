package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	testAccountID = "6f1d3a52-1b7e-4c1e-9a55-0c2f1e0b8d11"
	testOtherID   = "0a9b8c7d-6e5f-4a3b-9c2d-1e0f9a8b7c6d"
)

// What the API returns for a Pro database with two replicas in a customer's
// own AWS account.
const byocInstanceJSON = `{"instance":{"id":"i-1","name":"orders","tier":"pro","cloud":"aws",
	"region":"eu-central-1","data_size_gb":8,"throughput_mode":"standard","replica_count":2,
	"status":"running","endpoint":"orders.cloud.kividb.io:6379","public_endpoint":null,
	"kividb_version":"1.0.4","aof_enabled":true,"lua_enabled":false,"tls_enabled":false,
	"tls_only":false,"org_id":"o-1","cloud_account_id":"` + testAccountID + `",
	"private_endpoint":"orders.private.cloud.kividb.io",
	"private_readonly_endpoint":"orders-ro.private.cloud.kividb.io",
	"private_replica_endpoints":["orders-r1.private.cloud.kividb.io","orders-r2.private.cloud.kividb.io"],
	"billing_model":"byoc_management"}}`

const cloudAccountsJSON = `{"cloud_accounts":[
	{"id":"` + testAccountID + `","cloud":"aws","display_name":"production","aws_account_id":"123456789012",
	 "enabled_regions":["eu-central-1","us-east-1"],"status":"verified","aws_role_arn":"arn:aws:iam::123456789012:role/x"},
	{"id":"` + testOtherID + `","cloud":"aws","display_name":"staging","aws_account_id":"210987654321",
	 "enabled_regions":["eu-central-1"],"status":"pending"},
	{"id":"11111111-2222-4333-8444-555555555555","cloud":"aws","display_name":"shared","aws_account_id":"111111111111",
	 "enabled_regions":[],"status":"verified"},
	{"id":"66666666-7777-4888-8999-aaaaaaaaaaaa","cloud":"aws","display_name":"shared","aws_account_id":"222222222222",
	 "enabled_regions":[],"status":"verified"}],
	"trusted_principal_arns":["arn:aws:iam::999999999999:role/kividb"]}`

// --- creating a database in a connected account -----------------------------

func TestCreateSendsTheCloudAccountAndReadsPrivateEndpoints(t *testing.T) {
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/instances":
			_ = json.NewDecoder(r.Body).Decode(&created)
			_, _ = w.Write([]byte(`{"instance_id":"i-1","job_id":"j-1","status":"creating"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/instances/i-1":
			_, _ = w.Write([]byte(byocInstanceJSON))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	r := &instanceResource{client: NewClient(srv.URL, "k")}
	sch := instanceResourceSchema(t)

	plan := tfsdk.Plan{Schema: sch, Raw: objectOf(t, sch.Type().TerraformType(ctx), map[string]tftypes.Value{
		"name":             tftypes.NewValue(tftypes.String, "orders"),
		"tier":             tftypes.NewValue(tftypes.String, "pro"),
		"cloud":            tftypes.NewValue(tftypes.String, "aws"),
		"region":           tftypes.NewValue(tftypes.String, "eu-central-1"),
		"data_size_gb":     tftypes.NewValue(tftypes.Number, 8),
		"throughput_mode":  tftypes.NewValue(tftypes.String, "standard"),
		"replica_count":    tftypes.NewValue(tftypes.Number, 2),
		"aof_enabled":      tftypes.NewValue(tftypes.Bool, true),
		"lua_enabled":      tftypes.NewValue(tftypes.Bool, false),
		"tls_enabled":      tftypes.NewValue(tftypes.Bool, false),
		"tls_only":         tftypes.NewValue(tftypes.Bool, false),
		"wait_for_ready":   tftypes.NewValue(tftypes.Bool, true),
		"cloud_account_id": tftypes.NewValue(tftypes.String, testAccountID),
	}, true)}

	res := resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &res)
	failOn(t, res.Diagnostics)

	if created["cloud_account_id"] != testAccountID {
		t.Errorf("create did not send the cloud account: %v", created)
	}

	var got instanceModel
	failOn(t, res.State.Get(ctx, &got))
	if got.CloudAccountID.ValueString() != testAccountID {
		t.Errorf("cloud_account_id = %v", got.CloudAccountID)
	}
	if got.PrivateEndpoint.ValueString() != "orders.private.cloud.kividb.io" {
		t.Errorf("private_endpoint = %v", got.PrivateEndpoint)
	}
	if got.PrivateReadonlyEndpoint.ValueString() != "orders-ro.private.cloud.kividb.io" {
		t.Errorf("private_readonly_endpoint = %v", got.PrivateReadonlyEndpoint)
	}
	var replicas []string
	failOn(t, got.PrivateReplicaEndpoints.ElementsAs(ctx, &replicas, false))
	if strings.Join(replicas, ",") != "orders-r1.private.cloud.kividb.io,orders-r2.private.cloud.kividb.io" {
		t.Errorf("private_replica_endpoints = %v", replicas)
	}
}

// A database in KiviDB's own cloud: no account is sent, the private endpoints
// are null and the replica list is empty rather than null, so the attribute
// reads the same after create, refresh and import.
func TestHostedDatabaseHasNoPrivateEndpoints(t *testing.T) {
	var m instanceModel
	applyInstance(&m, &Instance{ID: "i-1", Tier: "essentials", Cloud: "aws", Region: "us-east-1"})
	if !m.CloudAccountID.IsNull() || !m.PrivateEndpoint.IsNull() || !m.PrivateReadonlyEndpoint.IsNull() {
		t.Errorf("hosted database must have null BYOC attributes: %+v", m)
	}
	if m.PrivateReplicaEndpoints.IsNull() || len(m.PrivateReplicaEndpoints.Elements()) != 0 {
		t.Errorf("private_replica_endpoints must be an empty list, got %v", m.PrivateReplicaEndpoints)
	}

	body, _ := json.Marshal(CreateInstanceRequest{Tier: "essentials", Cloud: "aws", Region: "us-east-1"})
	if strings.Contains(string(body), "cloud_account_id") {
		t.Errorf("an unset account must not be sent: %s", body)
	}
}

// The API answers in lower case. An id configured in upper case must not plan
// a replacement on every refresh.
func TestCloudAccountIDKeepsTheConfiguredSpelling(t *testing.T) {
	m := instanceModel{CloudAccountID: types.StringValue(strings.ToUpper(testAccountID))}
	id := testAccountID
	applyInstance(&m, &Instance{ID: "i-1", CloudAccountID: &id})
	if m.CloudAccountID.ValueString() != strings.ToUpper(testAccountID) {
		t.Errorf("spelling not kept: %v", m.CloudAccountID)
	}

	// A genuinely different account is drift and must show as such.
	other := testOtherID
	applyInstance(&m, &Instance{ID: "i-1", CloudAccountID: &other})
	if m.CloudAccountID.ValueString() != testOtherID {
		t.Errorf("a different account must replace the value: %v", m.CloudAccountID)
	}
}

func TestCloudAccountIDIsValidatedAndForcesReplacement(t *testing.T) {
	sch := instanceResourceSchema(t)
	raw := sch.Attributes["cloud_account_id"]
	a, ok := raw.(rschema.StringAttribute)
	if !ok {
		t.Fatalf("cloud_account_id is not a string attribute: %T", raw)
	}
	for _, good := range []string{testAccountID, strings.ToUpper(testAccountID)} {
		if d := runStringValidators(a.Validators, good); d.HasError() {
			t.Errorf("%q must be accepted: %v", good, d)
		}
	}
	for _, bad := range []string{"production", "123456789012", testAccountID + "x", ""} {
		if d := runStringValidators(a.Validators, bad); !d.HasError() {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if len(a.PlanModifiers) == 0 {
		t.Error("cloud_account_id must replace the database when changed")
	}
}

func TestIdempotencyKeyDiffersByAccount(t *testing.T) {
	base := instanceModel{
		Name: types.StringValue("orders"), Cloud: types.StringValue("aws"),
		Region: types.StringValue("eu-central-1"), Tier: types.StringValue("pro"),
		DataSizeGB: types.Int64Value(8), CloudAccountID: types.StringNull(),
	}
	hosted := idempotencyKeyFor(base)
	if hosted != "tf:orders:aws:eu-central-1:pro:8" {
		t.Errorf("the key for a hosted database must not change: %q", hosted)
	}
	base.CloudAccountID = types.StringValue(testAccountID)
	if idempotencyKeyFor(base) == hosted {
		t.Error("the same database in a different account is a different create")
	}
}

// --- planning the derived endpoints ------------------------------------------

// The private endpoints are derived from name, tier and replica count. They are
// kept from state when none of those change, and become unknown when one does.
func TestPrivateEndpointsFollowTheirInputs(t *testing.T) {
	ctx := context.Background()
	sch := instanceResourceSchema(t)
	typ := sch.Type().TerraformType(ctx)

	stateVals := map[string]tftypes.Value{
		"id":                        tftypes.NewValue(tftypes.String, "i-1"),
		"name":                      tftypes.NewValue(tftypes.String, "orders"),
		"tier":                      tftypes.NewValue(tftypes.String, "pro"),
		"replica_count":             tftypes.NewValue(tftypes.Number, 2),
		"data_size_gb":              tftypes.NewValue(tftypes.Number, 8),
		"cloud_account_id":          tftypes.NewValue(tftypes.String, testAccountID),
		"private_endpoint":          tftypes.NewValue(tftypes.String, "orders.private.cloud.kividb.io"),
		"private_readonly_endpoint": tftypes.NewValue(tftypes.String, "orders-ro.private.cloud.kividb.io"),
		"private_replica_endpoints": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{
			tftypes.NewValue(tftypes.String, "orders-r1.private.cloud.kividb.io"),
			tftypes.NewValue(tftypes.String, "orders-r2.private.cloud.kividb.io"),
		}),
	}
	state := tfsdk.State{Schema: sch, Raw: objectOf(t, typ, stateVals, false)}

	planWith := func(changes map[string]tftypes.Value) tfsdk.Plan {
		vals := map[string]tftypes.Value{}
		for k, v := range stateVals {
			vals[k] = v
		}
		for k, v := range changes {
			vals[k] = v
		}
		for _, k := range []string{"private_endpoint", "private_readonly_endpoint", "private_replica_endpoints"} {
			delete(vals, k) // computed: unknown in a plan until a modifier says otherwise
		}
		return tfsdk.Plan{Schema: sch, Raw: objectOf(t, typ, vals, true)}
	}

	endpoint := func(plan tfsdk.Plan) types.String {
		m := useStateUnlessChanged("name", "cloud_account_id")
		req := planmodifier.StringRequest{
			Plan: plan, State: state, StateValue: types.StringValue("orders.private.cloud.kividb.io"),
			PlanValue: types.StringUnknown(), ConfigValue: types.StringNull(),
		}
		res := planmodifier.StringResponse{PlanValue: req.PlanValue}
		m.PlanModifyString(ctx, req, &res)
		return res.PlanValue
	}
	replicas := func(plan tfsdk.Plan) types.List {
		m := useStateUnlessChanged("name", "cloud_account_id", "tier", "replica_count")
		var sv types.List
		failOn(t, state.GetAttribute(ctx, path.Root("private_replica_endpoints"), &sv))
		req := planmodifier.ListRequest{
			Plan: plan, State: state, StateValue: sv,
			PlanValue: types.ListUnknown(types.StringType), ConfigValue: types.ListNull(types.StringType),
		}
		res := planmodifier.ListResponse{PlanValue: req.PlanValue}
		m.PlanModifyList(ctx, req, &res)
		return res.PlanValue
	}

	// A resize touches none of the inputs: both are carried over.
	resize := planWith(map[string]tftypes.Value{"data_size_gb": tftypes.NewValue(tftypes.Number, 16)})
	if v := endpoint(resize); v.IsUnknown() || v.ValueString() != "orders.private.cloud.kividb.io" {
		t.Errorf("a resize must not make private_endpoint unknown: %v", v)
	}
	if v := replicas(resize); v.IsUnknown() || len(v.Elements()) != 2 {
		t.Errorf("a resize must not make private_replica_endpoints unknown: %v", v)
	}

	// A rename moves every hostname.
	rename := planWith(map[string]tftypes.Value{"name": tftypes.NewValue(tftypes.String, "billing")})
	if v := endpoint(rename); !v.IsUnknown() {
		t.Errorf("a rename must make private_endpoint unknown, got %v", v)
	}

	// A replica scale changes the list but not the primary's hostname.
	scale := planWith(map[string]tftypes.Value{"replica_count": tftypes.NewValue(tftypes.Number, 3)})
	if v := endpoint(scale); v.IsUnknown() {
		t.Error("a replica scale must not make private_endpoint unknown")
	}
	if v := replicas(scale); !v.IsUnknown() {
		t.Errorf("a replica scale must make private_replica_endpoints unknown, got %v", v)
	}

	// An unknown name (e.g. from another resource) may be a change.
	unknownName := planWith(map[string]tftypes.Value{"name": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})
	if v := endpoint(unknownName); !v.IsUnknown() {
		t.Error("an unknown name must leave private_endpoint unknown")
	}
}

// --- the cloud account data sources ------------------------------------------

func TestCloudAccountLookup(t *testing.T) {
	srv := cloudAccountsServer(t, http.StatusOK, cloudAccountsJSON)
	defer srv.Close()
	ctx := context.Background()

	for _, tc := range []struct {
		name, byID, byName string
		wantID, wantErr    string
		wantWarning        bool
	}{
		{name: "by name", byName: "production", wantID: testAccountID},
		{name: "by id", byID: testAccountID, wantID: testAccountID},
		{name: "by id, upper case", byID: strings.ToUpper(testAccountID), wantID: testAccountID},
		{name: "pending account warns", byName: "staging", wantID: testOtherID, wantWarning: true},
		{name: "unknown name", byName: "prod", wantErr: `no connected cloud account is named "prod"`},
		{name: "unknown id", byID: "00000000-0000-4000-8000-000000000000", wantErr: "no connected cloud account has id"},
		{name: "ambiguous name", byName: "shared", wantErr: `2 connected cloud accounts are named "shared"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &cloudAccountDataSource{client: NewClient(srv.URL, "k")}
			sch := dataSourceSchema(t, d)
			vals := map[string]tftypes.Value{}
			if tc.byID != "" {
				vals["id"] = tftypes.NewValue(tftypes.String, tc.byID)
			}
			if tc.byName != "" {
				vals["name"] = tftypes.NewValue(tftypes.String, tc.byName)
			}
			cfg := tfsdk.Config{Schema: sch, Raw: objectOf(t, sch.Type().TerraformType(ctx), vals, false)}
			res := datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
			d.Read(ctx, datasource.ReadRequest{Config: cfg}, &res)

			if tc.wantErr != "" {
				if !res.Diagnostics.HasError() {
					t.Fatalf("expected an error containing %q", tc.wantErr)
				}
				if msg := res.Diagnostics.Errors()[0].Detail(); !strings.Contains(msg, tc.wantErr) {
					t.Errorf("error %q does not say %q", msg, tc.wantErr)
				}
				if tc.byName == "shared" {
					msg := res.Diagnostics.Errors()[0].Detail()
					if !strings.Contains(msg, "11111111-") || !strings.Contains(msg, "66666666-") {
						t.Errorf("an ambiguous name must list both ids: %q", msg)
					}
				}
				return
			}
			failOn(t, res.Diagnostics)
			if got := res.Diagnostics.WarningsCount() > 0; got != tc.wantWarning {
				t.Errorf("warning = %v, want %v", got, tc.wantWarning)
			}
			var got cloudAccountModel
			failOn(t, res.State.Get(ctx, &got))
			if want := tc.wantID; tc.byID != "" && got.ID.ValueString() != tc.byID {
				t.Errorf("id = %v, want the configured %s", got.ID, tc.byID)
			} else if !strings.EqualFold(got.ID.ValueString(), want) {
				t.Errorf("id = %v, want %s", got.ID, tc.wantID)
			}
			if tc.wantID == testAccountID {
				var regions []string
				failOn(t, got.Regions.ElementsAs(ctx, &regions, false))
				if got.Name.ValueString() != "production" || got.Cloud.ValueString() != "aws" ||
					got.AWSAccountID.ValueString() != "123456789012" || got.Status.ValueString() != "verified" ||
					strings.Join(regions, ",") != "eu-central-1,us-east-1" {
					t.Errorf("fields not read: %+v regions=%v", got, regions)
				}
			}
		})
	}
}

func TestCloudAccountReadsAGoogleCloudProject(t *testing.T) {
	srv := cloudAccountsServer(t, http.StatusOK, `{"cloud_accounts":[
		{"id":"`+testAccountID+`","cloud":"gcp","display_name":"analytics","aws_account_id":null,
		 "gcp_project_id":"acme-prod-123","enabled_regions":["us-east1"],"status":"verified"}]}`)
	defer srv.Close()
	ctx := context.Background()

	d := &cloudAccountDataSource{client: NewClient(srv.URL, "k")}
	sch := dataSourceSchema(t, d)
	cfg := tfsdk.Config{Schema: sch, Raw: objectOf(t, sch.Type().TerraformType(ctx),
		map[string]tftypes.Value{"name": tftypes.NewValue(tftypes.String, "analytics")}, false)}
	res := datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
	d.Read(ctx, datasource.ReadRequest{Config: cfg}, &res)
	failOn(t, res.Diagnostics)

	var got cloudAccountModel
	failOn(t, res.State.Get(ctx, &got))
	if got.Cloud.ValueString() != "gcp" || got.GCPProjectID.ValueString() != "acme-prod-123" {
		t.Errorf("a Google Cloud project must read its cloud and project id: %+v", got)
	}
	if !got.AWSAccountID.IsNull() {
		t.Errorf("a Google Cloud project has no AWS account id, got %v", got.AWSAccountID)
	}
}

func TestCloudAccountRequiresExactlyOneOfIDAndName(t *testing.T) {
	ctx := context.Background()
	d := &cloudAccountDataSource{}
	sch := dataSourceSchema(t, d)
	for name, vals := range map[string]map[string]tftypes.Value{
		"neither": {},
		"both": {
			"id":   tftypes.NewValue(tftypes.String, testAccountID),
			"name": tftypes.NewValue(tftypes.String, "production"),
		},
	} {
		cfg := tfsdk.Config{Schema: sch, Raw: objectOf(t, sch.Type().TerraformType(ctx), vals, false)}
		var all diag.Diagnostics
		for _, v := range d.ConfigValidators(ctx) {
			var res datasource.ValidateConfigResponse
			v.ValidateDataSource(ctx, datasource.ValidateConfigRequest{Config: cfg}, &res)
			all.Append(res.Diagnostics...)
		}
		if !all.HasError() {
			t.Errorf("%s: must be refused at plan time", name)
		}
	}
}

func TestCloudAccountsListsEveryAccount(t *testing.T) {
	srv := cloudAccountsServer(t, http.StatusOK, cloudAccountsJSON)
	defer srv.Close()
	ctx := context.Background()

	d := &cloudAccountsDataSource{client: NewClient(srv.URL, "k")}
	sch := dataSourceSchema(t, d)
	res := datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
	d.Read(ctx, datasource.ReadRequest{}, &res)
	failOn(t, res.Diagnostics)

	var got cloudAccountsModel
	failOn(t, res.State.Get(ctx, &got))
	if len(got.CloudAccounts) != 4 || got.CloudAccounts[0].Name.ValueString() != "production" {
		t.Errorf("accounts not listed in order: %+v", got.CloudAccounts)
	}
	if got.CloudAccounts[2].Regions.IsNull() {
		t.Error("an account with no regions must have an empty list, not null")
	}
}

// When the feature is not enabled the whole route answers 404. That must not
// read as "account not found".
func TestCloudAccountsNotEnabledIsExplained(t *testing.T) {
	srv := cloudAccountsServer(t, http.StatusNotFound, `{"error":{"code":"not_found","message":"Not found"}}`)
	defer srv.Close()
	_, err := NewClient(srv.URL, "k").ListCloudAccounts(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if msg := explainCloudAccounts(err); !strings.Contains(msg, "not enabled") {
		t.Errorf("a 404 must say the feature is not enabled: %q", msg)
	}
}

func TestInstanceDataSourceReadsPrivateEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(byocInstanceJSON))
	}))
	defer srv.Close()
	ctx := context.Background()

	d := &instanceDataSource{client: NewClient(srv.URL, "k")}
	sch := dataSourceSchema(t, d)
	cfg := tfsdk.Config{Schema: sch, Raw: objectOf(t, sch.Type().TerraformType(ctx),
		map[string]tftypes.Value{"id": tftypes.NewValue(tftypes.String, "i-1")}, false)}
	res := datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
	d.Read(ctx, datasource.ReadRequest{Config: cfg}, &res)
	failOn(t, res.Diagnostics)

	var got instanceDataSourceModel
	failOn(t, res.State.Get(ctx, &got))
	if got.CloudAccountID.ValueString() != testAccountID ||
		got.PrivateEndpoint.ValueString() != "orders.private.cloud.kividb.io" ||
		got.PrivateReadonlyEndpoint.ValueString() != "orders-ro.private.cloud.kividb.io" ||
		len(got.PrivateReplicaEndpoints.Elements()) != 2 {
		t.Errorf("private endpoints not read: %+v", got)
	}
}

// --- helpers ------------------------------------------------------------------

func cloudAccountsServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/cloud-accounts" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func instanceResourceSchema(t *testing.T) rschema.Schema {
	t.Helper()
	var res resource.SchemaResponse
	NewInstanceResource().Schema(context.Background(), resource.SchemaRequest{}, &res)
	failOn(t, res.Diagnostics)
	return res.Schema
}

func dataSourceSchema(t *testing.T, d datasource.DataSource) dschema.Schema {
	t.Helper()
	var res datasource.SchemaResponse
	d.Schema(context.Background(), datasource.SchemaRequest{}, &res)
	failOn(t, res.Diagnostics)
	return res.Schema
}

// objectOf builds an object of the schema's type from the given attributes.
// The rest are null, or unknown when `unknownRest` is set -- which is what a
// plan holds for computed attributes nothing has decided yet.
func objectOf(t *testing.T, typ tftypes.Type, vals map[string]tftypes.Value, unknownRest bool) tftypes.Value {
	t.Helper()
	obj, ok := typ.(tftypes.Object)
	if !ok {
		t.Fatalf("schema type is %T, not an object", typ)
	}
	all := map[string]tftypes.Value{}
	for name, at := range obj.AttributeTypes {
		if v, ok := vals[name]; ok {
			all[name] = v
		} else if unknownRest {
			all[name] = tftypes.NewValue(at, tftypes.UnknownValue)
		} else {
			all[name] = tftypes.NewValue(at, nil)
		}
	}
	for name := range vals {
		if _, ok := obj.AttributeTypes[name]; !ok {
			t.Fatalf("schema has no attribute %q", name)
		}
	}
	return tftypes.NewValue(typ, all)
}

func failOn(t *testing.T, d diag.Diagnostics) {
	t.Helper()
	if d.HasError() {
		t.Fatalf("unexpected diagnostics: %v", d.Errors())
	}
}
