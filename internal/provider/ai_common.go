package provider

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Shared by the AI resources (semantic cache, agent memory, context retriever).

// aiNamePattern is SEMANTIC_CACHE_NAME_RE, AGENT_MEMORY_NAME_RE and
// CONTEXT_MODEL_NAME_RE, which are one pattern.
var aiNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

const aiNameRule = "must be 1-63 lowercase letters, digits, - or _, starting with a letter or digit"

// httpsPattern mirrors the API's "must use https" check on a custom endpoint.
var httpsPattern = regexp.MustCompile(`^https://\S+$`)

// isGone reports a 404: the thing, or the database it lived in, no longer
// exists. Terraform drops it from state rather than failing every plan.
func isGone(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsNotFound()
}

// splitImportID reads "<first>/<second>".
func splitImportID(id, format string) (string, string, error) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected an id of the form %s, got %q", format, id)
	}
	return parts[0], parts[1], nil
}

// alreadyExists is the error a create gives when the name is taken.
//
// Every settings write in these APIs is an upsert, so a create that did not look
// first would quietly take over -- and rewrite -- a namespace, store or model
// that something else made. Terraform's contract is that a create makes a new
// thing; an existing one is brought under management by importing it.
func alreadyExists(diags *diag.Diagnostics, what, address, importID string) {
	diags.AddError(
		what+" already exists",
		fmt.Sprintf("%s already exists. Creating it here would overwrite its settings. "+
			"To manage it with Terraform, import it instead:\n\n  terraform import %s %s", what, address, importID),
	)
}

// strPtr is a known, non-null string as a pointer, else nil.
func strPtr(s types.String) *string {
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	v := s.ValueString()
	return &v
}

func int64Ptr(v types.Int64) *int64 {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	x := v.ValueInt64()
	return &x
}

func float64Ptr(v types.Float64) *float64 {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	x := v.ValueFloat64()
	return &x
}

// checkProvider mirrors the API's rules for a provider and what goes with it:
// a model unless the provider is "none", a base URL for "openai_compatible"
// and only for it. A value the API would silently drop (a model for "none")
// is an error here, because it would read back as null and Terraform would
// report the provider as having changed it.
func checkProvider(diags *diag.Diagnostics, providerAttr string, provider, model, baseURL, apiKey types.String, modelAttr, baseAttr, keyAttr string) {
	if provider.IsNull() || provider.IsUnknown() {
		return
	}
	p := provider.ValueString()
	set := func(v types.String) bool { return !v.IsNull() && !v.IsUnknown() }
	if p == "none" {
		for attr, v := range map[string]types.String{modelAttr: model, baseAttr: baseURL, keyAttr: apiKey} {
			if set(v) {
				diags.AddAttributeError(path.Root(attr), "Not used without a provider",
					fmt.Sprintf("`%s` is only used when `%s` is not \"none\". Remove it, or choose a provider.", attr, providerAttr))
			}
		}
		return
	}
	if model.IsNull() {
		diags.AddAttributeError(path.Root(modelAttr), "Model required",
			fmt.Sprintf("`%s` is required when `%s` is %q.", modelAttr, providerAttr, p))
	}
	if p == "openai_compatible" {
		if baseURL.IsNull() {
			diags.AddAttributeError(path.Root(baseAttr), "Base URL required",
				fmt.Sprintf("`%s` is required when `%s` is \"openai_compatible\".", baseAttr, providerAttr))
		}
	} else if set(baseURL) {
		diags.AddAttributeError(path.Root(baseAttr), "Base URL not used",
			fmt.Sprintf("`%s` is only used when `%s` is \"openai_compatible\".", baseAttr, providerAttr))
	}
}

// keepSpelling returns what was configured when the server's value is the same
// text trimmed. The API trims descriptions before storing them; reading back
// the trimmed text would be reported as the provider changing the plan.
func keepSpelling(prior types.String, got string) types.String {
	if !prior.IsNull() && !prior.IsUnknown() && strings.TrimSpace(prior.ValueString()) == got {
		return prior
	}
	return types.StringValue(got)
}

func keepOptionalSpelling(prior types.String, got *string) types.String {
	if got == nil {
		return types.StringNull()
	}
	return keepSpelling(prior, *got)
}

// explainAI adds what to do for the refusals these resources meet in practice.
func explainAI(err error) string {
	apiErr, ok := err.(*APIError)
	if !ok {
		return explain(err)
	}
	switch apiErr.Code {
	case "instance_not_running":
		return apiErr.Message + "\n\nStart the database, then apply again."
	case "not_empty":
		return apiErr.Message + "\n\nThe API changes the vector size only when nothing is stored. " +
			"Empty it first, or replace this resource with `terraform apply -replace=...`, " +
			"which deletes everything stored in it."
	default:
		return explain(err)
	}
}
