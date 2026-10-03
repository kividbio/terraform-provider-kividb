package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Which clouds the provider accepts, and why that list is not the same question
// as which clouds the API sells.
//
// The provider's validator is a convenience: it turns a typo into a plan-time
// error instead of an apply-time one. It is NOT the gate on what is sellable --
// that lives in the control plane's capability table, and a provider-side list
// that disagrees with it can only be corrected by releasing a new provider and
// waiting for every practitioner to upgrade.
//
// All three clouds the API sells are accepted. This test pins that, and that the
// set stays closed, because the validator is easy to "tidy" in either direction.
func TestCloudValidatorAcceptsEveryCloudTheApiCanEverSell(t *testing.T) {
	attr := instanceSchemaCloudAttribute(t)

	for _, cloud := range []string{"aws", "azure", "gcp"} {
		if diags := runStringValidators(attr.Validators, cloud); diags.HasError() {
			t.Errorf("cloud %q must be accepted by the schema: %v", cloud, diags.Errors())
		}
	}

	// Still a closed set. An unbounded attribute would let a typo reach the API
	// as a create for a cloud that has never existed, which comes back as a
	// generic validation failure rather than "you misspelled azure".
	for _, nonsense := range []string{"", "AWS", "amazon", "googlecloud", "akamai"} {
		if diags := runStringValidators(attr.Validators, nonsense); !diags.HasError() {
			t.Errorf("cloud %q must be rejected by the schema", nonsense)
		}
	}
}

// The registry docs are generated from this string, so it must name every cloud
// and must not carry the pre-launch "not yet available" warning: GCP went live
// on 2026-10-03, and a stale warning would tell practitioners not to use it.
func TestCloudDescriptionListsEveryCloud(t *testing.T) {
	attr := instanceSchemaCloudAttribute(t)
	desc := attr.MarkdownDescription

	for _, needle := range []string{"aws", "azure", "gcp"} {
		if !strings.Contains(desc, needle) {
			t.Errorf("the cloud description must list %q: %q", needle, desc)
		}
	}
	if strings.Contains(desc, "not yet available") {
		t.Errorf("the cloud description still says gcp is not available: %q", desc)
	}
	// The replacement warning is the other thing this attribute has to carry, and
	// it is easy to lose when rewriting the sentence around it.
	if !strings.Contains(desc, "replaces the database") {
		t.Errorf("the cloud description must still warn that a change replaces the database: %q", desc)
	}
}

func instanceSchemaCloudAttribute(t *testing.T) schema.StringAttribute {
	t.Helper()

	var resp resource.SchemaResponse
	NewInstanceResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)

	raw, ok := resp.Schema.Attributes["cloud"]
	if !ok {
		t.Fatal("the instance resource has no `cloud` attribute")
	}
	attr, ok := raw.(schema.StringAttribute)
	if !ok {
		t.Fatalf("`cloud` is not a string attribute: %T", raw)
	}
	return attr
}

func runStringValidators(vs []validator.String, value string) diag.Diagnostics {
	req := validator.StringRequest{ConfigValue: types.StringValue(value)}
	var all diag.Diagnostics
	for _, v := range vs {
		var resp validator.StringResponse
		v.ValidateString(context.Background(), req, &resp)
		all.Append(resp.Diagnostics...)
	}
	return all
}
