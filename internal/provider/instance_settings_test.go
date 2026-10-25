package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const testCA = "-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIUQ2xpZW50Q0E=\n-----END CERTIFICATE-----\n"

// instanceJSON is GET /instances/i-1 for a running paid database on 1.1.2,
// with the given fields replaced.
func instanceJSON(t *testing.T, over map[string]any) string {
	t.Helper()
	row := map[string]any{
		"id": "i-1", "name": "orders", "tier": "pro", "cloud": "aws", "region": "eu-central-1",
		"data_size_gb": 8, "throughput_mode": "standard", "replica_count": 2, "status": "running",
		"endpoint": "orders.cloud.kividb.io:6379", "kividb_version": "1.1.2", "aof_enabled": true,
		"lua_enabled": false, "tls_enabled": true, "tls_only": false, "free_tier": false, "org_id": "o-1",
		"shard_count": 1, "replication_tls": false, "client_cert_mode": "off", "client_ca_pem": nil,
	}
	for k, v := range over {
		row[k] = v
	}
	b, err := json.Marshal(map[string]any{"instance": row})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// paidInstanceVals is a Pro database with TLS on, as configured and planned.
func paidInstanceVals() map[string]tftypes.Value {
	vals := createConfigVals()
	vals["tls_enabled"] = boolean(true)
	vals["kividb_version"] = str("1.1.2")
	vals["free_tier"] = boolean(false)
	vals["shard_count"] = num(1)
	vals["replication_tls"] = boolean(false)
	vals["client_cert_mode"] = str("off")
	vals["client_ca_pem"] = nullStr()
	vals["cloud_account_id"] = nullStr()
	return vals
}

func instanceStateVals() map[string]tftypes.Value {
	vals := paidInstanceVals()
	vals["id"] = str("i-1")
	vals["status"] = str("running")
	return vals
}

func TestCreateSendsFreeTierAndReadsItBack(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("POST /instances", 202, `{"instance_id":"i-1","job_id":"j-1","status":"creating"}`)
	f.answer("GET /instances/i-1", 200, instanceJSON(t, map[string]any{
		"tier": "essentials", "data_size_gb": 1, "replica_count": 0, "aof_enabled": false,
		"tls_enabled": false, "free_tier": true,
	}))

	vals := paidInstanceVals()
	vals["tier"], vals["data_size_gb"], vals["replica_count"] = str("essentials"), num(1), num(0)
	vals["free_tier"], vals["tls_enabled"], vals["aof_enabled"] = boolean(true), boolean(false), boolean(false)
	res := doCreate(t, &instanceResource{client: f.client()}, vals)
	failOn(t, res.Diagnostics)

	body := f.sent("POST /instances")[0].Body
	if body["free_tier"] != true {
		t.Errorf("free_tier was not sent: %v", body)
	}
	if _, sent := body["shard_count"]; sent {
		t.Errorf("a single-shard database must not send shard_count: %v", body)
	}
	var got instanceModel
	failOn(t, res.State.Get(context.Background(), &got))
	if !got.FreeTier.ValueBool() || got.AofEnabled.ValueBool() || got.ShardCount.ValueInt64() != 1 {
		t.Errorf("state = free_tier %v aof %v shards %v", got.FreeTier, got.AofEnabled, got.ShardCount)
	}
}

func TestCreateSendsShardCount(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("POST /instances", 202, `{"instance_id":"i-1"}`)
	f.answer("GET /instances/i-1", 200, instanceJSON(t, map[string]any{"shard_count": 3, "tls_enabled": false}))
	vals := paidInstanceVals()
	vals["shard_count"], vals["tls_enabled"] = num(3), boolean(false)
	res := doCreate(t, &instanceResource{client: f.client()}, vals)
	failOn(t, res.Diagnostics)
	if body := f.sent("POST /instances")[0].Body; body["shard_count"] != float64(3) {
		t.Errorf("shard_count was not sent: %v", body)
	}
	if _, sent := f.sent("POST /instances")[0].Body["free_tier"]; sent {
		t.Error("free_tier must not be sent for a paid database")
	}
}

// The create endpoint takes neither encrypted replication nor client
// certificates, so a new database that asks for them gets them from a binary
// change once it is running -- and the apply waits for that to land.
func TestCreateAppliesNodeSecurityOnceRunning(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("POST /instances", 202, `{"instance_id":"i-1"}`)
	applied := false
	f.on("GET /instances/i-1", func(apiCall) apiReply {
		if applied {
			return apiReply{200, instanceJSON(t, map[string]any{"replication_tls": true,
				"client_cert_mode": "optional", "client_ca_pem": testCA})}
		}
		return apiReply{200, instanceJSON(t, nil)}
	})
	f.on("POST /instances/i-1/binary", func(apiCall) apiReply {
		applied = true
		return apiReply{202, `{"job_id":"j-2"}`}
	})

	vals := paidInstanceVals()
	vals["replication_tls"], vals["client_cert_mode"], vals["client_ca_pem"] = boolean(true), str("optional"), str(testCA)
	vals["wait_for_ready"] = boolean(false) // waited for anyway: the change needs a running database
	res := doCreate(t, &instanceResource{client: f.client()}, vals)
	failOn(t, res.Diagnostics)

	sent := f.sent("POST /instances/i-1/binary")
	if len(sent) != 1 {
		t.Fatalf("expected one binary change, got %d", len(sent))
	}
	b := sent[0].Body
	if b["kividb_version"] != "1.1.2" || b["replication_tls"] != true || b["client_cert_mode"] != "optional" || b["client_ca_pem"] != testCA {
		t.Errorf("binary body = %v", b)
	}
	for _, k := range []string{"lua_enabled", "tls_enabled"} {
		if _, has := b[k]; has {
			t.Errorf("%s did not change and must not be sent: %v", k, b)
		}
	}
	if body := f.sent("POST /instances")[0].Body; body["replication_tls"] != nil || body["client_cert_mode"] != nil {
		t.Errorf("the create endpoint does not take these: %v", body)
	}
	var got instanceModel
	failOn(t, res.State.Get(context.Background(), &got))
	if !got.ReplicationTLS.ValueBool() || got.ClientCertMode.ValueString() != "optional" {
		t.Errorf("state = %v %v", got.ReplicationTLS, got.ClientCertMode)
	}
}

func TestCreateWithoutNodeSecurityMakesNoBinaryChange(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("POST /instances", 202, `{"instance_id":"i-1"}`)
	f.answer("GET /instances/i-1", 200, instanceJSON(t, nil))
	failOn(t, doCreate(t, &instanceResource{client: f.client()}, paidInstanceVals()).Diagnostics)
	// No route for /binary is registered: the fake fails the test if it is called.
}

func TestUpdateSendsClientCertificatesAndKeepsTheConfiguredPEM(t *testing.T) {
	f := newFakeAPI(t)
	applied := false
	f.on("GET /instances/i-1", func(apiCall) apiReply {
		if applied {
			return apiReply{200, instanceJSON(t, map[string]any{"client_cert_mode": "optional", "client_ca_pem": testCA})}
		}
		return apiReply{200, instanceJSON(t, nil)}
	})
	f.on("POST /instances/i-1/binary", func(apiCall) apiReply {
		applied = true
		return apiReply{202, `{"job_id":"j-2"}`}
	})

	// The same certificate as a Windows file holds it.
	configured := strings.ReplaceAll(testCA, "\n", "\r\n") + "\r\n"
	plan := instanceStateVals()
	delete(plan, "status")
	plan["client_cert_mode"], plan["client_ca_pem"] = str("optional"), str(configured)
	res := doUpdate(t, &instanceResource{client: f.client()}, instanceStateVals(), plan)
	failOn(t, res.Diagnostics)

	sent := f.sent("POST /instances/i-1/binary")
	if len(sent) != 1 {
		t.Fatalf("expected one binary change, got %+v", sent)
	}
	b := sent[0].Body
	if b["client_cert_mode"] != "optional" || b["client_ca_pem"] != configured || b["kividb_version"] != "1.1.2" {
		t.Errorf("binary body = %v", b)
	}
	if _, has := b["replication_tls"]; has {
		t.Errorf("replication_tls did not change and must not be sent: %v", b)
	}
	var got instanceModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.ClientCaPem.ValueString() != configured {
		t.Errorf("the configured spelling of the same CA must be kept: %q", got.ClientCaPem.ValueString())
	}
}

func TestUpdateRefusalIsSurfaced(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET /instances/i-1", 200, instanceJSON(t, nil))
	f.answer("POST /instances/i-1/binary", 400,
		`{"error":{"code":"client_ca_invalid","message":"Certificate 1 (CN=leaf) is not a CA certificate."}}`)
	plan := instanceStateVals()
	plan["client_cert_mode"], plan["client_ca_pem"] = str("optional"), str(testCA)
	res := doUpdate(t, &instanceResource{client: f.client()}, instanceStateVals(), plan)
	wantError(t, res.Diagnostics, "Certificate 1 (CN=leaf) is not a CA certificate.")
}

func TestReadDecodesTheSettings(t *testing.T) {
	var m instanceModel
	applyInstance(&m, mustInstance(t, instanceJSON(t, map[string]any{
		"shard_count": 4, "replication_tls": true, "client_cert_mode": "required", "client_ca_pem": testCA,
	})))
	if m.ShardCount.ValueInt64() != 4 || !m.ReplicationTLS.ValueBool() || m.ClientCertMode.ValueString() != "required" ||
		m.ClientCaPem.ValueString() != testCA {
		t.Errorf("decoded %v %v %v %v", m.ShardCount, m.ReplicationTLS, m.ClientCertMode, m.ClientCaPem)
	}

	// An older API without the columns reads as their defaults.
	var old instanceModel
	applyInstance(&old, mustInstance(t, `{"instance":{"id":"i-1","status":"running"}}`))
	if old.ShardCount.ValueInt64() != 1 || old.ClientCertMode.ValueString() != "off" || !old.ClientCaPem.IsNull() {
		t.Errorf("defaults: %v %v %v", old.ShardCount, old.ClientCertMode, old.ClientCaPem)
	}
}

func mustInstance(t *testing.T, body string) *Instance {
	t.Helper()
	var w struct {
		Instance Instance `json:"instance"`
	}
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatal(err)
	}
	return &w.Instance
}

