package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestKividbVersionRank(t *testing.T) {
	order := []string{"1.1.3", "1.2.0", "1.2.1-rc2", "1.2.1-rc3", "1.2.1", "1.3.0"}
	for i := 1; i < len(order); i++ {
		if kividbVersionRank(order[i-1]) >= kividbVersionRank(order[i]) {
			t.Fatalf("%s should rank below %s", order[i-1], order[i])
		}
	}
	for _, bad := range []string{"", "latest", "1.2", "1.2.x"} {
		if kividbVersionRank(bad) != -1 {
			t.Fatalf("%q should not read as a version", bad)
		}
	}
}

func TestVersionFloorModifier(t *testing.T) {
	existing := tfsdk.State{Raw: tftypes.NewValue(tftypes.Object{}, map[string]tftypes.Value{})}
	run := func(config, state string, created bool) string {
		req := planmodifier.StringRequest{
			ConfigValue: types.StringValue(config),
			StateValue:  types.StringValue(state),
			PlanValue:   types.StringValue(config),
		}
		if created {
			req.State = existing
		} else {
			req.State = tfsdk.State{Raw: tftypes.NewValue(tftypes.Object{}, nil)}
			req.StateValue = types.StringNull()
		}
		res := &planmodifier.StringResponse{PlanValue: req.PlanValue}
		versionFloorModifier{}.PlanModifyString(context.Background(), req, res)
		return res.PlanValue.ValueString()
	}
	cases := []struct {
		name, config, state string
		created             bool
		want                string
	}{
		{"moved to a newer patch: keep it, no downgrade", "1.2.0", "1.2.1", true, "1.2.1"},
		{"asking for a newer release upgrades", "1.2.1", "1.2.0", true, "1.2.1"},
		{"same version: nothing to do", "1.2.1", "1.2.1", true, "1.2.1"},
		{"moved off an unsupported minor: keep it", "1.1.3", "1.2.1", true, "1.2.1"},
		{"create: the configured version", "1.2.1", "", false, "1.2.1"},
	}
	for _, c := range cases {
		if got := run(c.config, c.state, c.created); got != c.want {
			t.Errorf("%s: plan %q, want %q", c.name, got, c.want)
		}
	}
}
