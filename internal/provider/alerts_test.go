package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
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
	testInstanceID = "790df3ae-c064-45f5-b924-9ed89471fd72"
	testSlackURL   = "https://hooks.slack.com/services/T000/B000/XXXXXXXXXXXXXXXX"
	testPDKey      = "abcdefghijklmnopqrstuvwxyz012345"
)

// --- a fake of routes/alerts.ts and routes/audit-log.ts ----------------------
//
// It does what the real handlers do with a body -- the per-metric defaults,
// the failover overrides, dedupe, the org checks and the refusals -- so a test
// that passes here is a test of the request the API would actually see, not of
// a stub that accepts anything.

type fakeAlertsAPI struct {
	mu       sync.Mutex
	t        *testing.T
	seq      int
	channels []map[string]any
	rules    []map[string]any
	bodies   map[string]map[string]any // last body per "METHOD path"
	audit    map[string]any
}

func newFakeAlertsAPI(t *testing.T) (*fakeAlertsAPI, *Client) {
	f := &fakeAlertsAPI{t: t, bodies: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewClient(srv.URL, "kvdb_test")
}

func (f *fakeAlertsAPI) nextID() string {
	f.seq++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq)
}

func refuse(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func (f *fakeAlertsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.bodies[r.Method+" "+r.URL.Path] = body
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/alerts/channels":
		_ = json.NewEncoder(w).Encode(map[string]any{"channels": f.channels, "signature_header": "X-KiviDB-Signature"})

	case r.Method == http.MethodPost && r.URL.Path == "/alerts/channels":
		ch := map[string]any{"id": f.nextID(), "name": body["name"], "kind": body["kind"], "created_at": "2026-10-25T10:00:00.000Z"}
		out := map[string]any{}
		switch body["kind"] {
		case "email":
			roles := body["roles"]
			if roles == nil {
				roles = []any{"owner", "admin"}
			}
			ch["roles"] = roles
		case "pagerduty":
			key, _ := body["routing_key"].(string)
			region := body["region"]
			if region == nil {
				region = "us"
			}
			ch["region"] = region
			ch["key_hint"] = "••••" + key[len(key)-4:]
		default:
			u, _ := body["url"].(string)
			if strings.Contains(u, "10.0.0.") {
				refuse(w, 422, "url_not_allowed", "That address is private; alerts are delivered only to public addresses.")
				return
			}
			ch["url_hint"] = maskURL(u)
			if body["kind"] == "webhook" {
				out["signing_secret"] = "whsec_test_secret"
				out["signature_header"] = "X-KiviDB-Signature"
			}
		}
		f.channels = append(f.channels, ch)
		out["channel"] = ch
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodDelete && len(parts) == 3 && parts[1] == "channels":
		for _, rule := range f.rules {
			for _, c := range rule["channel_ids"].([]string) {
				if c == parts[2] {
					refuse(w, 409, "channel_in_use", fmt.Sprintf("Used by \"%s\". Remove it from those alerts first.", rule["name"]))
					return
				}
			}
		}
		for i, c := range f.channels {
			if c["id"] == parts[2] {
				f.channels = append(f.channels[:i], f.channels[i+1:]...)
				w.WriteHeader(204)
				return
			}
		}
		refuse(w, 404, "not_found", "")

	case r.Method == http.MethodGet && r.URL.Path == "/alerts/rules":
		_ = json.NewEncoder(w).Encode(map[string]any{"rules": f.rules})

	case (r.Method == http.MethodPost && r.URL.Path == "/alerts/rules") ||
		(r.Method == http.MethodPut && len(parts) == 3 && parts[1] == "rules"):
		rule, status, code, msg := f.normalize(body)
		if status != 0 {
			refuse(w, status, code, msg)
			return
		}
		if r.Method == http.MethodPost {
			rule["id"] = f.nextID()
			f.rules = append(f.rules, rule)
			w.WriteHeader(201)
		} else {
			found := false
			for i, x := range f.rules {
				if x["id"] == parts[2] {
					rule["id"] = parts[2]
					f.rules[i] = rule
					found = true
				}
			}
			if !found {
				refuse(w, 404, "not_found", "")
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"rule": rule})

	case r.Method == http.MethodDelete && len(parts) == 3 && parts[1] == "rules":
		for i, x := range f.rules {
			if x["id"] == parts[2] {
				f.rules = append(f.rules[:i], f.rules[i+1:]...)
				w.WriteHeader(204)
				return
			}
		}
		refuse(w, 404, "not_found", "")

	case r.Method == http.MethodGet && r.URL.Path == "/audit-log/settings":
		_ = json.NewEncoder(w).Encode(f.audit)

	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}
}

// normalize is normalizeRule from routes/alerts.ts.
func (f *fakeAlertsAPI) normalize(b map[string]any) (map[string]any, int, string, string) {
	metric, _ := b["metric"].(string)
	m, ok := alertMetrics[metric]
	if !ok {
		return nil, 400, "bad_request", "metric"
	}
	var instanceID any
	if !m.organization {
		instanceID = b["instance_id"]
		if instanceID == nil {
			return nil, 400, "instance_required", m.label + " alerts are about one database; choose it."
		}
		if instanceID != testInstanceID {
			return nil, 404, "instance_not_found", "No such database in this organization."
		}
	}
	seen := map[string]bool{}
	channels := []string{}
	for _, c := range b["channel_ids"].([]any) {
		s := c.(string)
		if !seen[s] {
			seen[s] = true
			channels = append(channels, s)
		}
	}
	for _, c := range channels {
		exists := false
		for _, ch := range f.channels {
			if ch["id"] == c {
				exists = true
			}
		}
		if !exists {
			return nil, 404, "channel_not_found", "One of the chosen channels does not exist in this organization."
		}
	}
	failover := metric == "failover"
	threshold := m.defaultThreshold
	if t, ok := b["threshold"].(float64); ok {
		threshold = t
	}
	if failover {
		threshold = 0
	} else if threshold < 0 {
		return nil, 400, "bad_threshold", "The threshold cannot be negative."
	}
	op := m.defaultOp
	if o, ok := b["op"].(string); ok && !failover {
		op = o
	}
	forMinutes := b["for_minutes"]
	if failover {
		forMinutes = 0
	}
	return map[string]any{
		"name": b["name"], "metric": metric, "instance_id": instanceID, "op": op, "threshold": threshold,
		"for_minutes": forMinutes, "cooldown_minutes": b["cooldown_minutes"], "channel_ids": channels,
		"enabled": b["enabled"],
	}, 0, "", ""
}

