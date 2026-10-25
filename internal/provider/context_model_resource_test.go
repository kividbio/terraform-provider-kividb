package provider

import (
	"fmt"
	"strings"
	"testing"

	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const cmPath = "/context-models/catalog"

// contextFake answers the context-model routes the way routes/context-retriever.ts
// does: the entities come back as zod parsed them (descriptions trimmed,
// storage, id_field and relationships defaulted), with one search tool per
// entity, and the GET lists the keys that are not revoked.
func contextFake(t *testing.T) *fakeAI {
	f := newFakeAI(t)
	keys := []map[string]any{}
	nextKey := 0

	f.putHook = func(path string, b map[string]any) (int, any) {
		status := 201
		prev := f.get(path)
		if prev != nil {
			status = 200
			if prev["model"].(map[string]any)["instance"].(map[string]any)["id"] != b["instance_id"] {
				return 409, map[string]any{"error": map[string]any{"code": "immutable", "message": "A model's database cannot change; create another model."}}
			}
		}
		entities, _ := b["entities"].([]any)
		tools := []any{}
		for _, raw := range entities {
			e := raw.(map[string]any)
			e["description"] = strings.TrimSpace(e["description"].(string))
			if e["storage"] == nil {
				e["storage"] = "hash"
			}
			if e["id_field"] == nil {
				e["id_field"] = "id"
			}
			if e["relationships"] == nil {
				e["relationships"] = []any{}
			}
			for _, rf := range e["fields"].([]any) {
				if d, ok := rf.(map[string]any)["description"].(string); ok {
					rf.(map[string]any)["description"] = strings.TrimSpace(d)
				}
			}
			tools = append(tools, map[string]any{"name": "search_" + e["name"].(string), "description": "x", "inputSchema": map[string]any{}})
		}
		desc, _ := b["description"].(string)
		model := map[string]any{
			"name": "catalog", "description": strings.TrimSpace(desc),
			"instance": map[string]any{"id": b["instance_id"]}, "entities": entities,
			"created_at": "2026-10-25T10:00:00.000Z", "updated_at": "2026-10-25T10:00:00.000Z",
		}
		f.set(path, map[string]any{"model": model, "tools": tools, "counts": nil, "keys": keys})
		return status, map[string]any{"model": model, "tools": tools}
	}

	f.hook = func(method, path string, b map[string]any) (int, any, bool) {
		switch {
		case method == "POST" && path == cmPath+"/keys":
			if f.get(cmPath) == nil {
				return 404, map[string]any{"error": map[string]any{"code": "model_not_found", "message": "No such context model."}}, true
			}
			nextKey++
			filters := b["filters"]
			if filters == nil {
				filters = map[string]any{}
			}
			k := map[string]any{
				"id": fmt.Sprintf("00000000-0000-4000-8000-%012d", nextKey), "name": strings.TrimSpace(b["name"].(string)),
				"key_prefix": "kvctx_abcdef", "entities": b["entities"], "filters": filters,
				"created_at": "2026-10-25T11:00:00.000Z", "last_used_at": nil, "revoked": false,
			}
			keys = append(keys, k)
			obj := f.get(cmPath)
			obj["keys"] = keys
			f.set(cmPath, obj)
			return 201, map[string]any{"key": fmt.Sprintf("kvctx_secret%d", nextKey), "agent_key": k}, true
		case method == "DELETE" && strings.HasPrefix(path, cmPath+"/keys/"):
			id := strings.TrimPrefix(path, cmPath+"/keys/")
			for i, k := range keys {
				if k["id"] == id {
					keys = append(keys[:i:i], keys[i+1:]...)
					if obj := f.get(cmPath); obj != nil {
						obj["keys"] = keys
						f.set(cmPath, obj)
					}
					return 204, nil, true
				}
			}
			return 404, map[string]any{"error": map[string]any{"code": "key_not_found", "message": "No such agent key."}}, true
		}
		return 0, nil, false
	}
	return f
}

func cmEntities() []contextEntityModel {
	return []contextEntityModel{
		{
			Name:        types.StringValue("customer"),
			Description: types.StringValue("A customer of the shop.  "), // trailing spaces: the API trims them
			Prefix:      types.StringValue("customer:"),
			Storage:     types.StringValue("hash"),
			IDField:     types.StringValue("id"),
			Fields: []contextFieldModel{
				{Name: types.StringValue("name"), Type: types.StringValue("text"), Description: types.StringValue("Full name")},
				{Name: types.StringValue("tenant"), Type: types.StringValue("tag"), Description: types.StringNull()},
			},
			Relationships: []contextRelationshipModel{
				{Name: types.StringValue("orders"), Entity: types.StringValue("order"), Field: types.StringValue("customer_id"), Description: types.StringNull()},
			},
		},
		{
			Name:        types.StringValue("order"),
			Description: types.StringValue("An order a customer placed."),
			Prefix:      types.StringValue("order:"),
			Storage:     types.StringValue("json"),
			IDField:     types.StringValue("order_id"),
			Fields: []contextFieldModel{
				{Name: types.StringValue("customer_id"), Type: types.StringValue("tag"), Description: types.StringNull()},
				{Name: types.StringValue("total"), Type: types.StringValue("numeric"), Description: types.StringNull()},
			},
			Relationships: []contextRelationshipModel{},
		},
	}
}

func cmPlan() contextModelModel {
	return contextModelModel{
		ID:          types.StringUnknown(),
		Name:        types.StringValue("catalog"),
		InstanceID:  types.StringValue(testInstanceID),
		Description: types.StringValue("The shop's customers and their orders."),
		Entities:    cmEntities(),
		Tools:       types.ListUnknown(types.StringType),
		MCPURL:      types.StringUnknown(),
		CreatedAt:   types.StringUnknown(),
	}
}

func TestContextModelSchema(t *testing.T) {
	sch := schemaOf(t, NewContextModelResource())
	checkDescribed(t, sch)
	checkReplacement(t, sch, []string{"name", "instance_id"}, []string{"description", "entities"})
	checkNameRule(t, sch.Attributes["name"].(rschema.StringAttribute))
	if !strings.Contains(sch.MarkdownDescription, "agent key") || !strings.Contains(sch.MarkdownDescription, "records themselves stay") {
		t.Error("the description must say what destroying the model deletes, and what it keeps")
	}
}

func TestContextModelLifecycle(t *testing.T) {
	f := contextFake(t)
	r := &contextModelResource{client: f.client()}
	sch := schemaOf(t, r)

	st, d := doCreate(t, r, sch, cmPlan())
	failOn(t, d)
	put := f.callsTo("PUT", cmPath)
	if len(put) != 1 {
		t.Fatalf("create sent %d PUTs", len(put))
	}
	b := put[0].Body
	ents := b["entities"].([]any)
	order := ents[1].(map[string]any)
	if b["instance_id"] != testInstanceID || len(ents) != 2 || order["storage"] != "json" || order["id_field"] != "order_id" {
		t.Errorf("create sent %v", b)
	}
	customerFields := ents[0].(map[string]any)["fields"].([]any)
	if _, ok := customerFields[1].(map[string]any)["description"]; ok {
		t.Error("an unset field description must not be sent")
	}
	if rels := order["relationships"].([]any); len(rels) != 0 {
		t.Errorf("relationships = %v", rels)
	}

	m := getModel[contextModelModel](t, st)
	if m.ID.ValueString() != "catalog" || m.MCPURL.ValueString() != f.srv.URL+"/context/mcp" || len(m.Tools.Elements()) != 2 {
		t.Errorf("state after create: %+v", m)
	}
	// The trimmed description reads back as configured: no diff, no
	// "provider produced an unexpected new value".
	if got := m.Entities[0].Description.ValueString(); got != "A customer of the shop.  " {
		t.Errorf("description spelling lost: %q", got)
	}

	// Refresh: unchanged.
	st, d = doRead(t, r, st)
	failOn(t, d)
	if got := getModel[contextModelModel](t, st); got.Entities[0].Description.ValueString() != "A customer of the shop.  " ||
		got.Entities[1].Fields[1].Type.ValueString() != "numeric" || !got.Entities[0].Fields[1].Description.IsNull() {
		t.Errorf("read: %+v", got)
	}

	// Update in place: a field changes type, a third entity is added.
	next := getModel[contextModelModel](t, st)
	next.Entities[1].Fields[1].Type = types.StringValue("tag")
	next.Entities = append(next.Entities, contextEntityModel{
		Name: types.StringValue("ticket"), Description: types.StringValue("A support ticket."), Prefix: types.StringValue("ticket:"),
		Storage: types.StringValue("hash"), IDField: types.StringValue("id"),
		Fields:        []contextFieldModel{{Name: types.StringValue("subject"), Type: types.StringValue("text"), Description: types.StringNull()}},
		Relationships: []contextRelationshipModel{},
	})
	next.Tools = types.ListUnknown(types.StringType)
	st, d = doUpdate(t, r, sch, st, next)
	failOn(t, d)
	if put = f.callsTo("PUT", cmPath); len(put) != 2 || len(put[1].Body["entities"].([]any)) != 3 {
		t.Fatalf("update sent %v", put)
	}
	if got := getModel[contextModelModel](t, st); len(got.Tools.Elements()) != 3 || got.Entities[1].Fields[1].Type.ValueString() != "tag" {
		t.Errorf("state after update: %+v", got)
	}

	failOn(t, doDelete(t, r, st))
	if len(f.callsTo("DELETE", cmPath)) != 1 {
		t.Error("delete did not call DELETE")
	}
	failOn(t, doDelete(t, r, st))
	st, d = doRead(t, r, st)
	failOn(t, d)
	if !st.Raw.IsNull() {
		t.Error("a model that is gone must leave state")
	}
}

func TestContextModelCreateRefusesAnExistingModel(t *testing.T) {
	f := contextFake(t)
	f.putHook(cmPath, map[string]any{"instance_id": testInstanceID, "entities": []any{}})
	r := &contextModelResource{client: f.client()}
	_, d := doCreate(t, r, schemaOf(t, r), cmPlan())
	expectError(t, d, "already exists", "terraform import kividb_context_model.<name> catalog")
}

func TestContextModelImport(t *testing.T) {
	f := contextFake(t)
	r := &contextModelResource{client: f.client()}
	sch := schemaOf(t, r)
	f.putHook(cmPath, map[string]any{
		"instance_id": testInstanceID, "description": "made in the console",
		"entities": []any{map[string]any{
			"name": "customer", "description": "A customer.", "prefix": "c:",
			"fields": []any{map[string]any{"name": "tenant", "type": "tag"}},
		}},
	})

	st, d := doImport(t, r, sch, "catalog")
	failOn(t, d)
	st, d = doRead(t, r, st)
	failOn(t, d)
	m := getModel[contextModelModel](t, st)
	if m.InstanceID.ValueString() != testInstanceID || m.Description.ValueString() != "made in the console" || len(m.Entities) != 1 ||
		m.Entities[0].Storage.ValueString() != "hash" || m.Entities[0].IDField.ValueString() != "id" ||
		m.Entities[0].Relationships == nil || len(m.Entities[0].Relationships) != 0 {
		t.Errorf("imported: %+v", m)
	}

	_, d = doImport(t, r, sch, "Not A Name")
	expectError(t, d, "Import a context model by its name")
}

func TestContextModelRefusalsKeepTheServersWords(t *testing.T) {
	f := contextFake(t)
	r := &contextModelResource{client: f.client()}
	sch := schemaOf(t, r)

	f.fail("PUT", cmPath, 409, `{"error":{"code":"instance_not_running","message":"The database must be running."}}`)
	_, d := doCreate(t, r, sch, cmPlan())
	expectError(t, d, "The database must be running.", "Start the database")

	f.fail("PUT", cmPath, 400, `{"error":{"code":"bad_request","issues":{"formErrors":[],"fieldErrors":{"entities":["order needs a tag or numeric field named customer_id"]}}}}`)
	_, d = doCreate(t, r, sch, cmPlan())
	expectError(t, d, "entities: order needs a tag or numeric field named customer_id")
}

// The model is saved before its indexes are built. When only the indexes fail,
// the model exists, so it must be in state: otherwise the next create would
// refuse it as already existing.
func TestContextModelSavedWithoutIndexesStaysInState(t *testing.T) {
	f := contextFake(t)
	r := &contextModelResource{client: f.client()}
	sch := schemaOf(t, r)
	inner := f.putHook
	f.putHook = func(path string, b map[string]any) (int, any) {
		inner(path, b)
		return 502, map[string]any{"error": map[string]any{"code": "index_failed", "message": "Saved, but the indexes could not be built: timeout"}}
	}
	st, d := doCreate(t, r, sch, cmPlan())
	expectError(t, d, "Saved, but the indexes could not be built: timeout")
	if st.Raw.IsNull() {
		t.Fatal("a model the API saved must be kept in state")
	}
	if m := getModel[contextModelModel](t, st); m.Name.ValueString() != "catalog" {
		t.Errorf("state: %+v", m)
	}
}