func TestInstanceConfigValidation(t *testing.T) {
	free := func() map[string]tftypes.Value {
		return map[string]tftypes.Value{
			"tier": str("essentials"), "cloud": str("aws"), "region": str("eu-central-1"),
			"data_size_gb": num(1), "free_tier": boolean(true),
		}
	}
	paid := func() map[string]tftypes.Value {
		return map[string]tftypes.Value{
			"tier": str("pro"), "cloud": str("aws"), "region": str("eu-central-1"), "data_size_gb": num(8),
			"tls_enabled": boolean(true),
		}
	}
	with := func(base map[string]tftypes.Value, kv ...any) map[string]tftypes.Value {
		for i := 0; i < len(kv); i += 2 {
			base[kv[i].(string)] = kv[i+1].(tftypes.Value)
		}
		return base
	}

	for _, tc := range []struct {
		name     string
		cfg      map[string]tftypes.Value
		wantPath string // empty: valid
	}{
		{"free tier as the API makes it", free(), ""},
		{"free tier of 8 GB", with(free(), "data_size_gb", num(8)), "data_size_gb"},
		{"free tier on Pro", with(free(), "tier", str("pro")), "tier"},
		{"free tier on Azure", with(free(), "cloud", str("azure")), "cloud"},
		{"free tier with replicas", with(free(), "replica_count", num(1)), "replica_count"},
		{"free tier with TLS", with(free(), "tls_enabled", boolean(true)), "tls_enabled"},
		{"free tier with AOF", with(free(), "aof_enabled", boolean(true)), "aof_enabled"},
		{"free tier sharded", with(free(), "shard_count", num(2)), "shard_count"},
		{"paid with AOF off", with(paid(), "aof_enabled", boolean(false)), "aof_enabled"},
		{"paid with AOF on", with(paid(), "aof_enabled", boolean(true)), ""},
		{"encrypted replication", with(paid(), "replication_tls", boolean(true)), ""},
		{"encrypted replication without TLS", with(paid(), "replication_tls", boolean(true), "tls_enabled", boolean(false)), "replication_tls"},
		{"encrypted replication sharded", with(paid(), "replication_tls", boolean(true), "shard_count", num(3)), "replication_tls"},
		{"optional client certs", with(paid(), "client_cert_mode", str("optional"), "client_ca_pem", str(testCA)), ""},
		{"client certs without a CA", with(paid(), "client_cert_mode", str("optional")), "client_ca_pem"},
		{"client certs without TLS", with(paid(), "client_cert_mode", str("optional"), "client_ca_pem", str(testCA), "tls_enabled", boolean(false)), "client_cert_mode"},
		{"required without tls_only", with(paid(), "client_cert_mode", str("required"), "client_ca_pem", str(testCA)), "client_cert_mode"},
		{"required with tls_only", with(paid(), "client_cert_mode", str("required"), "client_ca_pem", str(testCA), "tls_only", boolean(true)), ""},
		{"a CA with certs off", with(paid(), "client_ca_pem", str(testCA)), "client_ca_pem"},
		// Unknown at plan time is checked by the API at apply instead.
		{"unknown TLS", with(paid(), "replication_tls", boolean(true), "tls_enabled", tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue)), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sch := instanceResourceSchema(t)
			req := resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: sch,
				Raw: objectOf(t, sch.Type().TerraformType(ctx), tc.cfg, false)}}
			var res resource.ValidateConfigResponse
			(&instanceResource{}).ValidateConfig(ctx, req, &res)
			if tc.wantPath == "" {
				failOn(t, res.Diagnostics)
				return
			}
			if !res.Diagnostics.HasError() {
				t.Fatalf("expected an error at %s", tc.wantPath)
			}
			found := false
			for _, e := range res.Diagnostics.Errors() {
				if p, ok := e.(interface{ Path() path.Path }); ok && p.Path().Equal(path.Root(tc.wantPath)) {
					found = true
				}
			}
			if !found {
				t.Errorf("no error at %s: %v", tc.wantPath, res.Diagnostics.Errors())
			}
		})
	}
}

