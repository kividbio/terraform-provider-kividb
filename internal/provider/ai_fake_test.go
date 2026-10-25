package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A fake of the AI routes, enough to drive each resource through its life:
// it keeps what was PUT, answers GETs with it the way the API shapes them, and
// records every request. A test can make the next matching request fail with
// a given status and body, to check the refusal reaches the operator.

const testInstanceID = "790df3ae-c064-45f5-b924-9ed89471fd72"

type fakeCall struct {
	Method string
	Path   string
	Body   map[string]any
}

type fakeAI struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	calls []fakeCall
	// path -> the object the GET returns (already in the API's response shape)
	objects map[string]map[string]any
	// "METHOD path" -> {status, body} for the next request that matches
	failNext map[string][2]string
	// answer a PUT with this, instead of echoing the body back
	putHook func(path string, body map[string]any) (int, any)
	// answers any request it returns handled=true for, before everything else
	hook func(method, path string, body map[string]any) (status int, out any, handled bool)
}

func newFakeAI(t *testing.T) *fakeAI {
	f := &fakeAI{t: t, objects: map[string]map[string]any{}, failNext: map[string][2]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAI) client() *Client { return NewClient(f.srv.URL, "kvdb_test") }

func (f *fakeAI) fail(method, path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[method+" "+path] = [2]string{itoa(status), body}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (f *fakeAI) set(path string, obj map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[path] = obj
}

func (f *fakeAI) get(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[path]
}

func (f *fakeAI) callsTo(method, path string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Method == method && c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeAI) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if r.Header.Get("Authorization") != "Bearer kvdb_test" {
		f.t.Errorf("%s %s sent no API key", r.Method, r.URL.Path)
	}
	path := r.URL.EscapedPath()

	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Method: r.Method, Path: path, Body: body})
	failure, failing := f.failNext[r.Method+" "+path]
	if failing {
		delete(f.failNext, r.Method+" "+path)
	}
	obj, exists := f.objects[path]
	hook := f.putHook
	anyHook := f.hook
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if failing {
		var status int
		_ = json.Unmarshal([]byte(failure[0]), &status)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(failure[1]))
		return
	}
	if anyHook != nil {
		if status, out, handled := anyHook(r.Method, path, body); handled {
			w.WriteHeader(status)
			if out != nil {
				_ = json.NewEncoder(w).Encode(out)
			}
			return
		}
	}
	notFound := func() {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"No such thing."}}`))
	}

	switch r.Method {
	case http.MethodGet:
		if !exists {
			notFound()
			return
		}
		_ = json.NewEncoder(w).Encode(obj)
	case http.MethodPut:
		if hook == nil {
			f.t.Fatalf("PUT %s with no putHook", path)
		}
		status, out := hook(path, body)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	case http.MethodDelete:
		if !exists {
			notFound()
			return
		}
		f.mu.Lock()
		delete(f.objects, path)
		f.mu.Unlock()
		w.WriteHeader(204)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, path)
		w.WriteHeader(500)
	}
}

// --- driving a resource -------------------------------------------------------

func schemaOf(t *testing.T, r resource.Resource) rschema.Schema {
	t.Helper()
	var res resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &res)
	failOn(t, res.Diagnostics)
	if d := res.Schema.ValidateImplementation(context.Background()); d.HasError() {
		t.Fatalf("schema is not valid: %v", d)
	}
	return res.Schema
}

func nullRaw(sch rschema.Schema) tftypes.Value {
	return tftypes.NewValue(sch.Type().TerraformType(context.Background()), nil)
}

func planOf(t *testing.T, sch rschema.Schema, model any) tfsdk.Plan {
	t.Helper()
	p := tfsdk.Plan{Schema: sch, Raw: nullRaw(sch)}
	failOn(t, p.Set(context.Background(), model))
	return p
}

func stateOf(t *testing.T, sch rschema.Schema, model any) tfsdk.State {
	t.Helper()
	s := tfsdk.State{Schema: sch, Raw: nullRaw(sch)}
	failOn(t, s.Set(context.Background(), model))
	return s
}

func configOf(t *testing.T, sch rschema.Schema, model any) tfsdk.Config {
	t.Helper()
	s := stateOf(t, sch, model)
	return tfsdk.Config{Schema: sch, Raw: s.Raw}
}

func doCreate(t *testing.T, r resource.Resource, sch rschema.Schema, plan any) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	res := resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: nullRaw(sch)}}
	r.Create(context.Background(), resource.CreateRequest{Plan: planOf(t, sch, plan)}, &res)
	return res.State, res.Diagnostics
}

func doRead(t *testing.T, r resource.Resource, st tfsdk.State) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	res := resource.ReadResponse{State: st}
	r.Read(context.Background(), resource.ReadRequest{State: st}, &res)
	return res.State, res.Diagnostics
}

func doUpdate(t *testing.T, r resource.Resource, sch rschema.Schema, prior tfsdk.State, plan any) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	res := resource.UpdateResponse{State: prior}
	r.Update(context.Background(), resource.UpdateRequest{Plan: planOf(t, sch, plan), State: prior}, &res)
	return res.State, res.Diagnostics
}

func doDelete(t *testing.T, r resource.Resource, st tfsdk.State) diag.Diagnostics {
	t.Helper()
	res := resource.DeleteResponse{State: st}
	r.Delete(context.Background(), resource.DeleteRequest{State: st}, &res)
	return res.Diagnostics
}

func doImport(t *testing.T, r resource.ResourceWithImportState, sch rschema.Schema, id string) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	res := resource.ImportStateResponse{State: tfsdk.State{Schema: sch, Raw: nullRaw(sch)}}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: id}, &res)
	return res.State, res.Diagnostics
}

func doValidate(t *testing.T, r resource.ResourceWithValidateConfig, sch rschema.Schema, cfg any) diag.Diagnostics {
	t.Helper()
	res := resource.ValidateConfigResponse{}
	r.ValidateConfig(context.Background(), resource.ValidateConfigRequest{Config: configOf(t, sch, cfg)}, &res)
	return res.Diagnostics
}

func getModel[T any](t *testing.T, st tfsdk.State) T {
	t.Helper()
	var m T
	failOn(t, st.Get(context.Background(), &m))
	return m
}

// diagText is every diagnostic's summary and detail, for checking that the
// server's own words reached the operator.
func diagText(d diag.Diagnostics) string {
	var b strings.Builder
	for _, x := range d {
		b.WriteString(x.Summary())
		b.WriteString(": ")
		b.WriteString(x.Detail())
		b.WriteString("\n")
	}
	return b.String()
}

func expectError(t *testing.T, d diag.Diagnostics, contains ...string) {
	t.Helper()
	if !d.HasError() {
		t.Fatalf("expected an error, got none")
	}
	text := diagText(d)
	for _, c := range contains {
		if !strings.Contains(text, c) {
			t.Errorf("diagnostics do not mention %q:\n%s", c, text)
		}
	}
}

// requiresReplace reports whether an attribute's plan modifiers include
// RequiresReplace, by what the modifier says about itself.
func requiresReplace(t *testing.T, sch rschema.Schema, name string) bool {
	t.Helper()
	var descs []string
	switch a := sch.Attributes[name].(type) {
	case rschema.StringAttribute:
		for _, m := range a.PlanModifiers {
			descs = append(descs, m.Description(context.Background()))
		}
	case rschema.Int64Attribute:
		for _, m := range a.PlanModifiers {
			descs = append(descs, m.Description(context.Background()))
		}
	case rschema.Float64Attribute:
		for _, m := range a.PlanModifiers {
			descs = append(descs, m.Description(context.Background()))
		}
	case rschema.BoolAttribute:
		for _, m := range a.PlanModifiers {
			descs = append(descs, m.Description(context.Background()))
		}
	case rschema.ListAttribute:
		for _, m := range a.PlanModifiers {
			descs = append(descs, m.Description(context.Background()))
		}
	case rschema.MapAttribute:
		for _, m := range a.PlanModifiers {
			descs = append(descs, m.Description(context.Background()))
		}
	case rschema.ListNestedAttribute:
		for _, m := range a.PlanModifiers {
			descs = append(descs, m.Description(context.Background()))
		}
	case nil:
		t.Fatalf("no attribute %q", name)
	}
	for _, d := range descs {
		if strings.Contains(strings.ToLower(d), "destroy and recreate") || strings.Contains(strings.ToLower(d), "replace") {
			return true
		}
	}
	return false
}

// checkReplacement pins which attributes force a new resource and which change
// in place.
func checkReplacement(t *testing.T, sch rschema.Schema, replace, inPlace []string) {
	t.Helper()
	for _, n := range replace {
		if !requiresReplace(t, sch, n) {
			t.Errorf("%s must force replacement", n)
		}
	}
	for _, n := range inPlace {
		if requiresReplace(t, sch, n) {
			t.Errorf("%s must change in place", n)
		}
	}
}

// every MarkdownDescription is written, top level and nested.
func checkDescribed(t *testing.T, sch rschema.Schema) {
	t.Helper()
	if sch.MarkdownDescription == "" {
		t.Error("the resource has no description")
	}
	var walk func(prefix string, attrs map[string]rschema.Attribute)
	walk = func(prefix string, attrs map[string]rschema.Attribute) {
		for name, a := range attrs {
			if a.GetMarkdownDescription() == "" {
				t.Errorf("%s%s has no description", prefix, name)
			}
			if n, ok := a.(rschema.ListNestedAttribute); ok {
				walk(prefix+name+".", n.NestedObject.Attributes)
			}
		}
	}
	walk("", sch.Attributes)
}
