package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const networkPath = "/instances/i-1/network"
const privateLinkPath = "/instances/i-1/private-link"

// fakeNetwork is the network half of the API for one database, i-1: it keeps
// the rules and flags a PUT sends, and answers the way network.ts does,
// including its refusals.
type fakeNetwork struct {
	rules          []map[string]any
	tlsEnabled     bool
	tlsOnly        bool
	privateEnabled bool
	blocked        bool
	clientCertMode string
	serviceName    any
	principals     []any
	reconcile      map[string]any
}

func (n *fakeNetwork) view() string {
	ports := []int{6379}
	switch {
	case n.blocked:
		ports = []int{}
	case n.tlsEnabled && n.tlsOnly:
		ports = []int{6380}
	case n.tlsEnabled:
		ports = []int{6379, 6380}
	}
	port := 0
	if len(ports) > 0 {
		port = ports[0]
	}
	rules := n.rules
	if rules == nil {
		rules = []map[string]any{}
	}
	rec := n.reconcile
	if rec == nil {
		rec = map[string]any{"synced": true}
	}
	b, _ := json.Marshal(map[string]any{"rules": rules, "port": port, "ports": ports, "tls_enabled": n.tlsEnabled,
		"tls_only": n.tlsOnly, "private_network_enabled": n.privateEnabled, "public_access_blocked": n.blocked,
		"reconcile": rec})
	return string(b)
}

func refusal(code, message string) apiReply {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": message}})
	return apiReply{400, string(b)}
}

func (n *fakeNetwork) serve(f *fakeAPI) {
	f.on("GET "+networkPath, func(apiCall) apiReply { return apiReply{200, n.view()} })
	f.on("PUT "+networkPath, func(c apiCall) apiReply {
		blocked, tlsOnly := n.blocked, n.tlsOnly
		if v, ok := c.Body["public_access_blocked"].(bool); ok {
			blocked = v
		}
		if v, ok := c.Body["tls_only"].(bool); ok {
			tlsOnly = v
		}
		if blocked && !n.privateEnabled {
			return refusal("public_block_requires_private_link",
				"Enable private connectivity before blocking public access, or the instance becomes unreachable.")
		}
		if tlsOnly && !n.tlsEnabled {
			return refusal("tls_only_requires_tls", "Enable TLS on this instance before requiring TLS-only connections.")
		}
		if !tlsOnly && n.tlsOnly && n.clientCertMode == "required" {
			return refusal("tls_only_required_by_client_certs", "This database requires client certificates, which only TLS can check. Set client certificates to optional or off first.")
		}
		n.blocked, n.tlsOnly = blocked, tlsOnly
		n.rules = nil
		for i, r := range c.Body["rules"].([]any) {
			rule := r.(map[string]any)
			n.rules = append(n.rules, map[string]any{"id": string(rune('a' + i)), "cidr": rule["cidr"], "port": 6379,
				"action": rule["action"], "label": rule["label"]})
		}
		return apiReply{200, n.view()}
	})
	f.on("GET "+privateLinkPath, func(apiCall) apiReply {
		principals := n.principals
		if principals == nil {
			principals = []any{}
		}
		b, _ := json.Marshal(map[string]any{"enabled": n.privateEnabled, "region": "eu-central-1", "tls_enabled": n.tlsEnabled,
			"ports":        []map[string]any{{"port": 6379, "role": "primary", "tls": false, "label": "Primary"}},
			"service_name": n.serviceName, "allowed_principals": principals, "public_access_blocked": n.blocked})
		return apiReply{200, string(b)}
	})
	f.on("PUT "+privateLinkPath, func(c apiCall) apiReply {
		enabled := n.privateEnabled
		if v, ok := c.Body["enabled"].(bool); ok {
			enabled = v
		}
		if !enabled && n.blocked {
			return refusal("private_link_in_use", "Public access is blocked on this instance. Allow public access first, or it would become unreachable.")
		}
		n.privateEnabled = enabled
		if p, ok := c.Body["allowed_principals"].([]any); ok {
			n.principals = p
		}
		if !enabled {
			n.serviceName = nil
		}
		return apiReply{200, `{"applying":true}`}
	})
}

