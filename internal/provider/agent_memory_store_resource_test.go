package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const amPath = "/instances/" + testInstanceID + "/agent-memory/support"

// agentMemoryFake answers PUT the way routes/agent-memory.ts does: no keys in
// the answer, models and base URLs dropped where unused, defaults filled in,
// and memory_types the built-in two plus the custom ones.
func agentMemoryFake(t *testing.T) *fakeAI {
	f := newFakeAI(t)
	f.putHook = func(path string, b map[string]any) (int, any) {
		status := 201
		if f.get(path) != nil {
			status = 200
		}
		def := func(k string, v any) any {
			if x, ok := b[k]; ok {
				return x
			}
			return v
		}
		custom, _ := b["custom_types"].([]any)
		memTypes := []any{"semantic", "episodic"}
		for _, c := range custom {
			memTypes = append(memTypes, c.(map[string]any)["name"])
		}
		s := map[string]any{
			"name":           "support",
			"embed_provider": b["embed_provider"], "embed_model": b["embed_model"], "embed_base_url": nil,
			"has_embed_api_key": b["embed_api_key"] != nil, "dimensions": b["dimensions"],
			"llm_provider": b["llm_provider"], "llm_model": b["llm_model"], "llm_base_url": nil,
			"has_llm_api_key":          b["llm_api_key"] != nil,
			"extraction_delay_seconds": def("extraction_delay_seconds", 300), "summarize_after": def("summarize_after", 40),
			"keep_recent": def("keep_recent", 12), "session_ttl_seconds": def("session_ttl_seconds", 3600),
			"memory_ttl_seconds": def("memory_ttl_seconds", 31536000), "dedupe_threshold": def("dedupe_threshold", 0.92),
			"custom_types": custom, "memory_types": memTypes, "redact_pii": def("redact_pii", true),
			"last_extraction_at": nil, "last_extraction_error": nil, "memories_extracted": 0,
			"created_at": "2026-10-25T10:00:00.000Z", "updated_at": "2026-10-25T10:00:00.000Z",
		}
		f.set(path, map[string]any{"store": s, "stats": nil})
		return status, map[string]any{"store": s}
	}
	return f
}

func amCustomTypes(t *testing.T, names ...string) types.List {
	t.Helper()
	vals := []attr.Value{}
	for _, n := range names {
		vals = append(vals, types.ObjectValueMust(agentMemoryCustomTypeAttrTypes, map[string]attr.Value{
			"name": types.StringValue(n), "description": types.StringValue("Which product the user is asking about."),
		}))
	}
	return types.ListValueMust(types.ObjectType{AttrTypes: agentMemoryCustomTypeAttrTypes}, vals)
}

// The plan Terraform makes for a store with no hosted models: every default
// filled in, the computed attributes unknown.
func amPlan(t *testing.T) agentMemoryStoreModel {
	return agentMemoryStoreModel{
		ID:                     types.StringUnknown(),
		InstanceID:             types.StringValue(testInstanceID),
		Name:                   types.StringValue("support"),
		EmbedProvider:          types.StringValue("none"),
		EmbedModel:             types.StringNull(),
		EmbedBaseURL:           types.StringNull(),
		EmbedAPIKey:            types.StringNull(),
		HasEmbedAPIKey:         types.BoolUnknown(),
		Dimensions:             types.Int64Value(384),
		LLMProvider:            types.StringValue("none"),
		LLMModel:               types.StringNull(),
		LLMBaseURL:             types.StringNull(),
		LLMAPIKey:              types.StringNull(),
		HasLLMAPIKey:           types.BoolUnknown(),
		ExtractionDelaySeconds: types.Int64Value(300),
		SummarizeAfter:         types.Int64Value(40),
		KeepRecent:             types.Int64Value(12),
		SessionTTLSeconds:      types.Int64Value(3600),
		MemoryTTLSeconds:       types.Int64Value(31536000),
		DedupeThreshold:        types.Float64Value(0.92),
		CustomTypes:            amCustomTypes(t),
		MemoryTypes:            types.ListUnknown(types.StringType),
		RedactPII:              types.BoolValue(true),
		CreatedAt:              types.StringUnknown(),
	}
}