// --- helpers ---------------------------------------------------------------

func resourceSchema(t *testing.T, r resource.Resource) rschema.Schema {
	t.Helper()
	var res resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &res)
	failOn(t, res.Diagnostics)
	return res.Schema
}

// planOf is what Terraform hands Create: the given values, and every other
// attribute null if it is configurable or unknown if only the server sets it.
func planOf(t *testing.T, sch rschema.Schema, vals map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	typ := sch.Type().TerraformType(context.Background()).(tftypes.Object)
	all := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		if v, ok := vals[name]; ok {
			all[name] = v
			continue
		}
		a := sch.Attributes[name]
		if a.IsComputed() && !a.IsOptional() {
			all[name] = tftypes.NewValue(at, tftypes.UnknownValue)
		} else {
			all[name] = tftypes.NewValue(at, nil)
		}
	}
	for name := range vals {
		if _, ok := typ.AttributeTypes[name]; !ok {
			t.Fatalf("schema has no attribute %q", name)
		}
	}
	return tftypes.NewValue(typ, all)
}

func tfStr(s string) tftypes.Value        { return tftypes.NewValue(tftypes.String, s) }
func tfNum(n float64) tftypes.Value       { return tftypes.NewValue(tftypes.Number, n) }
func tfBool(b bool) tftypes.Value         { return tftypes.NewValue(tftypes.Bool, b) }
func tfNull(t tftypes.Type) tftypes.Value { return tftypes.NewValue(t, nil) }
func tfStrSet(vs ...string) tftypes.Value {
	elems := make([]tftypes.Value, 0, len(vs))
	for _, v := range vs {
		elems = append(elems, tfStr(v))
	}
	return tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, elems)
}

func emptyState(sch rschema.Schema) tfsdk.State {
	return tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(context.Background()), nil)}
}

func create(t *testing.T, r resource.Resource, sch rschema.Schema, vals map[string]tftypes.Value) resource.CreateResponse {
	t.Helper()
	res := resource.CreateResponse{State: emptyState(sch)}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Schema: sch, Raw: planOf(t, sch, vals)}}, &res)
	return res
}

func read(t *testing.T, r resource.Resource, state tfsdk.State) resource.ReadResponse {
	t.Helper()
	res := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &res)
	return res
}

// importAndRead is `terraform import`: the id alone, then a read.
func importAndRead(t *testing.T, r resource.ResourceWithImportState, sch rschema.Schema, id string) resource.ReadResponse {
	t.Helper()
	ctx := context.Background()
	typ := sch.Type().TerraformType(ctx).(tftypes.Object)
	nulls := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		nulls[name] = tftypes.NewValue(at, nil)
	}
	ires := resource.ImportStateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(typ, nulls)}}
	r.ImportState(ctx, resource.ImportStateRequest{ID: id}, &ires)
	failOn(t, ires.Diagnostics)
	return read(t, r, ires.State)
}

func validateResource(t *testing.T, r resource.ResourceWithValidateConfig, sch rschema.Schema, vals map[string]tftypes.Value) []string {
	t.Helper()
	cfg := tfsdk.Config{Schema: sch, Raw: planOf(t, sch, vals)}
	// A config holds null, not unknown, for attributes nobody wrote.
	typ := sch.Type().TerraformType(context.Background()).(tftypes.Object)
	all := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		if v, ok := vals[name]; ok {
			all[name] = v
		} else {
			all[name] = tftypes.NewValue(at, nil)
		}
	}
	cfg.Raw = tftypes.NewValue(typ, all)
	var res resource.ValidateConfigResponse
	r.ValidateConfig(context.Background(), resource.ValidateConfigRequest{Config: cfg}, &res)
	var out []string
	for _, d := range res.Diagnostics.Errors() {
		out = append(out, d.Summary()+": "+d.Detail())
	}
	return out
}

// --- kividb_alert_channel ---------------------------------------------------

func TestAlertChannelWebhookCreateKeepsTheSecretShownOnce(t *testing.T) {
	f, c := newFakeAlertsAPI(t)
	r := &alertChannelResource{client: c}
	sch := resourceSchema(t, r)

	res := create(t, r, sch, map[string]tftypes.Value{
		"name": tfStr("ops hook"), "kind": tfStr("webhook"), "url": tfStr("https://example.com/hooks/kividb/alerts"),
		"roles": tfNull(tftypes.Set{ElementType: tftypes.String}), "region": tfNull(tftypes.String),
	})
	failOn(t, res.Diagnostics)

	sent := f.bodies["POST /alerts/channels"]
	if sent["url"] != "https://example.com/hooks/kividb/alerts" || sent["kind"] != "webhook" {
		t.Errorf("create body wrong: %v", sent)
	}
	for _, k := range []string{"roles", "routing_key", "region"} {
		if _, present := sent[k]; present {
			t.Errorf("%s does not belong to a webhook and must not be sent: %v", k, sent)
		}
	}

	var got alertChannelModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.SigningSecret.ValueString() != "whsec_test_secret" {
		t.Errorf("the signing secret is shown once and must be kept: %v", got.SigningSecret)
	}
	if got.URL.ValueString() != "https://example.com/hooks/kividb/alerts" {
		t.Errorf("url lost: %v", got.URL)
	}
	if got.URLHint.ValueString() != "https://example.com/hooks/kivid…" {
		t.Errorf("url_hint = %q", got.URLHint.ValueString())
	}
	if !got.Roles.IsNull() || !got.Region.IsNull() || !got.KeyHint.IsNull() {
		t.Errorf("attributes of other kinds must be null: %+v", got)
	}

	// A refresh keeps what the API never returns.
	rr := read(t, r, res.State)
	failOn(t, rr.Diagnostics)
	var after alertChannelModel
	failOn(t, rr.State.Get(context.Background(), &after))
	if after.SigningSecret.ValueString() != "whsec_test_secret" || after.URL.ValueString() != got.URL.ValueString() {
		t.Errorf("a read lost the secret or the url: %+v", after)
	}
}