func newNetworkAPI(t *testing.T, n *fakeNetwork) *fakeAPI {
	f := newFakeAPI(t)
	n.serve(f)
	return f
}

func allowlist(entries ...[2]string) tftypes.Value {
	typ := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"cidr": tftypes.String, "label": tftypes.String}}
	vals := []tftypes.Value{}
	for _, e := range entries {
		label := tftypes.NewValue(tftypes.String, nil)
		if e[1] != "" {
			label = dsStr(e[1])
		}
		vals = append(vals, tftypes.NewValue(typ, map[string]tftypes.Value{"cidr": dsStr(e[0]), "label": label}))
	}
	return tftypes.NewValue(tftypes.Set{ElementType: typ}, vals)
}

func networkState(t *testing.T, s tfsdk.State) instanceNetworkModel {
	t.Helper()
	var m instanceNetworkModel
	failOn(t, s.Get(context.Background(), &m))
	return m
}

func TestInstanceNetworkCreateSendsOnlyTheAllowlist(t *testing.T) {
	n := &fakeNetwork{tlsEnabled: true, tlsOnly: true}
	f := newNetworkAPI(t, n)
	res := dsDoCreate(t, &instanceNetworkResource{client: f.client()}, map[string]tftypes.Value{
		"instance_id": dsStr("i-1"), "allowlist": allowlist([2]string{"203.0.113.7/32", "office"}, [2]string{"198.51.100.0/24", ""}),
	})
	failOn(t, res.Diagnostics)

	body := f.sent("PUT " + networkPath)[0].Body
	for _, k := range []string{"tls_only", "public_access_blocked"} {
		if _, sent := body[k]; sent {
			t.Errorf("%s belongs to another resource and must not be sent: %v", k, body)
		}
	}
	if rules := body["rules"].([]any); len(rules) != 2 {
		t.Errorf("rules sent = %v", rules)
	}
	got := networkState(t, res.State)
	if len(got.Allowlist.Elements()) != 2 || got.Port.ValueInt64() != 6380 || !got.Synced.ValueBool() || got.ID.ValueString() != "i-1" {
		t.Errorf("state = %+v", got)
	}
	if !n.tlsOnly {
		t.Error("the allowlist change must leave tls_only alone")
	}
}

func TestInstanceNetworkReadRefreshesAndWarnsAboutDenyRules(t *testing.T) {
	n := &fakeNetwork{rules: []map[string]any{
		{"id": "a", "cidr": "203.0.113.7/32", "port": 6379, "action": "allow", "label": "office"},
		{"id": "b", "cidr": "192.0.2.0/24", "port": 6379, "action": "deny", "label": nil},
	}, reconcile: map[string]any{"synced": false, "error": "Network settings will apply once the instance is available."}}
	f := newNetworkAPI(t, n)
	res := dsDoRead(t, &instanceNetworkResource{client: f.client()}, map[string]tftypes.Value{
		"id": dsStr("i-1"), "instance_id": dsStr("i-1"), "allowlist": allowlist(),
	})
	failOn(t, res.Diagnostics)
	got := networkState(t, res.State)
	if len(got.Allowlist.Elements()) != 1 || got.Synced.ValueBool() || !strings.Contains(got.SyncError.ValueString(), "once the instance") {
		t.Errorf("state = %+v", got)
	}
	if res.Diagnostics.WarningsCount() != 1 || !strings.Contains(res.Diagnostics.Warnings()[0].Detail(), "192.0.2.0/24") {
		t.Errorf("a deny rule must be named as not enforced: %v", res.Diagnostics.Warnings())
	}
}

func TestInstanceNetworkGoneIsRemoved(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET "+networkPath, 404, `{"error":{"code":"not_found"}}`)
	res := dsDoRead(t, &instanceNetworkResource{client: f.client()}, map[string]tftypes.Value{"id": dsStr("i-1"), "instance_id": dsStr("i-1")})
	failOn(t, res.Diagnostics)
	if !res.State.Raw.IsNull() {
		t.Error("the allowlist of a database that is gone must be removed from state")
	}
}

