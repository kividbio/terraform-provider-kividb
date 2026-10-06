package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// stateUnlessChanged keeps a computed value from state when none of the
// attributes it is derived from change in this plan.
//
// It is UseStateForUnknown with conditions, and exists for the private
// endpoints of a database in the customer's own cloud account. Those are not
// stored anywhere: they are a function of the name, the tier and the replica
// count. Plain UseStateForUnknown would be wrong -- a rename moves the
// hostname, a replica scale adds or removes one -- and promise a value the
// apply would then contradict. Leaving them unknown on every plan would be
// right but noisy: every unrelated resize would show three endpoints as
// "(known after apply)", in the one place a practitioner reads carefully.
//
// So: unknown when an input changes, the value in state when none does.
type stateUnlessChanged struct {
	deps []string
}

func useStateUnlessChanged(deps ...string) stateUnlessChanged {
	return stateUnlessChanged{deps: deps}
}

func (m stateUnlessChanged) Description(_ context.Context) string {
	return "Keeps the value from state unless an attribute it is derived from changes."
}

func (m stateUnlessChanged) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m stateUnlessChanged) keep(ctx context.Context, plan tfsdk.Plan, state tfsdk.State) bool {
	// Create (no state) or destroy (no plan): nothing to carry over.
	if state.Raw.IsNull() || plan.Raw.IsNull() {
		return false
	}
	return inputsUnchanged(ctx, plan, state, m.deps)
}

func (m stateUnlessChanged) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, res *planmodifier.StringResponse) {
	if !req.PlanValue.IsUnknown() || req.ConfigValue.IsUnknown() {
		return
	}
	if m.keep(ctx, req.Plan, req.State) {
		res.PlanValue = req.StateValue
	}
}

func (m stateUnlessChanged) PlanModifyList(ctx context.Context, req planmodifier.ListRequest, res *planmodifier.ListResponse) {
	if !req.PlanValue.IsUnknown() || req.ConfigValue.IsUnknown() {
		return
	}
	if m.keep(ctx, req.Plan, req.State) {
		res.PlanValue = req.StateValue
	}
}

// inputsUnchanged reports whether every named top-level attribute has the same,
// known value in the plan as in state. An unknown planned value counts as a
// change: it may well be one.
func inputsUnchanged(ctx context.Context, plan tfsdk.Plan, state tfsdk.State, names []string) bool {
	for _, n := range names {
		p := path.Root(n)
		switch n {
		case "replica_count", "data_size_gb":
			var a, b types.Int64
			if plan.GetAttribute(ctx, p, &a).HasError() || state.GetAttribute(ctx, p, &b).HasError() {
				return false
			}
			if a.IsUnknown() || !a.Equal(b) {
				return false
			}
		default:
			var a, b types.String
			if plan.GetAttribute(ctx, p, &a).HasError() || state.GetAttribute(ctx, p, &b).HasError() {
				return false
			}
			if a.IsUnknown() || !a.Equal(b) {
				return false
			}
		}
	}
	return true
}