func TestAlertChannelEmailAndPagerDutyDefaults(t *testing.T) {
	f, c := newFakeAlertsAPI(t)
	r := &alertChannelResource{client: c}
	sch := resourceSchema(t, r)
	ctx := context.Background()

	res := create(t, r, sch, map[string]tftypes.Value{
		"name": tfStr("on-call"), "kind": tfStr("email"), "roles": tfStrSet("owner", "admin"),
		"region": tfNull(tftypes.String),
	})
	failOn(t, res.Diagnostics)
	var email alertChannelModel
	failOn(t, res.State.Get(ctx, &email))
	var roles []string
	failOn(t, email.Roles.ElementsAs(ctx, &roles, false))
	sort.Strings(roles)
	if strings.Join(roles, ",") != "admin,owner" {
		t.Errorf("roles = %v", roles)
	}
	if !email.URL.IsNull() || !email.SigningSecret.IsNull() {
		t.Errorf("an email channel has no url or secret: %+v", email)
	}

	res = create(t, r, sch, map[string]tftypes.Value{
		"name": tfStr("pager"), "kind": tfStr("pagerduty"), "routing_key": tfStr(testPDKey),
		"region": tfStr("eu"), "roles": tfNull(tftypes.Set{ElementType: tftypes.String}),
	})
	failOn(t, res.Diagnostics)
	if sent := f.bodies["POST /alerts/channels"]; sent["routing_key"] != testPDKey || sent["region"] != "eu" {
		t.Errorf("pagerduty body wrong: %v", sent)
	}
	var pd alertChannelModel
	failOn(t, res.State.Get(ctx, &pd))
	if pd.KeyHint.ValueString() != "••••2345" || pd.Region.ValueString() != "eu" || pd.RoutingKey.ValueString() != testPDKey {
		t.Errorf("pagerduty state wrong: %+v", pd)
	}
	if !pd.SigningSecret.IsNull() {
		t.Error("a PagerDuty key is the customer's own; there is no signing secret")
	}
}

func TestAlertChannelDefaultModifiers(t *testing.T) {
	ctx := context.Background()
	sch := resourceSchema(t, &alertChannelResource{})
	cfgFor := func(kind string) tfsdk.Config {
		return tfsdk.Config{Schema: sch, Raw: planOf(t, sch, map[string]tftypes.Value{
			"kind": tfStr(kind), "name": tfStr("x"),
			"roles": tfNull(tftypes.Set{ElementType: tftypes.String}), "region": tfNull(tftypes.String),
		})}
	}
	plan := func(kind string) tfsdk.Plan {
		return tfsdk.Plan{Schema: sch, Raw: cfgFor(kind).Raw}
	}

	for kind, want := range map[string]string{"email": "admin,owner", "slack": ""} {
		req := planmodifier.SetRequest{Config: cfgFor(kind), Plan: plan(kind), ConfigValue: types.SetNull(types.StringType),
			PlanValue: types.SetUnknown(types.StringType), State: emptyState(sch)}
		res := planmodifier.SetResponse{PlanValue: req.PlanValue}
		defaultEmailRolesModifier{}.PlanModifySet(ctx, req, &res)
		var got []string
		if !res.PlanValue.IsNull() {
			failOn(t, res.PlanValue.ElementsAs(ctx, &got, false))
		}
		sort.Strings(got)
		if strings.Join(got, ",") != want {
			t.Errorf("%s: roles planned %v, want %q", kind, got, want)
		}
	}
	for kind, want := range map[string]string{"pagerduty": "us", "email": ""} {
		req := planmodifier.StringRequest{Config: cfgFor(kind), Plan: plan(kind), ConfigValue: types.StringNull(),
			PlanValue: types.StringUnknown(), State: emptyState(sch)}
		res := planmodifier.StringResponse{PlanValue: req.PlanValue}
		defaultPagerDutyRegionModifier{}.PlanModifyString(ctx, req, &res)
		if res.PlanValue.ValueString() != want || (want == "" && !res.PlanValue.IsNull()) {
			t.Errorf("%s: region planned %v, want %q", kind, res.PlanValue, want)
		}
	}
}

func TestAlertChannelGoneIsRemovedFromState(t *testing.T) {
	f, c := newFakeAlertsAPI(t)
	r := &alertChannelResource{client: c}
	sch := resourceSchema(t, r)
	res := create(t, r, sch, map[string]tftypes.Value{
		"name": tfStr("s"), "kind": tfStr("slack"), "url": tfStr(testSlackURL),
		"roles": tfNull(tftypes.Set{ElementType: tftypes.String}), "region": tfNull(tftypes.String),
	})
	failOn(t, res.Diagnostics)

	f.channels = nil // deleted in the console
	rr := read(t, r, res.State)
	failOn(t, rr.Diagnostics)
	if !rr.State.Raw.IsNull() {
		t.Error("a channel that is no longer listed must be removed from state, not fail the plan")
	}
}