func TestInstanceNetworkUpdateReplacesTheList(t *testing.T) {
	n := &fakeNetwork{}
	f := newNetworkAPI(t, n)
	prior := map[string]tftypes.Value{"id": dsStr("i-1"), "instance_id": dsStr("i-1"), "allowlist": allowlist([2]string{"203.0.113.7/32", ""})}
	res := dsDoUpdate(t, &instanceNetworkResource{client: f.client()}, prior, map[string]tftypes.Value{
		"id": dsStr("i-1"), "instance_id": dsStr("i-1"), "allowlist": allowlist([2]string{"198.51.100.9/32", "ci"}),
	})
	failOn(t, res.Diagnostics)
	if len(n.rules) != 1 || n.rules[0]["cidr"] != "198.51.100.9/32" || n.rules[0]["label"] != "ci" {
		t.Errorf("rules now = %v", n.rules)
	}
}

// Destroying leaves the allowlist alone: no request is made at all.
func TestInstanceNetworkDeleteChangesNothing(t *testing.T) {
	f := newFakeAPI(t) // no routes: any request fails the test
	failOn(t, dsDoDelete(t, &instanceNetworkResource{client: f.client()}, map[string]tftypes.Value{
		"id": dsStr("i-1"), "instance_id": dsStr("i-1"), "allowlist": allowlist([2]string{"203.0.113.7/32", ""}),
	}).Diagnostics)
}

func TestInstanceNetworkImport(t *testing.T) {
	res := dsDoImport(t, &instanceNetworkResource{}, "i-1")
	failOn(t, res.Diagnostics)
	if got := networkState(t, res.State); got.InstanceID.ValueString() != "i-1" || got.ID.ValueString() != "i-1" {
		t.Errorf("import state = %+v", got)
	}
}

func TestInstanceNetworkRefusalsAreSurfaced(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       []string
	}{
		{"cidr", `{"error":{"code":"bad_request","message":"Invalid CIDR: 300.0.0.1/32"}}`, 400, []string{"Invalid CIDR: 300.0.0.1/32"}},
		{"firewall", `{"error":{"code":"sg_sync_failed","message":"Could not update network rules. Try again."}}`, 502,
			[]string{"Could not update network rules. Try again.", "Apply again"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.answer("PUT "+networkPath, tc.status, tc.body)
			res := dsDoCreate(t, &instanceNetworkResource{client: f.client()}, map[string]tftypes.Value{
				"instance_id": dsStr("i-1"), "allowlist": allowlist([2]string{"203.0.113.7/32", ""}),
			})
			wantError(t, res.Diagnostics, tc.want...)
		})
	}
}

func TestInstanceNetworkConfigValidation(t *testing.T) {
	ctx := context.Background()
	sch := dsResourceSchema(t, NewInstanceNetworkResource())
	run := func(list tftypes.Value) resource.ValidateConfigResponse {
		cfg := tfsdk.Config{Schema: sch, Raw: objectOf(t, sch.Type().TerraformType(ctx),
			map[string]tftypes.Value{"instance_id": dsStr("i-1"), "allowlist": list}, false)}
		var res resource.ValidateConfigResponse
		(&instanceNetworkResource{}).ValidateConfig(ctx, resource.ValidateConfigRequest{Config: cfg}, &res)
		return res
	}
	if res := run(allowlist([2]string{"203.0.113.7/32", "a"}, [2]string{"203.0.113.7/32", "b"})); !res.Diagnostics.HasError() {
		t.Error("the same address twice must be refused")
	}
	if res := run(allowlist()); res.Diagnostics.HasError() || res.Diagnostics.WarningsCount() != 1 {
		t.Errorf("an empty allowlist is allowed, with a warning: %v", res.Diagnostics)
	}
	if res := run(allowlist([2]string{"0.0.0.0/0", ""})); res.Diagnostics.WarningsCount() != 1 {
		t.Errorf("0.0.0.0/0 must warn: %v", res.Diagnostics)
	}

	for _, bad := range []string{"10.0.0.1", "010.0.0.1/32", "10.0.0.256/32", "10.0.0.0/33", "::1/128"} {
		if !canonicalCIDR.MatchString(bad) {
			continue
		}
		t.Errorf("%q must be refused", bad)
	}
	for _, good := range []string{"10.0.0.1/32", "0.0.0.0/0", "203.0.113.0/24", "255.255.255.255/32"} {
		if !canonicalCIDR.MatchString(good) {
			t.Errorf("%q must be accepted", good)
		}
	}
}

