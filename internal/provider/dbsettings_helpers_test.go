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

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A fake KiviDB API for the database-settings resources: routes keyed by
// "METHOD /path", each answering a status and a body, and every request
// recorded so a test can check what was sent.

type apiCall struct {
	Method string
	Path   string
	Body   map[string]any
}

type apiReply struct {
	Status int
	Body   string
}

type fakeAPI struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]func(apiCall) apiReply
	calls  []apiCall
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, routes: map[string]func(apiCall) apiReply{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := apiCall{Method: r.Method, Path: r.URL.EscapedPath()}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &call.Body)
		}
		f.mu.Lock()
		f.calls = append(f.calls, call)
		h, ok := f.routes[r.Method+" "+call.Path]
		f.mu.Unlock()
		if !ok {
			t.Errorf("unexpected %s %s", r.Method, call.Path)
			w.WriteHeader(500)
			return
		}
		rep := h(call)
		if rep.Status == 0 {
			rep.Status = 200
		}
		if !strings.HasPrefix(rep.Body, "-----") {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(rep.Status)
		_, _ = w.Write([]byte(rep.Body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) on(route string, h func(apiCall) apiReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = h
}

// answer registers a route that always gives the same reply.
func (f *fakeAPI) answer(route string, status int, body string) {
	f.on(route, func(apiCall) apiReply { return apiReply{status, body} })
}

func (f *fakeAPI) client() *Client { return NewClient(f.srv.URL, "kvdb_test") }

// sent returns the requests made to one route, in order.
func (f *fakeAPI) sent(route string) []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []apiCall
	for _, c := range f.calls {
		if c.Method+" "+c.Path == route {
			out = append(out, c)
		}
	}
	return out
}

// --- driving a resource the way Terraform does ---------------------------------

func dsResourceSchema(t *testing.T, r resource.Resource) rschema.Schema {
	t.Helper()
	var res resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &res)
	failOn(t, res.Diagnostics)
	return res.Schema
}

// planned builds a plan: the given values, unknown for any computed attribute
// not given (what Terraform plans before the API has answered), null for the
// rest.
func planned(t *testing.T, sch rschema.Schema, vals map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	typ := sch.Type().TerraformType(context.Background()).(tftypes.Object)
	all := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		if v, ok := vals[name]; ok {
			all[name] = v
		} else if sch.Attributes[name].IsComputed() {
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

// stored builds state: the given values, null for the rest.
func stored(t *testing.T, sch rschema.Schema, vals map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	return objectOf(t, sch.Type().TerraformType(context.Background()), vals, false)
}

func nullState(sch rschema.Schema) tfsdk.State {
	return tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(context.Background()), nil)}
}

func dsDoCreate(t *testing.T, r resource.Resource, vals map[string]tftypes.Value) resource.CreateResponse {
	t.Helper()
	sch := dsResourceSchema(t, r)
	res := resource.CreateResponse{State: nullState(sch)}
	r.Create(context.Background(), resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: planned(t, sch, vals)},
	}, &res)
	return res
}

func dsDoRead(t *testing.T, r resource.Resource, vals map[string]tftypes.Value) resource.ReadResponse {
	t.Helper()
	sch := dsResourceSchema(t, r)
	st := tfsdk.State{Schema: sch, Raw: stored(t, sch, vals)}
	res := resource.ReadResponse{State: st}
	r.Read(context.Background(), resource.ReadRequest{State: st}, &res)
	return res
}

func dsDoUpdate(t *testing.T, r resource.Resource, prior, plan map[string]tftypes.Value) resource.UpdateResponse {
	t.Helper()
	sch := dsResourceSchema(t, r)
	st := tfsdk.State{Schema: sch, Raw: stored(t, sch, prior)}
	res := resource.UpdateResponse{State: st}
	r.Update(context.Background(), resource.UpdateRequest{
		State: st,
		Plan:  tfsdk.Plan{Schema: sch, Raw: planned(t, sch, plan)},
	}, &res)
	return res
}

func dsDoDelete(t *testing.T, r resource.Resource, vals map[string]tftypes.Value) resource.DeleteResponse {
	t.Helper()
	sch := dsResourceSchema(t, r)
	st := tfsdk.State{Schema: sch, Raw: stored(t, sch, vals)}
	res := resource.DeleteResponse{State: st}
	r.Delete(context.Background(), resource.DeleteRequest{State: st}, &res)
	return res
}

func dsDoImport(t *testing.T, r resource.ResourceWithImportState, id string) resource.ImportStateResponse {
	t.Helper()
	sch := dsResourceSchema(t, r)
	res := resource.ImportStateResponse{State: nullState(sch)}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: id}, &res)
	return res
}

// readDataSource runs a data source's Read with the given configuration.
func dsReadDataSource(t *testing.T, d datasource.DataSource, vals map[string]tftypes.Value) datasource.ReadResponse {
	t.Helper()
	sch := dataSourceSchema(t, d)
	typ := sch.Type().TerraformType(context.Background())
	cfg := tfsdk.Config{Schema: sch, Raw: objectOf(t, typ, vals, false)}
	res := datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(typ, nil)}}
	d.Read(context.Background(), datasource.ReadRequest{Config: cfg}, &res)
	return res
}

func dsStr(v string) tftypes.Value   { return tftypes.NewValue(tftypes.String, v) }
func num(v int64) tftypes.Value      { return tftypes.NewValue(tftypes.Number, v) }
func dsBoolean(v bool) tftypes.Value { return tftypes.NewValue(tftypes.Bool, v) }
func nullStr() tftypes.Value         { return tftypes.NewValue(tftypes.String, nil) }

// wantError fails unless the diagnostics hold an error whose text contains
// every one of want.
func wantError(t *testing.T, d diag.Diagnostics, want ...string) {
	t.Helper()
	if !d.HasError() {
		t.Fatalf("expected an error containing %q, got none", want)
	}
	var all []string
	for _, e := range d.Errors() {
		all = append(all, e.Summary()+": "+e.Detail())
	}
	text := strings.Join(all, "\n")
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("error does not say %q:\n%s", w, text)
		}
	}
}