// After an import the URL is null in state. Writing it into configuration
// must not replace a channel that already holds it -- and a different URL must.
func TestAlertChannelImportThenFillInTheURL(t *testing.T) {
	f, c := newFakeAlertsAPI(t)
	f.channels = []map[string]any{{"id": "00000000-0000-4000-8000-0000000000aa", "name": "s", "kind": "slack",
		"url_hint": maskURL(testSlackURL), "created_at": "2026-10-01T00:00:00Z"}}
	r := &alertChannelResource{client: c}
	sch := resourceSchema(t, r)
	ctx := context.Background()

	rr := importAndRead(t, r, sch, "00000000-0000-4000-8000-0000000000aa")
	failOn(t, rr.Diagnostics)
	var imported alertChannelModel
	failOn(t, rr.State.Get(ctx, &imported))
	if imported.Kind.ValueString() != "slack" || imported.Name.ValueString() != "s" || !imported.URL.IsNull() {
		t.Fatalf("import read wrong: %+v", imported)
	}

	check := func(planned string) bool {
		req := planmodifier.StringRequest{
			Path: path.Root("url"), State: rr.State, Plan: tfsdk.Plan{Schema: sch, Raw: rr.State.Raw},
			StateValue: types.StringNull(), PlanValue: types.StringValue(planned), ConfigValue: types.StringValue(planned),
		}
		res := planmodifier.StringResponse{PlanValue: req.PlanValue}
		secretReplaceModifier{hint: path.Root("url_hint"), hintOf: maskURL}.PlanModifyString(ctx, req, &res)
		failOn(t, res.Diagnostics)
		return res.RequiresReplace
	}
	if check(testSlackURL) {
		t.Error("the URL the channel already holds must be accepted in place after an import")
	}
	if !check("https://hooks.slack.com/services/T999/B999/YYYYYYYYYYYYYYYY") {
		t.Error("a different URL must replace the channel")
	}

	// Outside an import, any change replaces.
	full := planmodifier.StringRequest{
		Path: path.Root("url"), State: rr.State, Plan: tfsdk.Plan{Schema: sch, Raw: rr.State.Raw},
		StateValue: types.StringValue(testSlackURL), PlanValue: types.StringValue(testSlackURL + "x"),
	}
	res := planmodifier.StringResponse{PlanValue: full.PlanValue}
	secretReplaceModifier{hint: path.Root("url_hint"), hintOf: maskURL}.PlanModifyString(ctx, full, &res)
	if !res.RequiresReplace {
		t.Error("changing a known URL must replace the channel: there is no update endpoint")
	}
}

func TestAlertChannelInUseExplainsCreateBeforeDestroy(t *testing.T) {
	f, c := newFakeAlertsAPI(t)
	r := &alertChannelResource{client: c}
	sch := resourceSchema(t, r)
	res := create(t, r, sch, map[string]tftypes.Value{
		"name": tfStr("s"), "kind": tfStr("slack"), "url": tfStr(testSlackURL),
		"roles": tfNull(tftypes.Set{ElementType: tftypes.String}), "region": tfNull(tftypes.String),
	})
	failOn(t, res.Diagnostics)
	var ch alertChannelModel
	failOn(t, res.State.Get(context.Background(), &ch))
	f.rules = []map[string]any{{"id": "r", "name": "memory", "channel_ids": []string{ch.ID.ValueString()}}}

	dres := resource.DeleteResponse{State: res.State}
	r.Delete(context.Background(), resource.DeleteRequest{State: res.State}, &dres)
	if !dres.Diagnostics.HasError() {
		t.Fatal("deleting a channel in use must fail")
	}
	detail := dres.Diagnostics.Errors()[0].Detail()
	if !strings.Contains(detail, `Used by "memory"`) || !strings.Contains(detail, "create_before_destroy") {
		t.Errorf("the refusal must keep the server's words and say how to fix it: %q", detail)
	}

	// Already gone is not an error.
	f.rules, f.channels = nil, nil
	dres = resource.DeleteResponse{State: res.State}
	r.Delete(context.Background(), resource.DeleteRequest{State: res.State}, &dres)
	failOn(t, dres.Diagnostics)
}

func TestAlertChannelRefusalKeepsTheServersWords(t *testing.T) {
	_, c := newFakeAlertsAPI(t)
	r := &alertChannelResource{client: c}
	sch := resourceSchema(t, r)
	res := create(t, r, sch, map[string]tftypes.Value{
		"name": tfStr("h"), "kind": tfStr("webhook"), "url": tfStr("https://10.0.0.5/hook"),
		"roles": tfNull(tftypes.Set{ElementType: tftypes.String}), "region": tfNull(tftypes.String),
	})
	if !res.Diagnostics.HasError() {
		t.Fatal("expected the refusal")
	}
	if d := res.Diagnostics.Errors()[0].Detail(); !strings.Contains(d, "alerts are delivered only to public addresses") {
		t.Errorf("refusal not surfaced verbatim: %q", d)
	}
}

func TestAlertChannelConfigValidation(t *testing.T) {
	r := &alertChannelResource{}
	sch := resourceSchema(t, r)
	set := tftypes.Set{ElementType: tftypes.String}
	cases := []struct {
		name string
		vals map[string]tftypes.Value
		want string // empty: valid
	}{
		{"email", map[string]tftypes.Value{"kind": tfStr("email"), "name": tfStr("a"), "roles": tfStrSet("member")}, ""},
		{"email with url", map[string]tftypes.Value{"kind": tfStr("email"), "name": tfStr("a"), "url": tfStr(testSlackURL)}, "url"},
		{"slack", map[string]tftypes.Value{"kind": tfStr("slack"), "name": tfStr("a"), "url": tfStr(testSlackURL)}, ""},
		{"slack wrong host", map[string]tftypes.Value{"kind": tfStr("slack"), "name": tfStr("a"), "url": tfStr("https://example.com/services/abcdefghijk")}, "Slack incoming-webhook"},
		{"slack no url", map[string]tftypes.Value{"kind": tfStr("slack"), "name": tfStr("a")}, "needs `url`"},
		{"slack with roles", map[string]tftypes.Value{"kind": tfStr("slack"), "name": tfStr("a"), "url": tfStr(testSlackURL), "roles": tfStrSet("owner")}, "roles"},
		{"teams workflows", map[string]tftypes.Value{"kind": tfStr("teams"), "name": tfStr("a"), "url": tfStr("https://prod-01.westeurope.logic.azure.com/workflows/x")}, ""},
		{"teams office", map[string]tftypes.Value{"kind": tfStr("teams"), "name": tfStr("a"), "url": tfStr("https://acme.webhook.office.com/webhookb2/x")}, ""},
		{"teams other host", map[string]tftypes.Value{"kind": tfStr("teams"), "name": tfStr("a"), "url": tfStr("https://evil.example.com/logic.azure.com")}, "Teams Workflows"},
		{"teams odd port", map[string]tftypes.Value{"kind": tfStr("teams"), "name": tfStr("a"), "url": tfStr("https://x.logic.azure.com:8443/w")}, "Teams Workflows"},
		{"webhook http", map[string]tftypes.Value{"kind": tfStr("webhook"), "name": tfStr("a"), "url": tfStr("http://example.com/h")}, "https"},
		{"webhook too long", map[string]tftypes.Value{"kind": tfStr("webhook"), "name": tfStr("a"), "url": tfStr("https://example.com/" + strings.Repeat("a", 2040))}, "2048"},
		{"pagerduty", map[string]tftypes.Value{"kind": tfStr("pagerduty"), "name": tfStr("a"), "routing_key": tfStr(testPDKey)}, ""},
		{"pagerduty short key", map[string]tftypes.Value{"kind": tfStr("pagerduty"), "name": tfStr("a"), "routing_key": tfStr("abc")}, "Integration Key"},
		{"pagerduty no key", map[string]tftypes.Value{"kind": tfStr("pagerduty"), "name": tfStr("a")}, "routing_key"},
		{"pagerduty with url", map[string]tftypes.Value{"kind": tfStr("pagerduty"), "name": tfStr("a"), "routing_key": tfStr(testPDKey), "url": tfStr(testSlackURL)}, "url"},
		{"unknown url waits", map[string]tftypes.Value{"kind": tfStr("webhook"), "name": tfStr("a"), "url": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}, ""},
	}
	_ = set
	for _, tc := range cases {
		errs := validateResource(t, r, sch, tc.vals)
		joined := strings.Join(errs, "\n")
		if tc.want == "" && len(errs) > 0 {
			t.Errorf("%s: must be valid, got %s", tc.name, joined)
		}
		if tc.want != "" && !strings.Contains(joined, tc.want) {
			t.Errorf("%s: want an error mentioning %q, got %q", tc.name, tc.want, joined)
		}
	}
}