// --- kividb_private_link ---------------------------------------------------------

func testPrivateLink(f *fakeAPI) *privateLinkResource {
	return &privateLinkResource{client: f.client(), readyTimeout: time.Second, pollInterval: time.Millisecond}
}

func principals(vals ...string) tftypes.Value {
	out := []tftypes.Value{}
	for _, v := range vals {
		out = append(out, dsStr(v))
	}
	return tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, out)
}

const testARN = "arn:aws:iam::123456789012:root"

func plState(t *testing.T, s tfsdk.State) privateLinkModel {
	t.Helper()
	var m privateLinkModel
	failOn(t, s.Get(context.Background(), &m))
	return m
}

// Create turns it on, waits for the endpoint service, and only then blocks
// public access -- which the API would refuse before private connectivity is
// on.
func TestPrivateLinkCreateWaitsThenBlocks(t *testing.T) {
	n := &fakeNetwork{rules: []map[string]any{{"id": "a", "cidr": "203.0.113.7/32", "port": 6379, "action": "allow", "label": "office"}}}
	f := newNetworkAPI(t, n)
	polls := 0
	inner := f.routes["GET "+privateLinkPath]
	f.on("GET "+privateLinkPath, func(c apiCall) apiReply {
		polls++
		if polls == 3 {
			n.serviceName = "com.amazonaws.vpce.eu-central-1.vpce-svc-0abc"
		}
		return inner(c)
	})

	res := dsDoCreate(t, testPrivateLink(f), map[string]tftypes.Value{
		"instance_id": dsStr("i-1"), "allowed_principals": principals(testARN),
		"public_access_blocked": dsBoolean(true), "wait_for_ready": dsBoolean(true),
	})
	failOn(t, res.Diagnostics)

	put := f.sent("PUT " + privateLinkPath)[0].Body
	if put["enabled"] != true || len(put["allowed_principals"].([]any)) != 1 {
		t.Errorf("private-link PUT = %v", put)
	}
	if !n.blocked {
		t.Error("public access was not blocked")
	}
	// The block re-sent the allowlist as the API held it.
	if len(n.rules) != 1 || n.rules[0]["label"] != "office" {
		t.Errorf("the allowlist must survive the block: %v", n.rules)
	}
	got := plState(t, res.State)
	if got.ServiceName.ValueString() == "" || !got.PublicAccessBlocked.ValueBool() || got.Region.ValueString() != "eu-central-1" {
		t.Errorf("state = %+v", got)
	}
}

// A service that is never built fails the apply, but the add-on is on and
// billed, so it is still written to state.
func TestPrivateLinkTimeoutKeepsItInState(t *testing.T) {
	n := &fakeNetwork{}
	f := newNetworkAPI(t, n)
	r := testPrivateLink(f)
	r.readyTimeout = 10 * time.Millisecond
	res := dsDoCreate(t, r, map[string]tftypes.Value{
		"instance_id": dsStr("i-1"), "allowed_principals": principals(), "public_access_blocked": dsBoolean(false),
		"wait_for_ready": dsBoolean(true),
	})
	wantError(t, res.Diagnostics, "still being built")
	if res.State.Raw.IsNull() || plState(t, res.State).ID.ValueString() != "i-1" {
		t.Error("an enabled private link must be kept in state even when the wait fails")
	}
}

func TestPrivateLinkRefusalsAreSurfaced(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("PUT "+privateLinkPath, 400, `{"error":{"code":"byoc_private_link_not_needed","message":"This database runs in your own cloud account and is already inside your network, so private connectivity is not needed."}}`)
	res := dsDoCreate(t, testPrivateLink(f), map[string]tftypes.Value{
		"instance_id": dsStr("i-1"), "allowed_principals": principals(), "public_access_blocked": dsBoolean(false),
		"wait_for_ready": dsBoolean(true),
	})
	wantError(t, res.Diagnostics, "already inside your network, so private connectivity is not needed.")
	if !res.State.Raw.IsNull() {
		t.Error("a refused create must record nothing")
	}
}

