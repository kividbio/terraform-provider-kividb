package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// A database made from a snapshot is made where the snapshot lives.
//
// The API holds every restore to one rule: a snapshot is used only in its own
// cloud, region and cloud account. It refuses anything else with
// snapshot_account_mismatch, snapshot_cloud_mismatch or
// snapshot_region_mismatch -- and an omitted cloud_account_id on a create from
// a snapshot means "the snapshot's account", not KiviDB's. This file is the
// provider's half of that: the plan says where the database will go, and when
// the snapshot can be looked up at plan time a mismatch is an error in the
// plan rather than a refusal halfway through an apply.

// cloudAccountFromSnapshotOrState plans cloud_account_id when it is not
// configured.
//
// The attribute is optional and computed: computed because a database made
// from a snapshot takes the snapshot's account when none is set. Left at that,
// the framework would plan every unset cloud_account_id as unknown, and every
// database in KiviDB's cloud would show "(known after apply)" on every plan --
// and, the attribute forcing replacement, an unknown would plan a rebuild.
//
// So an unset value is planned as:
//
//   - the value in state, for a database that exists. Whatever account it is
//     in, it stays there: unsetting the attribute is not a request to move it,
//     and must never destroy it. That matters most for a database whose
//     account came from its snapshot -- removing restore_from_snapshot_id
//     afterwards would otherwise plan it into KiviDB's cloud, and replace it.
//     A different snapshot does replace the database, and Terraform plans a
//     replacement as a create, with no state, so that case lands below;
//   - unknown, for a database being created from a snapshot -- ModifyPlan fills
//     it in when it can read the snapshot;
//   - null, for any other database being created: KiviDB's cloud.
type cloudAccountFromSnapshotOrStateModifier struct{}

func cloudAccountFromSnapshotOrState() planmodifier.String {
	return cloudAccountFromSnapshotOrStateModifier{}
}

func (cloudAccountFromSnapshotOrStateModifier) Description(_ context.Context) string {
	return "Unset means KiviDB's cloud, or the snapshot's account for a database made from a snapshot."
}

func (m cloudAccountFromSnapshotOrStateModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (cloudAccountFromSnapshotOrStateModifier) PlanModifyString(
	ctx context.Context, req planmodifier.StringRequest, res *planmodifier.StringResponse,
) {
	// Configured, or configured from something not yet known: the
	// configuration decides.
	if !req.ConfigValue.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	switch {
	case !req.State.Raw.IsNull():
		res.PlanValue = req.StateValue
	case restoringFrom(ctx, req.Plan):
		// Left unknown for ModifyPlan.
	default:
		res.PlanValue = types.StringNull()
	}
}

// replaceWhenAccountChanges replaces the database when cloud_account_id names
// a different account. The API answers ids in lower case and the configured
// spelling is kept (see uuidPattern), so the same id in another case is not a
// different account -- the plain RequiresReplace compared strings, and
// writing out an inherited account in upper case planned a rebuild.
func replaceWhenAccountChanges() planmodifier.String {
	return stringplanmodifier.RequiresReplaceIf(
		func(_ context.Context, req planmodifier.StringRequest, res *stringplanmodifier.RequiresReplaceIfFuncResponse) {
			if req.PlanValue.IsUnknown() || req.PlanValue.IsNull() || req.StateValue.IsNull() {
				res.RequiresReplace = true
				return
			}
			res.RequiresReplace = !strings.EqualFold(req.StateValue.ValueString(), req.PlanValue.ValueString())
		},
		"Changing the cloud account replaces the database.",
		"Changing the cloud account replaces the database.",
	)
}

// restoringFrom reports whether the plan names a snapshot to create from. An
// unknown id counts: it will name one.
func restoringFrom(ctx context.Context, plan interface {
	GetAttribute(context.Context, path.Path, any) diag.Diagnostics
}) bool {
	for _, name := range []string{"restore_from_snapshot_id", "restore_from_kdb_snapshot_id"} {
		var v types.String
		if plan.GetAttribute(ctx, path.Root(name), &v).HasError() {
			continue
		}
		if !v.IsNull() {
			return true
		}
	}
	return false
}

// replaceWhenSnapshotChanges replaces the database when the snapshot it is
// made from is changed to a different one -- and only then.
//
// Removing the attribute does not replace it: the database was made, and
// forgetting where from is no reason to destroy it. Adding it to a database
// that already exists does not either: that is what configuration written
// after an import looks like, since the API keeps no record of the snapshot a
// database came from, and destroying an imported database to "restore" it
// would be the worst possible reading. ModifyPlan warns in that case instead.
func replaceWhenSnapshotChanges() planmodifier.String {
	return stringplanmodifier.RequiresReplaceIf(
		func(_ context.Context, req planmodifier.StringRequest, res *stringplanmodifier.RequiresReplaceIfFuncResponse) {
			if req.StateValue.IsNull() || req.PlanValue.IsNull() {
				return
			}
			if req.PlanValue.IsUnknown() {
				res.RequiresReplace = true
				return
			}
			res.RequiresReplace = !strings.EqualFold(req.StateValue.ValueString(), req.PlanValue.ValueString())
		},
		"Changing to a different snapshot replaces the database.",
		"Changing to a different snapshot replaces the database.",
	)
}

// ModifyPlan settles what attribute modifiers cannot: where a database made
// from a snapshot will be, and the name of the account it is in.
func (r *instanceResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, res *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var plan instanceModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	var configured types.String
	res.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("cloud_account_id"), &configured)...)
	if res.Diagnostics.HasError() {
		return
	}

	creating := req.State.Raw.IsNull()
	var state instanceModel
	if !creating {
		res.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if res.Diagnostics.HasError() {
			return
		}
	}

	kind, attr, snapshotID := snapshotSource(plan)

	if !creating && snapshotID.ValueString() != "" {
		var before types.String
		if kind == SnapshotKindKdb {
			before = state.RestoreFromKdbSnapshotID
		} else {
			before = state.RestoreFromSnapshotID
		}
		if before.IsNull() {
			res.Diagnostics.AddAttributeWarning(path.Root(attr),
				"The snapshot is only read when the database is created",
				fmt.Sprintf("This database already exists, so setting `%s` records the snapshot without "+
					"restoring anything, and does not replace the database. To create the database again "+
					"from this snapshot -- which destroys its current data -- run "+
					"`terraform apply -replace=<this resource's address>`.", attr))
		}
	}

	// Where the snapshot lives, when that can be known now: on a create, with
	// the snapshot's id known and a configured client.
	if creating && r.client != nil && !snapshotID.IsNull() && !snapshotID.IsUnknown() && snapshotID.ValueString() != "" {
		placement, err := r.client.SnapshotPlacementOf(ctx, kind, strings.ToLower(snapshotID.ValueString()))
		switch {
		case err == nil:
			res.Diagnostics.Append(checkSnapshotPlacement(kind, attr, placement, plan, configured)...)
			if res.Diagnostics.HasError() {
				return
			}
			if configured.IsNull() {
				// The API places the database in the snapshot's account when
				// none is named, so the plan can say which.
				plan.CloudAccountID = stringOrNull(placement.CloudAccountID)
			}
		default:
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.IsNotFound() {
				res.Diagnostics.AddAttributeError(path.Root(attr), "No such snapshot",
					fmt.Sprintf("No %s snapshot with id %s exists in this organization, or it was deleted.",
						kind, snapshotID.ValueString()))
				return
			}
			if errors.As(err, &apiErr) && apiErr.Code == "snapshot_not_ready" {
				res.Diagnostics.AddAttributeWarning(path.Root(attr), "The snapshot has not finished",
					fmt.Sprintf("%s snapshot %s is still being written. A database can only be made from a "+
						"finished snapshot; the apply is refused if it has not finished by then.",
						capitalize(string(kind)), snapshotID.ValueString()))
				break
			}
			// Anything else is not a reason to fail a plan: the API holds the
			// same rule at apply and explains a refusal there.
			res.Diagnostics.AddAttributeWarning(path.Root(attr), "Could not check the snapshot at plan time",
				"Where the snapshot lives could not be read, so its cloud, region and cloud account are "+
					"checked at apply instead: "+explain(err))
		}
	}

	// The account's name follows the account. Unknown on a create: the plan
	// would otherwise have to promise a display name before the API has
	// answered with one.
	switch {
	case plan.CloudAccountID.IsUnknown():
		plan.CloudAccountName = types.StringUnknown()
	case plan.CloudAccountID.IsNull():
		plan.CloudAccountName = types.StringNull()
	case !creating && !state.CloudAccountID.IsNull() &&
		strings.EqualFold(state.CloudAccountID.ValueString(), plan.CloudAccountID.ValueString()):
		plan.CloudAccountName = state.CloudAccountName
	default:
		plan.CloudAccountName = types.StringUnknown()
	}

	res.Diagnostics.Append(res.Plan.SetAttribute(ctx, path.Root("cloud_account_id"), plan.CloudAccountID)...)
	res.Diagnostics.Append(res.Plan.SetAttribute(ctx, path.Root("cloud_account_name"), plan.CloudAccountName)...)
}

