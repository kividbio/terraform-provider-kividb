package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRvTerraformEndToEnd drives the real terraform binary against the fake
// API, for what only Terraform itself decides: that a replacement's create
// half is planned with the old reservation's id in private state, and that
// metering moving consumed_cents is never a diff. Off unless
// KIVIDB_RV_E2E=1; needs terraform on PATH and no network (dev_overrides).
func TestRvTerraformEndToEnd(t *testing.T) {
	if os.Getenv("KIVIDB_RV_E2E") != "1" {
		t.Skip("set KIVIDB_RV_E2E=1 to run terraform against the fake API")
	}
	tf, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform not on PATH")
	}
	f, srv := rvNewFake(t, 4992)
	f.addInstance(rvID(1), "alpha", 1452)
	f.addInstance(rvID(2), "beta", 1452)

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	exe := "terraform-provider-kividb"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command("go", "build", "-o", filepath.Join(bin, exe), "../..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	rc := filepath.Join(dir, "dev.tfrc")
	_ = os.WriteFile(rc, []byte(`provider_installation {
  dev_overrides { "kividbio/kividb" = "`+filepath.ToSlash(bin)+`" }
  direct {}
}
`), 0o600)
	cfg := filepath.Join(dir, "cfg")
	_ = os.MkdirAll(cfg, 0o755)
	write := func(amount string) {
		_ = os.WriteFile(filepath.Join(cfg, "main.tf"), []byte(`terraform {
  required_providers {
    kividb = { source = "kividbio/kividb" }
  }
}
provider "kividb" {
  endpoint = "`+srv.URL+`"
  api_key  = "kvdb_test"
}
resource "kividb_credit_reservation" "alpha" {
  instance_id  = "`+rvID(1)+`"
  amount_cents = `+amount+`
}
`), 0o600)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command(tf, args...)
		cmd.Dir = cfg
		cmd.Env = append(os.Environ(), "TF_CLI_CONFIG_FILE="+rc, "TF_IN_AUTOMATION=1", "NO_COLOR=1")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Refused at plan, in the API's words.
	write("5000")
	out, err := run("plan", "-no-color")
	if err == nil || !strings.Contains(out, "Only $49.92 of your balance is unreserved; add credits to reserve $50.00.") {
		t.Fatalf("plan should fail with the refusal:\n%s", out)
	}

	// Allowed with a warning, applied.
	write("4000")
	out, err = run("apply", "-auto-approve", "-no-color")
	if err != nil || !strings.Contains(out, "(beta)") {
		t.Fatalf("apply:\n%s", out)
	}

	// Metering moves consumed_cents: no changes.
	f.mu.Lock()
	f.rows[0].consumed = 3500
	f.mu.Unlock()
	out, err = run("plan", "-no-color", "-detailed-exitcode")
	if err != nil {
		t.Fatalf("metering must not be a diff:\n%s", out)
	}

	// A replacement the API would refuse fails the plan: Terraform keeps the
	// errors of a replacement's create half (and drops its warnings).
	f.mu.Lock()
	f.balance = 3000
	f.mu.Unlock()
	write("2990")
	out, err = run("plan", "-no-color")
	if err == nil || !strings.Contains(out, "That would leave $0.10 for your 1 running database(s) without a reservation") {
		t.Fatalf("a refused replacement must fail the plan: %s", out)
	}
	f.mu.Lock()
	f.balance = 4992
	f.mu.Unlock()

	// Lowering: one replacement, previewed with the old reservation released.
	write("3000")
	f.resetCalls()
	out, err = run("plan", "-no-color")
	if err != nil || !strings.Contains(out, "must be replaced") || !strings.Contains(out, "Lowering a credit reservation replaces it") ||
		!strings.Contains(out, "(beta)") {
		t.Fatalf("lowering plan:\n%s", out)
	}
	released := false
	for _, p := range f.previews {
		if p["release_reservation_id"] == rvID(1) {
			released = true
		}
	}
	if !released || strings.Contains(out, "already has an active reservation") {
		t.Fatalf("the replacement was not previewed with the release (previews %v):\n%s", f.previews, out)
	}
	out, err = run("apply", "-auto-approve", "-no-color")
	if err != nil {
		t.Fatalf("replacement apply:\n%s", out)
	}
	if len(f.rows) != 1 || f.rows[0].amount != 3000 || f.rows[0].status != "active" {
		t.Errorf("after replacement: %+v", f.rows[0])
	}
	out, err = run("destroy", "-auto-approve", "-no-color")
	if err != nil || len(f.rows) != 0 {
		t.Fatalf("destroy:\n%s", out)
	}
}