func plStateVals(blocked bool, principalList ...string) map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"id": dsStr("i-1"), "instance_id": dsStr("i-1"), "allowed_principals": principals(principalList...),
		"public_access_blocked": dsBoolean(blocked), "wait_for_ready": dsBoolean(true),
		"service_name": dsStr("svc"), "region": dsStr("eu-central-1"),
	}
}

func TestPrivateLinkReadForgetsADisabledLink(t *testing.T) {
	n := &fakeNetwork{privateEnabled: false}
	f := newNetworkAPI(t, n)
	res := dsDoRead(t, testPrivateLink(f), plStateVals(false))
	failOn(t, res.Diagnostics)
	if !res.State.Raw.IsNull() {
		t.Error("private connectivity turned off elsewhere must be removed from state")
	}

	g := newFakeAPI(t)
	g.answer("GET "+privateLinkPath, 404, `{"error":{"code":"not_found"}}`)
	res = dsDoRead(t, testPrivateLink(g), plStateVals(false))
	failOn(t, res.Diagnostics)
	if !res.State.Raw.IsNull() {
		t.Error("the private link of a database that is gone must be removed from state")
	}
}

// Unblocking goes before the principal change; the service already exists.
func TestPrivateLinkUpdate(t *testing.T) {
	n := &fakeNetwork{privateEnabled: true, blocked: true, serviceName: "svc", principals: []any{testARN}}
	f := newNetworkAPI(t, n)
	plan := plStateVals(false, testARN, "arn:aws:iam::210987654321:role/app")
	delete(plan, "service_name")
	delete(plan, "region")
	res := dsDoUpdate(t, testPrivateLink(f), plStateVals(true, testARN), plan)
	failOn(t, res.Diagnostics)
	if n.blocked {
		t.Error("public access must be allowed again")
	}
	if len(n.principals) != 2 {
		t.Errorf("principals = %v", n.principals)
	}
	if body := f.sent("PUT " + privateLinkPath)[0].Body; body["enabled"] != nil {
		t.Errorf("a principal change must not resend enabled: %v", body)
	}
}

// Destroy opens public access first, because the API refuses to turn private
// connectivity off while it is the only way in.
func TestPrivateLinkDeleteUnblocksThenDisables(t *testing.T) {
	n := &fakeNetwork{privateEnabled: true, blocked: true, serviceName: "svc"}
	f := newNetworkAPI(t, n)
	failOn(t, dsDoDelete(t, testPrivateLink(f), plStateVals(true)).Diagnostics)
	if n.blocked || n.privateEnabled {
		t.Errorf("after destroy: blocked=%v enabled=%v", n.blocked, n.privateEnabled)
	}

	g := newFakeAPI(t)
	g.answer("GET "+privateLinkPath, 404, `{"error":{"code":"not_found"}}`)
	failOn(t, dsDoDelete(t, testPrivateLink(g), plStateVals(false)).Diagnostics)
}

func TestPrivateLinkImport(t *testing.T) {
	res := dsDoImport(t, &privateLinkResource{}, "i-1")
	failOn(t, res.Diagnostics)
	if got := plState(t, res.State); got.InstanceID.ValueString() != "i-1" || !got.WaitForReady.ValueBool() {
		t.Errorf("import state = %+v", got)
	}
}

