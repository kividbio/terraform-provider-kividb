package provider

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func akPlan(t *testing.T) contextAgentKeyModel {
	return contextAgentKeyModel{
		ID:        types.StringUnknown(),
		Model:     types.StringValue("catalog"),
		Name:      types.StringValue("acme support bot"),
		Entities:  types.ListValueMust(types.StringType, []attr.Value{types.StringValue("customer")}),
		Filters:   types.MapValueMust(filtersType, map[string]attr.Value{"customer": types.MapValueMust(types.StringType, map[string]attr.Value{"tenant": types.StringValue("acme")})}),
		Key:       types.StringUnknown(),
		KeyPrefix: types.StringUnknown(),
		MCPURL:    types.StringUnknown(),
		CreatedAt: types.StringUnknown(),
	}
}

func modelWithKeys(t *testing.T) *fakeAI {
	f := contextFake(t)
	f.putHook(cmPath, map[string]any{"instance_id": testInstanceID, "entities": []any{}})
	return f
}

func TestContextAgentKeySchema(t *testing.T) {
	sch := schemaOf(t, NewContextAgentKeyResource())
	checkDescribed(t, sch)
	checkReplacement(t, sch, []string{"model", "name", "entities", "filters"}, nil)
	if !sch.Attributes["key"].IsSensitive() || !sch.Attributes["key"].IsComputed() {
		t.Error("key must be computed and sensitive")
	}
}

func TestContextAgentKeyLifecycle(t *testing.T) {
	f := modelWithKeys(t)
	r := &contextAgentKeyResource{client: f.client()}
	sch := schemaOf(t, r)

	st, d := doCreate(t, r, sch, akPlan(t))
	failOn(t, d)
	post := f.callsTo("POST", cmPath+"/keys")
	if len(post) != 1 {
		t.Fatalf("create sent %d POSTs", len(post))
	}
	b := post[0].Body
	if b["name"] != "acme support bot" || fmt.Sprint(b["entities"]) != "[customer]" || fmt.Sprint(b["filters"]) != "map[customer:map[tenant:acme]]" {
		t.Errorf("create sent %v", b)
	}
	m := getModel[contextAgentKeyModel](t, st)
	if m.Key.ValueString() != "kvctx_secret1" || m.ID.ValueString() == "" || m.KeyPrefix.ValueString() != "kvctx_abcdef" ||
		m.MCPURL.ValueString() != f.srv.URL+"/context/mcp" {
		t.Errorf("state after create: %+v", m)
	}

	// Refresh keeps the secret, which is never listed.
	st, d = doRead(t, r, st)
	failOn(t, d)
	if got := getModel[contextAgentKeyModel](t, st); got.Key.ValueString() != "kvctx_secret1" || len(got.Filters.Elements()) != 1 {
		t.Errorf("read: %+v", got)
	}

	// Delete revokes it, by id.
	failOn(t, doDelete(t, r, st))
	if len(f.callsTo("DELETE", cmPath+"/keys/"+m.ID.ValueString())) != 1 {
		t.Error("delete did not revoke the key")
	}
	// Revoked already is not a failure.
	failOn(t, doDelete(t, r, st))
}

func TestContextAgentKeyRevokedElsewhereLeavesState(t *testing.T) {
	f := modelWithKeys(t)
	r := &contextAgentKeyResource{client: f.client()}
	sch := schemaOf(t, r)
	st, d := doCreate(t, r, sch, akPlan(t))
	failOn(t, d)

	// Revoked in the console: the model no longer lists it.
	obj := f.get(cmPath)
	obj["keys"] = []map[string]any{}
	f.set(cmPath, obj)
	after, d := doRead(t, r, st)
	failOn(t, d)
	if !after.Raw.IsNull() {
		t.Error("a revoked key must leave state")
	}

	// The model deleted, and its keys with it.
	f.set(cmPath, nil)
	f.mu.Lock()
	delete(f.objects, cmPath)
	f.mu.Unlock()
	after, d = doRead(t, r, st)
	failOn(t, d)
	if !after.Raw.IsNull() {
		t.Error("a key whose model is gone must leave state")
	}
}

func TestContextAgentKeyWithoutLimits(t *testing.T) {
	f := modelWithKeys(t)
	r := &contextAgentKeyResource{client: f.client()}
	sch := schemaOf(t, r)
	plan := akPlan(t)
	plan.Entities = types.ListNull(types.StringType)
	plan.Filters = types.MapNull(filtersType)
	st, d := doCreate(t, r, sch, plan)
	failOn(t, d)
	b := f.callsTo("POST", cmPath+"/keys")[0].Body
	if _, ok := b["entities"]; ok {
		t.Errorf("unset entities must not be sent (that means all of them): %v", b)
	}
	// {} from the API reads as the null that was configured.
	m := getModel[contextAgentKeyModel](t, st)
	if !m.Entities.IsNull() || !m.Filters.IsNull() {
		t.Errorf("state: %+v", m)
	}
	st, d = doRead(t, r, st)
	failOn(t, d)
	if m = getModel[contextAgentKeyModel](t, st); !m.Entities.IsNull() || !m.Filters.IsNull() {
		t.Errorf("read: %+v", m)
	}
}

func TestContextAgentKeyImport(t *testing.T) {
	f := modelWithKeys(t)
	r := &contextAgentKeyResource{client: f.client()}
	sch := schemaOf(t, r)
	created, d := doCreate(t, r, sch, akPlan(t))
	failOn(t, d)
	id := getModel[contextAgentKeyModel](t, created).ID.ValueString()

	// A key made in the console with a numeric filter.
	obj := f.get(cmPath)
	obj["keys"].([]map[string]any)[0]["filters"] = map[string]any{"order": map[string]any{"total": 5}}

	st, d := doImport(t, r, sch, "catalog/"+id)
	failOn(t, d)
	st, d = doRead(t, r, st)
	failOn(t, d)
	m := getModel[contextAgentKeyModel](t, st)
	if m.Name.ValueString() != "acme support bot" || !m.Key.IsNull() || m.Model.ValueString() != "catalog" {
		t.Errorf("imported: %+v", m)
	}
	var filters map[string]map[string]string
	failOn(t, m.Filters.ElementsAs(context.Background(), &filters, false))
	if filters["order"]["total"] != "5" {
		t.Errorf("numeric filter read as %v", filters)
	}

	_, d = doImport(t, r, sch, id)
	expectError(t, d, "<model>/<key_id>")
}

func TestContextAgentKeyRefusalsKeepTheServersWords(t *testing.T) {
	f := modelWithKeys(t)
	r := &contextAgentKeyResource{client: f.client()}
	f.fail("POST", cmPath+"/keys", 400, `{"error":{"code":"bad_filter","message":"customer.name must be one of its tag or numeric fields to filter on."}}`)
	_, d := doCreate(t, r, schemaOf(t, r), akPlan(t))
	expectError(t, d, "customer.name must be one of its tag or numeric fields to filter on.")
}