// snapshotSource names the snapshot the plan creates from, if any. The
// schema refuses both at once.
func snapshotSource(plan instanceModel) (SnapshotKind, string, types.String) {
	if !plan.RestoreFromKdbSnapshotID.IsNull() {
		return SnapshotKindKdb, "restore_from_kdb_snapshot_id", plan.RestoreFromKdbSnapshotID
	}
	return SnapshotKindDisk, "restore_from_snapshot_id", plan.RestoreFromSnapshotID
}

// checkSnapshotPlacement is the API's rule, applied to the plan: the account
// first, because no move can fix that one, then the cloud, then the region.
// Values not yet known are not checked; the API checks them at apply.
func checkSnapshotPlacement(kind SnapshotKind, attr string, p *SnapshotPlacement, plan instanceModel, configured types.String) diag.Diagnostics {
	var diags diag.Diagnostics
	snapAccount := ""
	if p.CloudAccountID != nil {
		snapAccount = *p.CloudAccountID
	}

	if !configured.IsNull() && !configured.IsUnknown() && !strings.EqualFold(configured.ValueString(), snapAccount) {
		fix := fmt.Sprintf("Set `cloud_account_id = %q`, or leave `cloud_account_id` unset and the database "+
			"is placed in the snapshot's account.", snapAccount)
		if snapAccount == "" {
			fix = "Remove `cloud_account_id`: the snapshot is in KiviDB's cloud, so the database is too."
		}
		diags.AddAttributeError(path.Root("cloud_account_id"), "The snapshot is in a different cloud account",
			fmt.Sprintf("%s snapshot %s lives in %s; it can only be restored there. Snapshots stay in the "+
				"account they were taken in. %s To move data between accounts, export it from the source "+
				"database and import it into the target.",
				capitalize(string(kind)), plan.snapshotIDFor(attr), p.AccountLabel(), fix))
		return diags
	}

	if !plan.Cloud.IsUnknown() && p.Cloud != "" && plan.Cloud.ValueString() != p.Cloud {
		diags.AddAttributeError(path.Root("cloud"), "The snapshot is on a different cloud",
			fmt.Sprintf("%s snapshot %s is on %s and this database is on %s. Snapshots stay in the cloud "+
				"they were taken in, so there is no way to move this one across. Set `cloud = %q`, or "+
				"export the data from the source database and import it into the target.",
				capitalize(string(kind)), plan.snapshotIDFor(attr), p.Cloud, plan.Cloud.ValueString(), p.Cloud))
		return diags
	}

	if !plan.Region.IsUnknown() && p.Region != "" && plan.Region.ValueString() != p.Region {
		diags.AddAttributeError(path.Root("region"), "The snapshot is in a different region",
			fmt.Sprintf("%s snapshot %s is in %s and this database is in %s. Set `region = %q`, or move "+
				"the snapshot to %s first and then create the database from it.",
				capitalize(string(kind)), plan.snapshotIDFor(attr), p.Region, plan.Region.ValueString(),
				p.Region, plan.Region.ValueString()))
	}
	return diags
}

func (m instanceModel) snapshotIDFor(attr string) string {
	if attr == "restore_from_kdb_snapshot_id" {
		return m.RestoreFromKdbSnapshotID.ValueString()
	}
	return m.RestoreFromSnapshotID.ValueString()
}

// snapshotLockHint is what to change in the configuration after the API
// refused a restore for the snapshot's location.
func snapshotLockHint(e *APIError) string {
	const rule = "A database made from a snapshot must be in the snapshot's cloud account, cloud and region."
	switch e.Code {
	case "snapshot_account_mismatch":
		if !e.HasSnapshotAccount {
			return rule + " Leave `cloud_account_id` unset to use the snapshot's account."
		}
		if e.SnapshotCloudAccountID == nil {
			return rule + " The snapshot is in KiviDB's cloud: remove `cloud_account_id` from the configuration."
		}
		return rule + fmt.Sprintf(" Set `cloud_account_id = %q`, or leave it unset to use the snapshot's account.",
			*e.SnapshotCloudAccountID)
	case "snapshot_cloud_mismatch":
		if e.SnapshotCloud != "" {
			return rule + fmt.Sprintf(" Set `cloud = %q`.", e.SnapshotCloud)
		}
	case "snapshot_region_mismatch":
		if e.SnapshotRegion != "" {
			return rule + fmt.Sprintf(" Set `region = %q`, or move the snapshot to the region you want first.",
				e.SnapshotRegion)
		}
	}
	return rule
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
