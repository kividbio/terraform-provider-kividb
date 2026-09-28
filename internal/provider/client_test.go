package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The request shapes are the contract with the control plane, so they are
// checked against what the API actually parses rather than assumed.
func TestVerbEndpointsSendWhatTheApiParses(t *testing.T) {
	var gotPath, gotMethod, gotAuth, gotIdem string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotency-Key")
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"instance_id":"i-1","id":"i-1","status":"running"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "kvdb_test_abc_secret")
	ctx := context.Background()

	if _, err := c.CreateInstance(ctx, CreateInstanceRequest{
		Tier: "essentials", Cloud: "aws", Region: "us-east-1",
		DataSizeGB: 8, ThroughputMode: "standard", ReplicaCount: 0,
	}, "tf:key"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if gotPath != "/instances" || gotMethod != http.MethodPost {
		t.Errorf("create hit %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer kvdb_test_abc_secret" {
		t.Errorf("key not sent as a bearer token: %q", gotAuth)
	}
	if gotIdem != "tf:key" {
		t.Errorf("idempotency key not sent: %q", gotIdem)
	}
	if gotBody["free_tier"] != nil {
		t.Errorf("free_tier must never be sent: the API refuses it over a key")
	}

	size := int64(32)
	mode := "high"
	if err := c.Resize(ctx, "i-1", ResizeRequest{DataSizeGB: &size, ThroughputMode: &mode}); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if gotPath != "/instances/i-1/resize" {
		t.Errorf("resize hit %s", gotPath)
	}
	if gotBody["data_size_gb"] != float64(32) || gotBody["throughput_mode"] != "high" {
		t.Errorf("resize body wrong: %v", gotBody)
	}

	// Only one field changing must not send the other as null: the server's
	// schema treats an explicit null as a value and rejects it.
	if err := c.Resize(ctx, "i-1", ResizeRequest{ThroughputMode: &mode}); err != nil {
		t.Fatalf("resize mode only: %v", err)
	}
	if _, present := gotBody["data_size_gb"]; present {
		t.Errorf("omitted field was still sent: %v", gotBody)
	}

	if err := c.ScaleReplicas(ctx, "i-1", 2); err != nil {
		t.Fatalf("replicas: %v", err)
	}
	if gotPath != "/instances/i-1/replicas" || gotBody["replica_count"] != float64(2) {
		t.Errorf("replicas wrong: %s %v", gotPath, gotBody)
	}

	// The upgrade endpoint takes a replica count, not a tier. This test used to
	// assert the opposite and passed anyway, because the stub server accepts
	// whatever it is sent -- so a body the real API answered with a 500 looked
	// correct in CI for as long as nobody pointed the provider at a real server.
	if err := c.UpgradeToPro(ctx, "i-1", 2); err != nil {
		t.Fatalf("tier: %v", err)
	}
	if gotPath != "/instances/i-1/upgrade" || gotBody["replica_count"] != float64(2) {
		t.Errorf("upgrade wrong: %s %v", gotPath, gotBody)
	}
	if _, present := gotBody["tier"]; present {
		t.Errorf("upgrade sent a tier, which the endpoint does not accept: %v", gotBody)
	}

	lua := true
	if err := c.UpgradeBinary(ctx, "i-1", BinaryRequest{KividbVersion: "1.0.4", LuaEnabled: &lua}); err != nil {
		t.Fatalf("binary: %v", err)
	}
	if gotPath != "/instances/i-1/binary" || gotBody["kividb_version"] != "1.0.4" || gotBody["lua_enabled"] != true {
		t.Errorf("binary wrong: %s %v", gotPath, gotBody)
	}
	if _, present := gotBody["tls_enabled"]; present {
		t.Errorf("unchanged flag was sent, which would apply it: %v", gotBody)
	}

	if err := c.Rename(ctx, "i-1", "new-name"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/instances/i-1" || gotBody["name"] != "new-name" {
		t.Errorf("rename wrong: %s %s %v", gotMethod, gotPath, gotBody)
	}
	if len(gotBody) != 1 {
		t.Errorf("rename must send only a name, sent: %v", gotBody)
	}
}

// A refusal the server explained must reach the operator in its own words.
func TestRefusalsKeepTheServersWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"This API key has the member role, which cannot modify this resource."}}`))
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "k").Rename(context.Background(), "i-1", "x")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("lost the API error: %T", err)
	}
	if apiErr.Code != "forbidden" || apiErr.Status != 403 {
		t.Errorf("refusal not decoded: %+v", apiErr)
	}
	if got := explain(err); got == "403" || len(got) < 40 {
		t.Errorf("explain() dropped the server's message: %q", got)
	}
}

func TestMissingInstanceIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found"}}`))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "k").GetInstance(context.Background(), "gone")
	apiErr, ok := err.(*APIError)
	if !ok || !apiErr.IsNotFound() {
		t.Fatalf("a deleted instance must be recognisable as gone, got %v", err)
	}
}

// The single-instance response is wrapped. Decoding it flat gave an Instance
// with every field empty, which surfaced as Terraform refusing an apply because
// the provider "changed" cloud from "aws" to "". Pinned here against the exact
// shape the API returns.
func TestGetInstanceUnwrapsTheResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"instance":{"id":"i-1","tier":"essentials","cloud":"aws",
			"region":"us-east-1","data_size_gb":8,"throughput_mode":"standard",
			"replica_count":0,"status":"creating","aof_enabled":true,"name":"c"}}`))
	}))
	defer srv.Close()

	inst, err := NewClient(srv.URL, "k").GetInstance(context.Background(), "i-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if inst.Cloud != "aws" || inst.Region != "us-east-1" || inst.Tier != "essentials" {
		t.Errorf("response not unwrapped: %+v", inst)
	}
	if inst.DataSizeGB != 8 || inst.ThroughputMode != "standard" || !inst.AofEnabled {
		t.Errorf("fields lost: %+v", inst)
	}
}

// An empty wrapper must be an error rather than a zero-valued instance that
// Terraform would write into state as real.
func TestGetInstanceRejectsAnEmptyWrapper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"instance":{}}`))
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "k").GetInstance(context.Background(), "i-1"); err == nil {
		t.Fatal("an empty instance must not be accepted as real")
	}
}
