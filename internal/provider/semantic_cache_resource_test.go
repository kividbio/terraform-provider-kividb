package provider

import (
	"strings"
	"testing"

	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const scPath = "/instances/" + testInstanceID + "/semantic-cache/answers"

// semanticCacheFake answers PUT the way routes/semantic-cache.ts does: the
// settings come back without the key, model and base_url are dropped where the
// provider does not use them, and the defaults are filled in.
func semanticCacheFake(t *testing.T) *fakeAI {
	f := newFakeAI(t)
	f.putHook = func(path string, b map[string]any) (int, any) {
		status := 201
		if f.get(path) != nil {
			status = 200
		}
		ns := map[string]any{
			"name": "answers", "provider": b["provider"], "model": b["model"], "base_url": b["base_url"],
			"has_api_key": b["api_key"] != nil, "dimensions": b["dimensions"],
			"threshold": 0.6, "default_ttl_seconds": 86400,
			"created_at": "2026-10-25T10:00:00.000Z", "updated_at": "2026-10-25T10:00:00.000Z",
		}
		if b["provider"] == "none" {
			ns["model"], ns["has_api_key"] = nil, false
		}
		if b["provider"] != "openai_compatible" {
			ns["base_url"] = nil
		}
		if v, ok := b["threshold"]; ok {
			ns["threshold"] = v
		}
		if v, ok := b["default_ttl_seconds"]; ok {
			ns["default_ttl_seconds"] = v
		}
		f.set(path, map[string]any{"namespace": ns, "stats": map[string]any{"entries": 3}})
		return status, map[string]any{"namespace": ns}
	}
	return f
}

func scPlan() semanticCacheModel {
	return semanticCacheModel{
		ID:                types.StringUnknown(),
		InstanceID:        types.StringValue(testInstanceID),
		Name:              types.StringValue("answers"),
		EmbedProvider:     types.StringValue("openai"),
		EmbedModel:        types.StringValue("text-embedding-3-small"),
		EmbedBaseURL:      types.StringNull(),
		EmbedAPIKey:       types.StringValue("sk-secret"),
		HasEmbedAPIKey:    types.BoolUnknown(),
		Dimensions:        types.Int64Value(1536),
		Threshold:         types.Float64Value(0.6),
		DefaultTTLSeconds: types.Int64Value(86400),
		CreatedAt:         types.StringUnknown(),
	}
}

func TestSemanticCacheSchema(t *testing.T) {
	sch := schemaOf(t, NewSemanticCacheResource())
	checkDescribed(t, sch)
	checkReplacement(t, sch,
		[]string{"instance_id", "name"},
		[]string{"embed_provider", "embed_model", "embed_base_url", "embed_api_key", "dimensions", "threshold", "default_ttl_seconds"})
	if !sch.Attributes["embed_api_key"].IsSensitive() {
		t.Error("embed_api_key must be sensitive")
	}
	checkNameRule(t, sch.Attributes["name"].(rschema.StringAttribute))
}

// checkNameRule pins the API's name pattern, ^[a-z0-9][a-z0-9_-]{0,62}$.
func checkNameRule(t *testing.T, a rschema.StringAttribute) {
	t.Helper()
	for _, good := range []string{"a", "0", "answers", "my-cache_2", strings.Repeat("a", 63)} {
		if d := runStringValidators(a.Validators, good); d.HasError() {
			t.Errorf("%q must be accepted: %v", good, d)
		}
	}
	for _, bad := range []string{"", "-a", "_a", "Answers", "a b", "a.b", strings.Repeat("a", 64)} {
		if d := runStringValidators(a.Validators, bad); !d.HasError() {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestSemanticCacheLifecycle(t *testing.T) {
	f := semanticCacheFake(t)
	r := &semanticCacheResource{client: f.client()}
	sch := schemaOf(t, r)

	// Create: looks first, then PUTs the whole settings.
	st, d := doCreate(t, r, sch, scPlan())
	failOn(t, d)
	if len(f.callsTo("GET", scPath)) != 1 {
		t.Error("create must check the namespace does not exist yet")
	}
	put := f.callsTo("PUT", scPath)
	if len(put) != 1 {
		t.Fatalf("create sent %d PUTs", len(put))
	}
	b := put[0].Body
	if b["provider"] != "openai" || b["model"] != "text-embedding-3-small" || b["api_key"] != "sk-secret" ||
		b["dimensions"] != float64(1536) || b["threshold"] != 0.6 || b["default_ttl_seconds"] != float64(86400) {
		t.Errorf("create sent %v", b)
	}
	if _, ok := b["base_url"]; ok {
		t.Errorf("an unset base_url must not be sent: %v", b)
	}
	m := getModel[semanticCacheModel](t, st)
	if m.ID.ValueString() != testInstanceID+"/answers" || !m.HasEmbedAPIKey.ValueBool() ||
		m.EmbedAPIKey.ValueString() != "sk-secret" || m.CreatedAt.ValueString() == "" {
		t.Errorf("state after create: %+v", m)
	}

	// Read: the key is never returned, so the one in state is kept.
	st, d = doRead(t, r, st)
	failOn(t, d)
	if got := getModel[semanticCacheModel](t, st); got.EmbedAPIKey.ValueString() != "sk-secret" {
		t.Errorf("read lost the API key: %v", got.EmbedAPIKey)
	}

	// Update in place: one PUT, to the same URL, with the new settings.
	next := scPlan()
	next.ID, next.CreatedAt = m.ID, m.CreatedAt
	next.Threshold = types.Float64Value(0.75)
	next.DefaultTTLSeconds = types.Int64Value(3600)
	next.EmbedAPIKey = types.StringNull() // left out: the stored key is kept
	st, d = doUpdate(t, r, sch, st, next)
	failOn(t, d)
	put = f.callsTo("PUT", scPath)
	if len(put) != 2 || put[1].Body["threshold"] != 0.75 || put[1].Body["default_ttl_seconds"] != float64(3600) {
		t.Fatalf("update sent %v", put)
	}
	if _, ok := put[1].Body["api_key"]; ok {
		t.Error("a key left out of the configuration must not be sent")
	}
	if got := getModel[semanticCacheModel](t, st); got.Threshold.ValueFloat64() != 0.75 || got.DefaultTTLSeconds.ValueInt64() != 3600 {
		t.Errorf("state after update: %+v", got)
	}

	// Delete.
	failOn(t, doDelete(t, r, st))
	if len(f.callsTo("DELETE", scPath)) != 1 {
		t.Error("delete did not call DELETE")
	}
	// Gone already is not a failure.
	failOn(t, doDelete(t, r, st))

	// Read after it is gone: removed from state.
	st, d = doRead(t, r, st)
	failOn(t, d)
	if !st.Raw.IsNull() {
		t.Error("a namespace that is gone must leave state")
	}
}

func TestSemanticCacheCreateRefusesAnExistingNamespace(t *testing.T) {
	f := semanticCacheFake(t)
	f.set(scPath, map[string]any{"namespace": map[string]any{"name": "answers", "provider": "none", "dimensions": 8}})
	r := &semanticCacheResource{client: f.client()}
	_, d := doCreate(t, r, schemaOf(t, r), scPlan())
	expectError(t, d, "already exists", "terraform import", testInstanceID+"/answers")
	if len(f.callsTo("PUT", scPath)) != 0 {
		t.Error("an existing namespace must not be overwritten")
	}
}

func TestSemanticCacheImport(t *testing.T) {
	f := semanticCacheFake(t)
	f.set(scPath, map[string]any{"namespace": map[string]any{
		"name": "answers", "provider": "none", "model": nil, "base_url": nil, "has_api_key": false,
		"dimensions": 384, "threshold": 0.8, "default_ttl_seconds": 600, "created_at": "2026-10-01T00:00:00Z",
	}, "stats": nil})
	r := &semanticCacheResource{client: f.client()}
	sch := schemaOf(t, r)

	st, d := doImport(t, r, sch, testInstanceID+"/answers")
	failOn(t, d)
	st, d = doRead(t, r, st)
	failOn(t, d)
	m := getModel[semanticCacheModel](t, st)
	if m.InstanceID.ValueString() != testInstanceID || m.Name.ValueString() != "answers" || m.EmbedProvider.ValueString() != "none" ||
		m.Dimensions.ValueInt64() != 384 || m.Threshold.ValueFloat64() != 0.8 || m.DefaultTTLSeconds.ValueInt64() != 600 ||
		!m.EmbedModel.IsNull() || !m.EmbedAPIKey.IsNull() {
		t.Errorf("imported: %+v", m)
	}

	_, d = doImport(t, r, sch, "answers")
	expectError(t, d, "<instance_id>/<name>")
}

func TestSemanticCacheRefusalsKeepTheServersWords(t *testing.T) {
	f := semanticCacheFake(t)
	r := &semanticCacheResource{client: f.client()}
	sch := schemaOf(t, r)
	st, d := doCreate(t, r, sch, scPlan())
	failOn(t, d)

	// Changing the size of a namespace that holds entries.
	f.fail("PUT", scPath, 409, `{"error":{"code":"not_empty","message":"This namespace holds 3 entries of 1536 dimensions; flush it before changing the size."}}`)
	next := getModel[semanticCacheModel](t, st)
	next.Dimensions = types.Int64Value(256)
	_, d = doUpdate(t, r, sch, st, next)
	expectError(t, d, "This namespace holds 3 entries of 1536 dimensions; flush it before changing the size.", "-replace")

	// A schema refusal names the field.
	f.fail("PUT", scPath, 400, `{"error":{"code":"bad_request","issues":{"formErrors":[],"fieldErrors":{"model":["model is required for this provider"]}}}}`)
	_, d = doUpdate(t, r, sch, st, next)
	expectError(t, d, "model: model is required for this provider")

	// Deleting from a stopped database.
	f.fail("DELETE", scPath, 409, `{"error":{"code":"instance_not_running","message":"The database must be running to use its semantic cache."}}`)
	expectError(t, doDelete(t, r, st), "The database must be running to use its semantic cache.", "Start the database")
}

func TestSemanticCacheValidatesTheProvider(t *testing.T) {
	r := &semanticCacheResource{}
	sch := schemaOf(t, r)

	none := scPlan()
	none.EmbedProvider = types.StringValue("none")
	none.EmbedAPIKey = types.StringNull()
	expectError(t, doValidate(t, r, sch, none), "embed_model")

	none.EmbedModel = types.StringNull()
	failOn(t, doValidate(t, r, sch, none))

	compat := scPlan()
	compat.EmbedProvider = types.StringValue("openai_compatible")
	expectError(t, doValidate(t, r, sch, compat), "embed_base_url")
	compat.EmbedBaseURL = types.StringValue("https://embed.example.com/v1")
	failOn(t, doValidate(t, r, sch, compat))

	noModel := scPlan()
	noModel.EmbedModel = types.StringNull()
	expectError(t, doValidate(t, r, sch, noModel), "embed_model")

	baseWithOpenAI := scPlan()
	baseWithOpenAI.EmbedBaseURL = types.StringValue("https://x.example.com")
	expectError(t, doValidate(t, r, sch, baseWithOpenAI), "embed_base_url")
}
