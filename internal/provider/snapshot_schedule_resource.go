package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*snapshotScheduleResource)(nil)
	_ resource.ResourceWithConfigure   = (*snapshotScheduleResource)(nil)
	_ resource.ResourceWithImportState = (*snapshotScheduleResource)(nil)
)

func NewSnapshotScheduleResource() resource.Resource { return &snapshotScheduleResource{} }

type snapshotScheduleResource struct{ client *Client }

type snapshotScheduleModel struct {
	ID            types.String `tfsdk:"id"`
	InstanceID    types.String `tfsdk:"instance_id"`
	Enabled       types.Bool   `tfsdk:"enabled"`
	Cadence       types.String `tfsdk:"cadence"`
	RetentionMode types.String `tfsdk:"retention_mode"`
	Retention     types.Int64  `tfsdk:"retention"`
}

// The column defaults (migration 0019), used when an older server's response
// leaves a field out.
const (
	defaultSnapshotCadence       = "daily"
	defaultSnapshotRetention     = 5
	defaultSnapshotRetentionMode = "keep_last"
)

func (r *snapshotScheduleResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_snapshot_schedule"
}

func (r *snapshotScheduleResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: `Automatic disk snapshots of a database, taken on a schedule and expired by a
retention rule.

Each scheduled snapshot is the same kind of copy a ` + "`kividb_disk_snapshot`" + ` is,
and is billed the same way, by the gigabyte-month it is kept; retention is
what bounds that. Only snapshots the schedule took are ever expired by it: one
taken by hand, in the console or with ` + "`kividb_disk_snapshot`" + `, is kept until it
is deleted.

Snapshots are taken while the database is running. Free-tier and sharded
databases are not snapshotted on a schedule, so the setting is stored for them
but has no effect -- the apply says so.

**Deleting this resource turns the schedule off and leaves its retention as
it is.** Putting the default retention back could expire scheduled snapshots
already taken -- the default keeps the last 5 -- so the ones the schedule
holds stay under the rule they were taken with.`,
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The database's id: a database has one schedule.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database to snapshot. **Changing this turns the old database's schedule off and sets the new one's.**",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Take snapshots. Defaults to `true`; `false` stops new ones and keeps those " +
					"already taken under `retention`.",
			},
			"cadence": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultSnapshotCadence),
				MarkdownDescription: "How often: `hourly`, `daily` (the default) or `weekly`.",
				Validators:          []validator.String{stringvalidator.OneOf("hourly", "daily", "weekly")},
			},
			"retention_mode": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultSnapshotRetentionMode),
				MarkdownDescription: "How `retention` is counted: `keep_last` (the default) keeps that many of the " +
					"newest scheduled snapshots; `keep_days` keeps each for that many days. Changing either " +
					"applies to the snapshots already held, so shortening it expires some at once -- and an " +
					"expired snapshot cannot be recovered.",
				Validators: []validator.String{stringvalidator.OneOf("keep_last", "keep_days")},
			},
			"retention": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(defaultSnapshotRetention),
				MarkdownDescription: "Snapshots to keep (`keep_last`) or days to keep each one (`keep_days`), " +
					"1-365. Defaults to 5.",
				Validators: []validator.Int64{int64validator.Between(1, 365)},
			},
		},
	}
}

func (r *snapshotScheduleResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		res.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	r.client = client
}

func (m *snapshotScheduleModel) apply(inst *Instance) {
	m.ID = types.StringValue(inst.ID)
	m.InstanceID = types.StringValue(inst.ID)
	m.Enabled = types.BoolValue(inst.DiskSnapshotEnabled)
	m.Cadence = types.StringValue(stringOr(inst.DiskSnapshotCadence, defaultSnapshotCadence))
	m.RetentionMode = types.StringValue(stringOr(inst.DiskSnapshotRetentionMode, defaultSnapshotRetentionMode))
	retention := int64(defaultSnapshotRetention)
	if inst.DiskSnapshotRetentionDays != nil {
		retention = *inst.DiskSnapshotRetentionDays
	}
	m.Retention = types.Int64Value(retention)
}

func stringOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

// write sends the whole schedule and reads the database back, so state is what
// the API stored rather than what was asked for.
func (r *snapshotScheduleResource) write(ctx context.Context, m *snapshotScheduleModel) diag.Diagnostics {
	var diags diag.Diagnostics
	id := m.InstanceID.ValueString()
	enabled := m.Enabled.ValueBool()
	cadence := m.Cadence.ValueString()
	mode := m.RetentionMode.ValueString()
	retention := m.Retention.ValueInt64()
	if err := r.client.SetDiskSnapshotConfig(ctx, id, DiskSnapshotConfig{
		Enabled: &enabled, Cadence: &cadence, RetentionMode: &mode, RetentionDays: &retention,
	}); err != nil {
		diags.AddError("Could not set the snapshot schedule", explain(err))
		return diags
	}
	inst, err := r.client.GetInstance(ctx, id)
	if err != nil {
		diags.AddError("Could not read the snapshot schedule back", explain(err))
		return diags
	}
	m.apply(inst)
	if enabled {
		diags.Append(scheduleWillNotRun(inst)...)
	}
	return diags
}

// scheduleWillNotRun warns when the API stores a schedule it will not act on:
// the scheduler skips free-tier and sharded databases.
func scheduleWillNotRun(inst *Instance) diag.Diagnostics {
	var diags diag.Diagnostics
	switch {
	case inst.FreeTier:
		diags.AddAttributeWarning(path.Root("enabled"), "Free-tier databases are not snapshotted",
			"The schedule is stored, but KiviDB takes scheduled snapshots of paid databases only. It "+
				"starts once this database is upgraded.")
	case inst.shards() > 1:
		diags.AddAttributeWarning(path.Root("enabled"), "Sharded databases are not snapshotted",
			"The schedule is stored, but disk snapshots are not taken of sharded databases, which have "+
				"one disk per node. Use KDB snapshots for a sharded database.")
	}
	return diags
}

func (r *snapshotScheduleResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan snapshotScheduleModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	res.Diagnostics.Append(r.write(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *snapshotScheduleResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state snapshotScheduleModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.InstanceID.ValueString()
	if id == "" {
		res.State.RemoveResource(ctx)
		return
	}
	inst, err := r.client.GetInstance(ctx, id)
	if err != nil {
		if isNotFound(err) {
			res.State.RemoveResource(ctx) // the database is gone
			return
		}
		res.Diagnostics.AddError("Could not read the snapshot schedule", explain(err))
		return
	}
	state.apply(inst)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *snapshotScheduleResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan snapshotScheduleModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	res.Diagnostics.Append(r.write(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

// Delete turns the schedule off and nothing else; see the resource description
// for why retention is left as it is.
func (r *snapshotScheduleResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state snapshotScheduleModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	off := false
	err := r.client.SetDiskSnapshotConfig(ctx, state.InstanceID.ValueString(), DiskSnapshotConfig{Enabled: &off})
	if err != nil && !isNotFound(err) {
		res.Diagnostics.AddError("Could not turn the snapshot schedule off", explain(err))
	}
}

func (r *snapshotScheduleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("instance_id"), req, res)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