func TestAgentMemoryStoreSchema(t *testing.T) {
	sch := schemaOf(t, NewAgentMemoryStoreResource())
	checkDescribed(t, sch)
	checkReplacement(t, sch,
		[]string{"instance_id", "name"},
		[]string{"embed_provider", "embed_model", "embed_api_key", "dimensions", "llm_provider", "llm_model", "llm_api_key",
			"extraction_delay_seconds", "summarize_after", "keep_recent", "session_ttl_seconds", "memory_ttl_seconds",
			"dedupe_threshold", "custom_types", "redact_pii"})
	for _, k := range []string{"embed_api_key", "llm_api_key"} {
		if !sch.Attributes[k].IsSensitive() {
			t.Errorf("%s must be sensitive", k)
		}
	}
	checkNameRule(t, sch.Attributes["name"].(rschema.StringAttribute))
}

func TestAgentMemoryStoreLifecycle(t *testing.T) {
	f := agentMemoryFake(t)
	r := &agentMemoryStoreResource{client: f.client()}
	sch := schemaOf(t, r)

	st, d := doCreate(t, r, sch, amPlan(t))
	failOn(t, d)
	put := f.callsTo("PUT", amPath)
	if len(put) != 1 {
		t.Fatalf("create sent %d PUTs", len(put))
	}
	b := put[0].Body
	if b["embed_provider"] != "none" || b["llm_provider"] != "none" || b["dimensions"] != float64(384) ||
		b["redact_pii"] != true || b["keep_recent"] != float64(12) {
		t.Errorf("create sent %v", b)
	}
	if ct, ok := b["custom_types"].([]any); !ok || len(ct) != 0 {
		t.Errorf("custom_types must be sent as an empty list, sent %v", b["custom_types"])
	}
	for _, k := range []string{"embed_model", "embed_api_key", "llm_model", "llm_api_key"} {
		if _, ok := b[k]; ok {
			t.Errorf("unset %s must not be sent", k)
		}
	}
	m := getModel[agentMemoryStoreModel](t, st)
	var memTypes []string
	failOn(t, m.MemoryTypes.ElementsAs(context.Background(), &memTypes, false))
	if m.ID.ValueString() != testInstanceID+"/support" || len(memTypes) != 2 || m.HasLLMAPIKey.ValueBool() {
		t.Errorf("state after create: %+v", m)
	}

	// Update in place: hosted models, a custom memory type, longer sessions.
	next := m
	next.LLMProvider = types.StringValue("anthropic")
	next.LLMModel = types.StringValue("claude-haiku-4-5")
	next.LLMAPIKey = types.StringValue("sk-ant-secret")
	next.SessionTTLSeconds = types.Int64Value(7200)
	next.CustomTypes = amCustomTypes(t, "product")
	next.MemoryTypes = types.ListUnknown(types.StringType)
	next.HasLLMAPIKey = types.BoolUnknown()
	st, d = doUpdate(t, r, sch, st, next)
	failOn(t, d)
	put = f.callsTo("PUT", amPath)
	if len(put) != 2 {
		t.Fatalf("update sent %d PUTs in all", len(put))
	}
	b = put[1].Body
	if b["llm_provider"] != "anthropic" || b["llm_model"] != "claude-haiku-4-5" || b["llm_api_key"] != "sk-ant-secret" ||
		b["session_ttl_seconds"] != float64(7200) {
		t.Errorf("update sent %v", b)
	}
	m = getModel[agentMemoryStoreModel](t, st)
	memTypes = nil
	failOn(t, m.MemoryTypes.ElementsAs(context.Background(), &memTypes, false))
	if len(memTypes) != 3 || memTypes[2] != "product" || !m.HasLLMAPIKey.ValueBool() || m.LLMAPIKey.ValueString() != "sk-ant-secret" {
		t.Errorf("state after update: %+v", m)
	}

	// Read keeps the key, which the API never returns.
	st, d = doRead(t, r, st)
	failOn(t, d)
	if got := getModel[agentMemoryStoreModel](t, st); got.LLMAPIKey.ValueString() != "sk-ant-secret" || len(got.CustomTypes.Elements()) != 1 {
		t.Errorf("read: %+v", got)
	}

	failOn(t, doDelete(t, r, st))
	if len(f.callsTo("DELETE", amPath)) != 1 {
		t.Error("delete did not call DELETE")
	}
	failOn(t, doDelete(t, r, st))
	st, d = doRead(t, r, st)
	failOn(t, d)
	if !st.Raw.IsNull() {
		t.Error("a store that is gone must leave state")
	}
}

