package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*maintenanceWindowResource)(nil)
	_ resource.ResourceWithConfigure   = (*maintenanceWindowResource)(nil)
	_ resource.ResourceWithImportState = (*maintenanceWindowResource)(nil)
)

func NewMaintenanceWindowResource() resource.Resource { return &maintenanceWindowResource{} }

type maintenanceWindowResource struct{ client *Client }

type maintenanceWindowModel struct {
	ID              types.String `tfsdk:"id"`
	InstanceID      types.String `tfsdk:"instance_id"`
	Day             types.String `tfsdk:"day"`
	Hour            types.Int64  `tfsdk:"hour"`
	Timezone        types.String `tfsdk:"timezone"`
	LengthHours     types.Int64  `tfsdk:"length_hours"`
	Description     types.String `tfsdk:"description"`
	NoticeHours     types.Int64  `tfsdk:"notice_hours"`
	NextWindowStart types.String `tfsdk:"next_window_start"`
	Planned         types.List   `tfsdk:"planned"`
}

// The API numbers days from Sunday, as JavaScript does.
var weekdays = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

// The window every database starts with (migration 0140), which Delete puts
// back.
const (
	defaultMaintenanceDay      = 0
	defaultMaintenanceHour     = 3
	defaultMaintenanceTimezone = "UTC"
)

var plannedMaintenanceType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"kind":           types.StringType,
	"starts_at":      types.StringType,
	"restarts_nodes": types.BoolType,
}}

func (r *maintenanceWindowResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_maintenance_window"
}

func (r *maintenanceWindowResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: `When KiviDB may do planned maintenance on a database: a weekly window,
four hours long, starting at a given hour on a given day in a given time zone.

Planned maintenance is work KiviDB starts on its own -- today, renewing the
database's TLS certificate, which restarts each node for a few seconds. It is
announced to the organization's owners at least 48 hours ahead and done inside
this window. Changes you make, failovers and the recovery of a failed node do
not wait for it.

Every database has a window from the start: Sunday 03:00 UTC unless it is
changed. This resource sets it. **Deleting this resource puts the window back to
Sunday 03:00 UTC** rather than leaving it where it was, because a window always
exists and that is the one a database without configuration has. Moving the
window, either way, moves maintenance already planned into the new one, with
fresh notice.

The day and hour are in ` + "`timezone`" + `, so a window at 03:00 in Europe/Berlin stays
at 03:00 local time across daylight-saving changes.`,
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The database's id: a database has one window.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database whose window this is. **Changing this resets the old database's window and sets the new one's.**",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"day": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The day the window starts on, in `timezone`: `sunday`, `monday`, ... `saturday`.",
				Validators:          []validator.String{stringvalidator.OneOfCaseInsensitive(weekdays...)},
			},
			"hour": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "The hour the window starts at, 0-23, in `timezone`. The window lasts four hours from it.",
				Validators:          []validator.Int64{int64validator.Between(0, 23)},
			},
			"timezone": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultMaintenanceTimezone),
				MarkdownDescription: "An IANA time zone such as `Europe/Berlin` or `Asia/Kolkata`. Defaults to `UTC`. " +
					"The API checks the name and refuses one it does not know.",
				Validators: []validator.String{stringvalidator.LengthBetween(1, 64)},
			},
			"length_hours": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "How long the window lasts.",
			},
			"description": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The window as the console shows it, e.g. `Sunday 03:00-07:00 (UTC)`.",
			},
			"notice_hours": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "The least notice given before planned maintenance.",
			},
			"next_window_start": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the next window starts, in UTC (RFC 3339). Moves on as each window passes.",
			},
			"planned": schema.ListAttribute{
				ElementType: plannedMaintenanceType,
				Computed:    true,
				MarkdownDescription: "Maintenance KiviDB has planned: `kind` (e.g. `tls_certificate_renewal`), " +
					"`starts_at`, and `restarts_nodes`. Empty when nothing is planned.",
			},
		},
	}
}

func (r *maintenanceWindowResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
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

func dayIndex(name string) int64 {
	for i, d := range weekdays {
		if strings.EqualFold(d, name) {
			return int64(i)
		}
	}
	return -1
}

// apply copies what the API answered over the model. The configured spelling
// of the day is kept when it names the same day.
func (m *maintenanceWindowModel) apply(instanceID string, v *Maintenance) {
	m.ID = types.StringValue(instanceID)
	m.InstanceID = types.StringValue(instanceID)
	if d := v.Window.Day; d >= 0 && int(d) < len(weekdays) {
		if m.Day.IsNull() || m.Day.IsUnknown() || dayIndex(m.Day.ValueString()) != d {
			m.Day = types.StringValue(weekdays[d])
		}
	}
	m.Hour = types.Int64Value(v.Window.Hour)
	m.Timezone = types.StringValue(v.Window.Timezone)
	m.LengthHours = types.Int64Value(v.Window.LengthHours)
	m.Description = types.StringValue(v.Window.Description)
	m.NoticeHours = types.Int64Value(v.NoticeHours)
	m.NextWindowStart = types.StringValue(v.NextWindowStart)

	elems := make([]attr.Value, 0, len(v.Planned))
	for _, p := range v.Planned {
		elems = append(elems, types.ObjectValueMust(plannedMaintenanceType.AttrTypes, map[string]attr.Value{
			"kind":           types.StringValue(p.Kind),
			"starts_at":      types.StringValue(p.StartsAt),
			"restarts_nodes": types.BoolValue(p.RestartsNodes),
		}))
	}
	m.Planned = types.ListValueMust(plannedMaintenanceType, elems)
}

func (r *maintenanceWindowResource) set(ctx context.Context, m *maintenanceWindowModel) error {
	id := m.InstanceID.ValueString()
	v, err := r.client.SetMaintenanceWindow(ctx, id, dayIndex(m.Day.ValueString()), m.Hour.ValueInt64(), m.Timezone.ValueString())
	if err != nil {
		return err
	}
	m.apply(id, v)
	return nil
}

func (r *maintenanceWindowResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan maintenanceWindowModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	if err := r.set(ctx, &plan); err != nil {
		res.Diagnostics.AddError("Could not set the maintenance window", explain(err))
		return
	}
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *maintenanceWindowResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state maintenanceWindowModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.InstanceID.ValueString()
	if id == "" {
		res.State.RemoveResource(ctx)
		return
	}
	v, err := r.client.GetMaintenance(ctx, id)
	if err != nil {
		if isNotFound(err) {
			res.State.RemoveResource(ctx) // the database is gone
			return
		}
		res.Diagnostics.AddError("Could not read the maintenance window", explain(err))
		return
	}
	state.apply(id, v)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *maintenanceWindowResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan maintenanceWindowModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	if err := r.set(ctx, &plan); err != nil {
		res.Diagnostics.AddError("Could not change the maintenance window", explain(err))
		return
	}
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

// Delete puts back the window a database starts with. A database always has a
// window, so leaving the configured one in place would keep a setting nothing
// describes any more; putting the default back destroys nothing.
func (r *maintenanceWindowResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state maintenanceWindowModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	_, err := r.client.SetMaintenanceWindow(ctx, state.InstanceID.ValueString(),
		defaultMaintenanceDay, defaultMaintenanceHour, defaultMaintenanceTimezone)
	if err != nil && !isNotFound(err) {
		res.Diagnostics.AddError("Could not reset the maintenance window", explain(err))
	}
}

func (r *maintenanceWindowResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("instance_id"), req, res)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