func TestPrivateLinkPrincipalPattern(t *testing.T) {
	for _, good := range []string{testARN, "arn:aws:iam::123456789012:role/app", "00000000-0000-0000-0000-000000000000",
		"my-project-123", "123456789012"} {
		if !privateLinkPrincipal.MatchString(good) {
			t.Errorf("%q must be accepted", good)
		}
	}
	for _, bad := range []string{"*", "arn:aws:iam::123:root", "My Project", "arn:aws:s3:::bucket"} {
		if privateLinkPrincipal.MatchString(bad) {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// --- kividb_instance.tls_only ------------------------------------------------------

// On a database with TLS, TLS-only changes in place through the network route,
// which gets the allowlist back unchanged, and no engine change is made.
func TestTLSOnlyChangesInPlace(t *testing.T) {
	n := &fakeNetwork{tlsEnabled: true, rules: []map[string]any{{"id": "a", "cidr": "203.0.113.7/32", "port": 6379, "action": "allow", "label": "office"}}}
	f := newNetworkAPI(t, n)
	f.on("GET /instances/i-1", func(apiCall) apiReply {
		return apiReply{200, instanceJSON(t, map[string]any{"tls_only": n.tlsOnly})}
	})
	plan := instanceStateVals()
	delete(plan, "status")
	plan["tls_only"] = dsBoolean(true)
	res := dsDoUpdate(t, &instanceResource{client: f.client()}, instanceStateVals(), plan)
	failOn(t, res.Diagnostics)
	if !n.tlsOnly {
		t.Fatal("tls_only was not set")
	}
	if body := f.sent("PUT " + networkPath)[0].Body; body["tls_only"] != true || len(body["rules"].([]any)) != 1 {
		t.Errorf("network PUT = %v", body)
	}
	if len(n.rules) != 1 || n.rules[0]["label"] != "office" {
		t.Errorf("the allowlist must come back unchanged: %v", n.rules)
	}
}

// TLS on, TLS-only on and client certificates required, in one apply: each is
// refused before the one it rests on, so the provider restarts with TLS, makes
// the database TLS-only, then restarts again to require certificates.
func TestTLSOnlyIsOrderedAgainstTheEngineChanges(t *testing.T) {
	n := &fakeNetwork{}
	f := newNetworkAPI(t, n)
	var order []string
	f.on("GET /instances/i-1", func(apiCall) apiReply {
		return apiReply{200, instanceJSON(t, map[string]any{"tls_enabled": n.tlsEnabled, "tls_only": n.tlsOnly,
			"client_cert_mode": stringOr(&n.clientCertMode, "off"), "client_ca_pem": map[bool]any{true: testCA, false: nil}[n.clientCertMode == "required"]})}
	})
	f.on("POST /instances/i-1/binary", func(c apiCall) apiReply {
		if m, ok := c.Body["client_cert_mode"].(string); ok {
			if m == "required" && !n.tlsOnly {
				return apiReply{400, `{"error":{"code":"client_certs_required_needs_tls_only","message":"Requiring client certificates needs \"Require TLS\" on too."}}`}
			}
			n.clientCertMode = m
			order = append(order, "binary:client_cert_mode")
		}
		if v, ok := c.Body["tls_enabled"].(bool); ok {
			n.tlsEnabled = v
			order = append(order, "binary:tls_enabled")
		}
		return apiReply{202, `{"job_id":"j"}`}
	})
	inner := f.routes["PUT "+networkPath]
	f.on("PUT "+networkPath, func(c apiCall) apiReply {
		r := inner(c)
		if r.Status == 200 {
			order = append(order, "network:tls_only")
		}
		return r
	})

	prior := instanceStateVals()
	prior["tls_enabled"] = dsBoolean(false)
	plan := instanceStateVals()
	delete(plan, "status")
	plan["tls_only"], plan["client_cert_mode"], plan["client_ca_pem"] = dsBoolean(true), dsStr("required"), dsStr(testCA)
	res := dsDoUpdate(t, &instanceResource{client: f.client()}, prior, plan)
	failOn(t, res.Diagnostics)
	if got := strings.Join(order, " > "); got != "binary:tls_enabled > network:tls_only > binary:client_cert_mode" {
		t.Errorf("order = %s", got)
	}
}

func TestTLSOnlyRefusalIsSurfaced(t *testing.T) {
	n := &fakeNetwork{tlsEnabled: true, tlsOnly: true, clientCertMode: "required"}
	f := newNetworkAPI(t, n)
	f.on("GET /instances/i-1", func(apiCall) apiReply { return apiReply{200, instanceJSON(t, nil)} })
	prior := instanceStateVals()
	prior["tls_only"] = dsBoolean(true)
	// State believes client certificates are optional; the database says required.
	plan := instanceStateVals()
	plan["tls_only"] = dsBoolean(false)
	res := dsDoUpdate(t, &instanceResource{client: f.client()}, prior, plan)
	wantError(t, res.Diagnostics, "This database requires client certificates, which only TLS can check.")
}