func TestAgentMemoryStoreCreateRefusesAnExistingStore(t *testing.T) {
	f := agentMemoryFake(t)
	f.set(amPath, map[string]any{"store": map[string]any{"name": "support"}})
	r := &agentMemoryStoreResource{client: f.client()}
	_, d := doCreate(t, r, schemaOf(t, r), amPlan(t))
	expectError(t, d, "already exists", testInstanceID+"/support")
	if len(f.callsTo("PUT", amPath)) != 0 {
		t.Error("an existing store must not be overwritten")
	}
}

func TestAgentMemoryStoreImport(t *testing.T) {
	f := agentMemoryFake(t)
	r := &agentMemoryStoreResource{client: f.client()}
	sch := schemaOf(t, r)
	// Made elsewhere, with an OpenAI embedding key.
	f.putHook(amPath, map[string]any{
		"embed_provider": "openai", "embed_model": "text-embedding-3-small", "embed_api_key": "x", "dimensions": 1536,
		"llm_provider": "none", "custom_types": []any{map[string]any{"name": "ticket", "description": "A support ticket the user mentions."}},
	})

	st, d := doImport(t, r, sch, testInstanceID+"/support")
	failOn(t, d)
	st, d = doRead(t, r, st)
	failOn(t, d)
	m := getModel[agentMemoryStoreModel](t, st)
	if m.EmbedProvider.ValueString() != "openai" || !m.HasEmbedAPIKey.ValueBool() || !m.EmbedAPIKey.IsNull() ||
		m.Dimensions.ValueInt64() != 1536 || len(m.CustomTypes.Elements()) != 1 || m.SummarizeAfter.ValueInt64() != 40 {
		t.Errorf("imported: %+v", m)
	}
	_, d = doImport(t, r, sch, testInstanceID)
	expectError(t, d, "<instance_id>/<name>")
}

func TestAgentMemoryStoreRefusalsKeepTheServersWords(t *testing.T) {
	f := agentMemoryFake(t)
	r := &agentMemoryStoreResource{client: f.client()}
	f.fail("PUT", amPath, 409, `{"error":{"code":"sharded_not_supported","message":"Agent memory is not available on sharded databases yet."}}`)
	_, d := doCreate(t, r, schemaOf(t, r), amPlan(t))
	expectError(t, d, "Agent memory is not available on sharded databases yet.")
}

func TestAgentMemoryStoreValidation(t *testing.T) {
	r := &agentMemoryStoreResource{}
	sch := schemaOf(t, r)

	cfg := amPlan(t)
	failOn(t, doValidate(t, r, sch, cfg))

	cfg.KeepRecent = types.Int64Value(40)
	expectError(t, doValidate(t, r, sch, cfg), "keep_recent")

	cfg = amPlan(t)
	cfg.SummarizeAfter = types.Int64Null()
	cfg.KeepRecent = types.Int64Value(39)
	failOn(t, doValidate(t, r, sch, cfg))

	cfg = amPlan(t)
	cfg.LLMProvider = types.StringValue("openai")
	expectError(t, doValidate(t, r, sch, cfg), "llm_model")

	cfg = amPlan(t)
	cfg.LLMAPIKey = types.StringValue("sk-unused")
	expectError(t, doValidate(t, r, sch, cfg), "llm_api_key")

	cfg = amPlan(t)
	cfg.CustomTypes = amCustomTypes(t, "product", "product")
	expectError(t, doValidate(t, r, sch, cfg), "listed twice")
}