func TestAlertChannelAttributeValidators(t *testing.T) {
	sch := resourceSchema(t, &alertChannelResource{})
	name := sch.Attributes["name"].(rschema.StringAttribute)
	for _, good := range []string{"a", "On-call team", strings.Repeat("x", 80)} {
		if d := runStringValidators(name.Validators, good); d.HasError() {
			t.Errorf("name %q must be accepted: %v", good, d)
		}
	}
	for _, bad := range []string{"", " padded", "padded ", strings.Repeat("x", 81), "   "} {
		if d := runStringValidators(name.Validators, bad); !d.HasError() {
			t.Errorf("name %q must be rejected", bad)
		}
	}
	kind := sch.Attributes["kind"].(rschema.StringAttribute)
	for _, bad := range []string{"sms", "Email", ""} {
		if d := runStringValidators(kind.Validators, bad); !d.HasError() {
			t.Errorf("kind %q must be rejected", bad)
		}
	}
	for _, a := range []string{"url", "routing_key", "signing_secret"} {
		if !sch.Attributes[a].IsSensitive() {
			t.Errorf("%s must be sensitive", a)
		}
	}
	roles := sch.Attributes["roles"].(rschema.SetAttribute)
	runSet := func(vs ...string) bool {
		req := validator.SetRequest{ConfigValue: stringSet(vs), Path: path.Root("roles")}
		bad := false
		for _, v := range roles.Validators {
			var res validator.SetResponse
			v.ValidateSet(context.Background(), req, &res)
			bad = bad || res.Diagnostics.HasError()
		}
		return bad
	}
	if runSet("owner", "member") || !runSet("viewer") || !runSet() {
		t.Error("roles must be 1 to 3 of owner, admin, member")
	}
}