// aof_enabled follows the tier when it is not configured, so neither a free
// database nor a paid one plans a value its read-back contradicts.
func TestAofFollowsTheTier(t *testing.T) {
	ctx := context.Background()
	sch := instanceResourceSchema(t)
	for _, tc := range []struct {
		free tftypes.Value
		want types.Bool
	}{
		{boolean(true), types.BoolValue(false)},
		{boolean(false), types.BoolValue(true)},
		{tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue), types.BoolUnknown()},
	} {
		vals := paidInstanceVals()
		vals["free_tier"] = tc.free
		plan := tfsdk.Plan{Schema: sch, Raw: planned(t, sch, vals)}
		req := planmodifier.BoolRequest{ConfigValue: types.BoolNull(), PlanValue: types.BoolUnknown(), Plan: plan}
		res := planmodifier.BoolResponse{PlanValue: req.PlanValue}
		aofFollowsTier().PlanModifyBool(ctx, req, &res)
		failOn(t, res.Diagnostics)
		if !res.PlanValue.Equal(tc.want) {
			t.Errorf("free_tier=%v: planned %v, want %v", tc.free, res.PlanValue, tc.want)
		}
	}
}

func TestClientCAFollowsTheMode(t *testing.T) {
	ctx := context.Background()
	sch := instanceResourceSchema(t)
	for _, tc := range []struct {
		mode tftypes.Value
		want types.String
	}{
		{str("off"), types.StringNull()},
		{str("optional"), types.StringValue(testCA)},
		{tftypes.NewValue(tftypes.String, tftypes.UnknownValue), types.StringUnknown()},
	} {
		vals := paidInstanceVals()
		vals["client_cert_mode"] = tc.mode
		plan := tfsdk.Plan{Schema: sch, Raw: planned(t, sch, vals)}
		req := planmodifier.StringRequest{ConfigValue: types.StringNull(), PlanValue: types.StringUnknown(),
			StateValue: types.StringValue(testCA), Plan: plan}
		res := planmodifier.StringResponse{PlanValue: req.PlanValue}
		caFollowsMode().PlanModifyString(ctx, req, &res)
		failOn(t, res.Diagnostics)
		if !res.PlanValue.Equal(tc.want) {
			t.Errorf("mode=%v: planned %v, want %v", tc.mode, res.PlanValue, tc.want)
		}
	}
}

func TestSamePEMIgnoresLayoutOnly(t *testing.T) {
	other := strings.Replace(testCA, "Q2xpZW50Q0E=", "T3RoZXJDQQ==", 1)
	if !samePEM(testCA, strings.ReplaceAll(testCA, "\n", "\r\n")+"  \n") {
		t.Error("line endings and trailing space are layout")
	}
	if samePEM(testCA, other) {
		t.Error("a different certificate is not the same PEM")
	}
	if samePEM(testCA, testCA+other) {
		t.Error("a bundle with another certificate is not the same PEM")
	}
}
