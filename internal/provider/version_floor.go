package provider

import (
	"context"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
)

// versionFloorModifier makes `kividb_version` the oldest release the
// configuration accepts rather than an exact pin.
//
// KiviDB Cloud supports only the latest patch of each supported minor release
// and moves databases on an older patch to it in their maintenance window. A
// configuration that says "1.2.0" would then plan a change back to 1.2.0 on
// every run, which the API refuses as a downgrade, so every apply would fail.
// When the database already runs something newer than the configuration asks
// for, the plan keeps what it runs. Asking for a newer release than it runs
// still upgrades it.
type versionFloorModifier struct{}

func (versionFloorModifier) Description(context.Context) string {
	return "A database already on a newer release than configured keeps it; the configured version is a minimum."
}

func (m versionFloorModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (versionFloorModifier) PlanModifyString(_ context.Context, req planmodifier.StringRequest, res *planmodifier.StringResponse) {
	if req.State.Raw.IsNull() || req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if req.StateValue.IsNull() || req.StateValue.IsUnknown() {
		return
	}
	want, have := req.ConfigValue.ValueString(), req.StateValue.ValueString()
	wr, hr := kividbVersionRank(want), kividbVersionRank(have)
	if wr < 0 || hr < 0 {
		return
	}
	if hr > wr {
		res.PlanValue = req.StateValue
	}
}

// kividbVersionRank orders engine versions: higher is newer, -1 when the
// string is not a version. A release candidate ranks below its release and
// above the previous one, as on the server (packages/shared kividbVersionRank).
func kividbVersionRank(v string) int64 {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return -1
	}
	var n [3]int64
	for i, p := range parts {
		x, err := strconv.ParseInt(p, 10, 64)
		if err != nil || x < 0 {
			return -1
		}
		n[i] = x
	}
	base := n[0]*1_000_000_000 + n[1]*1_000_000 + n[2]*1_000
	if !hasPre {
		return base
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, pre)
	seq, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		seq = 0
	}
	if seq > 999 {
		seq = 999
	}
	return base - 1_000 + seq
}