func TestMaskURLMatchesTheAPI(t *testing.T) {
	for in, want := range map[string]string{
		testSlackURL:                       "https://hooks.slack.com/services/T0…",
		"https://Example.com:443/a":        "https://example.com/a",
		"https://example.com:8443/abc":     "https://example.com:8443/abc",
		"https://example.com":              "https://example.com/",
		"https://example.com/123456789012": "https://example.com/12345678901…",
		"not a url":                        "",
	} {
		if got := maskURL(in); got != want {
			t.Errorf("maskURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- kividb_alert_rule ------------------------------------------------------

func newRuleFixture(t *testing.T) (*fakeAlertsAPI, *alertRuleResource, rschema.Schema, string) {
	f, c := newFakeAlertsAPI(t)
	chID := "00000000-0000-4000-8000-0000000000c1"
	f.channels = []map[string]any{{"id": chID, "name": "s", "kind": "email", "roles": []string{"owner"}}}
	r := &alertRuleResource{client: c}
	return f, r, resourceSchema(t, r), chID
}

func TestAlertRuleCreateReadUpdateDelete(t *testing.T) {
	f, r, sch, chID := newRuleFixture(t)
	ctx := context.Background()

	res := create(t, r, sch, map[string]tftypes.Value{
		"name": tfStr("memory high"), "metric": tfStr("memory_pct"), "instance_id": tfStr("11111111-2222-4333-8444-555555555555"),
		"op": tfStr("gt"), "threshold": tfNum(80), "for_minutes": tfNum(5), "cooldown_minutes": tfNum(60),
		"channel_ids": tfStrSet(chID), "enabled": tfBool(true),
	})
	if !res.Diagnostics.HasError() || !strings.Contains(res.Diagnostics.Errors()[0].Detail(), "No such database in this organization.") {
		t.Fatalf("an unknown database must be refused in the server's words: %v", res.Diagnostics)
	}

	// Written in upper case: sent in lower case, which is how the API matches
	// ids, and kept in state as written.
	res = create(t, r, sch, map[string]tftypes.Value{
		"name": tfStr("memory high"), "metric": tfStr("memory_pct"), "instance_id": tfStr(strings.ToUpper(testInstanceID)),
		"op": tfStr("gt"), "threshold": tfNum(90), "for_minutes": tfNum(10), "cooldown_minutes": tfNum(30),
		"channel_ids": tfStrSet(strings.ToUpper(chID)), "enabled": tfBool(true),
	})
	failOn(t, res.Diagnostics)
	sent := f.bodies["POST /alerts/rules"]
	if sent["threshold"] != float64(90) || sent["for_minutes"] != float64(10) || sent["instance_id"] != testInstanceID {
		t.Errorf("create body wrong: %v", sent)
	}
	var got alertRuleModel
	failOn(t, res.State.Get(ctx, &got))
	if got.ID.IsNull() || got.Threshold.ValueFloat64() != 90 || got.CooldownMinutes.ValueInt64() != 30 {
		t.Errorf("state wrong: %+v", got)
	}
	if got.InstanceID.ValueString() != strings.ToUpper(testInstanceID) || !got.ChannelIDs.Equal(stringSet([]string{strings.ToUpper(chID)})) {
		t.Errorf("ids must keep the spelling they were written in: %+v", got)
	}

	// Update is a PUT of the whole rule, under the same id.
	plan := planOf(t, sch, map[string]tftypes.Value{
		"id": tfStr(got.ID.ValueString()), "name": tfStr("memory very high"), "metric": tfStr("memory_pct"),
		"instance_id": tfStr(testInstanceID), "op": tfStr("gt"), "threshold": tfNum(95), "for_minutes": tfNum(10),
		"cooldown_minutes": tfNum(30), "channel_ids": tfStrSet(chID), "enabled": tfBool(false),
	})
	ures := resource.UpdateResponse{State: res.State}
	r.Update(ctx, resource.UpdateRequest{Plan: tfsdk.Plan{Schema: sch, Raw: plan}, State: res.State}, &ures)
	failOn(t, ures.Diagnostics)
	put := f.bodies["PUT /alerts/rules/"+got.ID.ValueString()]
	if put == nil || put["name"] != "memory very high" || put["enabled"] != false || put["threshold"] != float64(95) {
		t.Errorf("update must PUT the whole rule: %v", put)
	}
	var updated alertRuleModel
	failOn(t, ures.State.Get(ctx, &updated))
	if updated.ID.ValueString() != got.ID.ValueString() || updated.Enabled.ValueBool() {
		t.Errorf("updated state wrong: %+v", updated)
	}

	// Read after the database was deleted (its alerts go with it).
	rr := read(t, r, ures.State)
	failOn(t, rr.Diagnostics)
	if rr.State.Raw.IsNull() {
		t.Fatal("a rule that exists must still be in state")
	}
	f.rules = nil
	rr = read(t, r, ures.State)
	failOn(t, rr.Diagnostics)
	if !rr.State.Raw.IsNull() {
		t.Error("a rule that is no longer listed must be removed from state")
	}

	dres := resource.DeleteResponse{State: ures.State}
	r.Delete(ctx, resource.DeleteRequest{State: ures.State}, &dres)
	failOn(t, dres.Diagnostics) // already gone is not an error
}

func TestAlertRuleFailoverAndCreditRunwayTakeTheServersShape(t *testing.T) {
	f, r, sch, chID := newRuleFixture(t)
	ctx := context.Background()

	res := create(t, r, sch, map[string]tftypes.Value{
		"name": tfStr("failover"), "metric": tfStr("failover"), "instance_id": tfStr(testInstanceID),
		"op": tfStr("gt"), "threshold": tfNum(0), "for_minutes": tfNum(0), "cooldown_minutes": tfNum(60),
		"channel_ids": tfStrSet(chID), "enabled": tfBool(true),
	})
	failOn(t, res.Diagnostics)
	sent := f.bodies["POST /alerts/rules"]
	if _, present := sent["threshold"]; present {
		t.Errorf("a failover alert has no threshold to send: %v", sent)
	}
	var fo alertRuleModel
	failOn(t, res.State.Get(ctx, &fo))
	if fo.Op.ValueString() != "gt" || fo.Threshold.ValueFloat64() != 0 || fo.ForMinutes.ValueInt64() != 0 {
		t.Errorf("failover state wrong: %+v", fo)
	}

	res = create(t, r, sch, map[string]tftypes.Value{
		"name": tfStr("runway"), "metric": tfStr("credit_runway_days"),
		"op": tfStr("lt"), "threshold": tfNum(7), "for_minutes": tfNum(5), "cooldown_minutes": tfNum(1440),
		"channel_ids": tfStrSet(chID), "enabled": tfBool(true),
	})
	failOn(t, res.Diagnostics)
	if _, present := f.bodies["POST /alerts/rules"]["instance_id"]; present {
		t.Errorf("credit runway is about the organization; no instance_id may be sent")
	}
	var cr alertRuleModel
	failOn(t, res.State.Get(ctx, &cr))
	if !cr.InstanceID.IsNull() || cr.Op.ValueString() != "lt" {
		t.Errorf("credit runway state wrong: %+v", cr)
	}
}

func TestAlertRuleImport(t *testing.T) {
	f, r, sch, chID := newRuleFixture(t)
	f.rules = []map[string]any{{"id": "00000000-0000-4000-8000-0000000000r1", "name": "latency", "metric": "latency_p99_ms",
		"instance_id": testInstanceID, "instance_name": "cache", "op": "gt", "threshold": 12.5, "for_minutes": 3,
		"cooldown_minutes": 120, "channel_ids": []string{chID}, "enabled": true, "status": "firing", "last_value": 14.2}}
	rr := importAndRead(t, r, sch, "00000000-0000-4000-8000-0000000000r1")
	failOn(t, rr.Diagnostics)
	var got alertRuleModel
	failOn(t, rr.State.Get(context.Background(), &got))
	if got.Name.ValueString() != "latency" || got.Threshold.ValueFloat64() != 12.5 || got.ForMinutes.ValueInt64() != 3 ||
		got.CooldownMinutes.ValueInt64() != 120 || got.InstanceID.ValueString() != testInstanceID || len(got.ChannelIDs.Elements()) != 1 {
		t.Errorf("import read wrong: %+v", got)
	}
}

// The API answers in lower case; an id written in upper case is the same id
// and must not plan a change on every refresh.
func TestAlertRuleKeepsTheConfiguredIDSpelling(t *testing.T) {
	upper := strings.ToUpper(testInstanceID)
	ch := "00000000-0000-4000-8000-0000000000c1"
	m := alertRuleModel{InstanceID: types.StringValue(upper), ChannelIDs: stringSet([]string{strings.ToUpper(ch)})}
	id := testInstanceID
	m.apply(&AlertRule{ID: "r", InstanceID: &id, Metric: "memory_pct", Op: "gt", ChannelIDs: []string{ch}})
	if m.InstanceID.ValueString() != upper {
		t.Errorf("instance_id spelling not kept: %v", m.InstanceID)
	}
	if !m.ChannelIDs.Equal(stringSet([]string{strings.ToUpper(ch)})) {
		t.Errorf("channel id spelling not kept: %v", m.ChannelIDs)
	}
}

func TestAlertRuleConfigValidation(t *testing.T) {
	r := &alertRuleResource{}
	sch := resourceSchema(t, r)
	ch := tfStrSet("00000000-0000-4000-8000-0000000000c1")
	cases := []struct {
		name string
		vals map[string]tftypes.Value
		want string
	}{
		{"memory", map[string]tftypes.Value{"name": tfStr("a"), "metric": tfStr("memory_pct"), "instance_id": tfStr(testInstanceID), "channel_ids": ch}, ""},
		{"memory without a database", map[string]tftypes.Value{"name": tfStr("a"), "metric": tfStr("memory_pct"), "channel_ids": ch}, "Memory used alerts are about one database"},
		{"runway", map[string]tftypes.Value{"name": tfStr("a"), "metric": tfStr("credit_runway_days"), "channel_ids": ch}, ""},
		{"runway with a database", map[string]tftypes.Value{"name": tfStr("a"), "metric": tfStr("credit_runway_days"), "instance_id": tfStr(testInstanceID), "channel_ids": ch}, "about the organization"},
		{"failover", map[string]tftypes.Value{"name": tfStr("a"), "metric": tfStr("failover"), "instance_id": tfStr(testInstanceID), "channel_ids": ch}, ""},
		{"failover with threshold", map[string]tftypes.Value{"name": tfStr("a"), "metric": tfStr("failover"), "instance_id": tfStr(testInstanceID), "threshold": tfNum(1), "channel_ids": ch}, "no `threshold`"},
		{"failover with for_minutes", map[string]tftypes.Value{"name": tfStr("a"), "metric": tfStr("failover"), "instance_id": tfStr(testInstanceID), "for_minutes": tfNum(3), "channel_ids": ch}, "no `for_minutes`"},
	}
	for _, tc := range cases {
		errs := validateResource(t, r, sch, tc.vals)
		joined := strings.Join(errs, "\n")
		if tc.want == "" && len(errs) > 0 {
			t.Errorf("%s: must be valid, got %s", tc.name, joined)
		}
		if tc.want != "" && !strings.Contains(joined, tc.want) {
			t.Errorf("%s: want an error mentioning %q, got %q", tc.name, tc.want, joined)
		}
	}
}

func TestAlertRuleAttributeValidatorsMirrorTheZodSchema(t *testing.T) {
	sch := resourceSchema(t, &alertRuleResource{})
	ctx := context.Background()

	metric := sch.Attributes["metric"].(rschema.StringAttribute)
	for name := range alertMetrics {
		if d := runStringValidators(metric.Validators, name); d.HasError() {
			t.Errorf("metric %q must be accepted", name)
		}
	}
	if d := runStringValidators(metric.Validators, "cpu_pct"); !d.HasError() {
		t.Error("an unknown metric must be rejected")
	}

	int64OK := func(attr string, v int64) bool {
		a := sch.Attributes[attr].(rschema.Int64Attribute)
		ok := true
		for _, val := range a.Validators {
			var res validator.Int64Response
			val.ValidateInt64(ctx, validator.Int64Request{ConfigValue: types.Int64Value(v), Path: path.Root(attr)}, &res)
			ok = ok && !res.Diagnostics.HasError()
		}
		return ok
	}
	for _, tc := range []struct {
		attr string
		v    int64
		ok   bool
	}{
		{"for_minutes", 0, true}, {"for_minutes", 1440, true}, {"for_minutes", -1, false}, {"for_minutes", 1441, false},
		{"cooldown_minutes", 5, true}, {"cooldown_minutes", 10080, true}, {"cooldown_minutes", 4, false}, {"cooldown_minutes", 10081, false},
	} {
		if int64OK(tc.attr, tc.v) != tc.ok {
			t.Errorf("%s = %d: accepted should be %v", tc.attr, tc.v, tc.ok)
		}
	}

	threshold := sch.Attributes["threshold"].(rschema.Float64Attribute)
	floatOK := func(v float64) bool {
		ok := true
		for _, val := range threshold.Validators {
			var res validator.Float64Response
			val.ValidateFloat64(ctx, validator.Float64Request{ConfigValue: types.Float64Value(v), Path: path.Root("threshold")}, &res)
			ok = ok && !res.Diagnostics.HasError()
		}
		return ok
	}
	if !floatOK(0) || !floatOK(12.5) || floatOK(-1) {
		t.Error("threshold must be zero or more")
	}

	channels := sch.Attributes["channel_ids"].(rschema.SetAttribute)
	setOK := func(vs []string) bool {
		ok := true
		for _, val := range channels.Validators {
			var res validator.SetResponse
			val.ValidateSet(ctx, validator.SetRequest{ConfigValue: stringSet(vs), Path: path.Root("channel_ids")}, &res)
			ok = ok && !res.Diagnostics.HasError()
		}
		return ok
	}
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	if !setOK(eleven[:10]) || setOK(eleven) || setOK(nil) || setOK([]string{"not-a-uuid"}) {
		t.Error("channel_ids must be 1 to 10 UUIDs")
	}
}

func TestAlertRuleDefaultsFollowTheMetric(t *testing.T) {
	ctx := context.Background()
	sch := resourceSchema(t, &alertRuleResource{})
	for metric, want := range map[string]struct {
		op   string
		th   float64
		for_ int64
	}{
		"memory_pct":         {"gt", 80, 5},
		"ops_per_sec":        {"gt", 10000, 5},
		"latency_p99_ms":     {"gt", 5, 5},
		"credit_runway_days": {"lt", 7, 5},
		"failover":           {"gt", 0, 0},
	} {
		cfg := tfsdk.Config{Schema: sch, Raw: planOf(t, sch, map[string]tftypes.Value{"metric": tfStr(metric)})}
		plan := tfsdk.Plan{Schema: sch, Raw: cfg.Raw}

		sres := planmodifier.StringResponse{PlanValue: types.StringUnknown()}
		metricDefaultOp{}.PlanModifyString(ctx, planmodifier.StringRequest{Config: cfg, Plan: plan, ConfigValue: types.StringNull(), PlanValue: types.StringUnknown()}, &sres)
		fres := planmodifier.Float64Response{PlanValue: types.Float64Unknown()}
		metricDefaultThreshold{}.PlanModifyFloat64(ctx, planmodifier.Float64Request{Config: cfg, Plan: plan, ConfigValue: types.Float64Null(), PlanValue: types.Float64Unknown()}, &fres)
		ires := planmodifier.Int64Response{PlanValue: types.Int64Unknown()}
		metricDefaultForMinutes{}.PlanModifyInt64(ctx, planmodifier.Int64Request{Config: cfg, Plan: plan, ConfigValue: types.Int64Null(), PlanValue: types.Int64Unknown()}, &ires)

		if sres.PlanValue.ValueString() != want.op || fres.PlanValue.ValueFloat64() != want.th || ires.PlanValue.ValueInt64() != want.for_ {
			t.Errorf("%s: planned %v %v %v, want %+v", metric, sres.PlanValue, fres.PlanValue, ires.PlanValue, want)
		}
	}
}

func TestAlertRuleRefusalKeepsTheServersWords(t *testing.T) {
	_, r, sch, _ := newRuleFixture(t)
	res := create(t, r, sch, map[string]tftypes.Value{
		"name": tfStr("x"), "metric": tfStr("memory_pct"), "instance_id": tfStr(testInstanceID),
		"op": tfStr("gt"), "threshold": tfNum(80), "for_minutes": tfNum(5), "cooldown_minutes": tfNum(60),
		"channel_ids": tfStrSet("00000000-0000-4000-8000-00000000dead"), "enabled": tfBool(true),
	})
	if !res.Diagnostics.HasError() {
		t.Fatal("expected the refusal")
	}
	if d := res.Diagnostics.Errors()[0].Detail(); d != "One of the chosen channels does not exist in this organization." {
		t.Errorf("refusal not surfaced verbatim: %q", d)
	}
}

// The provider's metric list is the API's. The API publishes no endpoint for
// it, so this pins the copy against the source it was taken from.
func TestAlertMetricsMatchTheAPI(t *testing.T) {
	want := "connected_clients,credit_runway_days,failover,latency_p99_ms,memory_pct,ops_per_sec,replication_lag_seconds"
	if got := strings.Join(alertMetricNames(), ","); got != want {
		t.Errorf("metrics = %s", got)
	}
	sch := resourceSchema(t, &alertRuleResource{})
	desc := sch.Attributes["metric"].GetMarkdownDescription()
	for name := range alertMetrics {
		if !strings.Contains(desc, "`"+name+"`") {
			t.Errorf("the metric description must list %s", name)
		}
	}
}

// --- kividb_audit_log_settings ----------------------------------------------

func TestAuditLogSettingsDataSource(t *testing.T) {
	f, c := newFakeAlertsAPI(t)
	f.audit = map[string]any{"enabled": true, "enabled_at": "2026-10-24T09:00:00.000Z", "enabled_by": "owner@example.com",
		"hot_days": 90, "retention_days": 400, "can_change": false}
	d := &auditLogSettingsDataSource{client: c}
	sch := dataSourceSchema(t, d)
	ctx := context.Background()
	res := datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
	d.Read(ctx, datasource.ReadRequest{}, &res)
	failOn(t, res.Diagnostics)
	var got auditLogSettingsModel
	failOn(t, res.State.Get(ctx, &got))
	if !got.Enabled.ValueBool() || got.EnabledBy.ValueString() != "owner@example.com" || got.HotDays.ValueInt64() != 90 || got.RetentionDays.ValueInt64() != 400 {
		t.Errorf("settings wrong: %+v", got)
	}

	f.audit = map[string]any{"enabled": false, "enabled_at": nil, "enabled_by": nil, "hot_days": 90, "retention_days": 400}
	res = datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
	d.Read(ctx, datasource.ReadRequest{}, &res)
	failOn(t, res.Diagnostics)
	failOn(t, res.State.Get(ctx, &got))
	if got.Enabled.ValueBool() || !got.EnabledAt.IsNull() || !got.EnabledBy.IsNull() {
		t.Errorf("an organization with the log off: %+v", got)
	}
}

func TestAuditLogSettingsRefusalKeepsTheServersWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refuse(w, 403, "forbidden", "Only owners and admins can read the audit log.")
	}))
	defer srv.Close()
	d := &auditLogSettingsDataSource{client: NewClient(srv.URL, "k")}
	sch := dataSourceSchema(t, d)
	ctx := context.Background()
	res := datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
	d.Read(ctx, datasource.ReadRequest{}, &res)
	if !res.Diagnostics.HasError() || !strings.Contains(res.Diagnostics.Errors()[0].Detail(), "Only owners and admins can read the audit log.") {
		t.Errorf("refusal not surfaced: %v", res.Diagnostics)
	}
}

func TestAlertResourcesAreRegistered(t *testing.T) {
	p := New("test")()
	names := map[string]bool{}
	for _, f := range p.Resources(context.Background()) {
		var res resource.MetadataResponse
		f().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "kividb"}, &res)
		names[res.TypeName] = true
	}
	for _, f := range p.DataSources(context.Background()) {
		var res datasource.MetadataResponse
		f().Metadata(context.Background(), datasource.MetadataRequest{ProviderTypeName: "kividb"}, &res)
		names[res.TypeName] = true
	}
	for _, want := range []string{"kividb_alert_channel", "kividb_alert_rule", "kividb_audit_log_settings"} {
		if !names[want] {
			t.Errorf("%s is not registered", want)
		}
	}
}
