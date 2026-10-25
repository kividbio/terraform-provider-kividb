package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
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
	_ resource.Resource                   = (*instanceExportResource)(nil)
	_ resource.ResourceWithConfigure      = (*instanceExportResource)(nil)
	_ resource.ResourceWithImportState    = (*instanceExportResource)(nil)
	_ resource.ResourceWithValidateConfig = (*instanceExportResource)(nil)
)

func NewInstanceExportResource() resource.Resource { return &instanceExportResource{} }

// instanceExportResource is a database's export configuration: which
// destination its KDB exports are delivered to, and how often it exports on its
// own. The configuration belongs to the database and always exists, so
// "creating" this sets it and "destroying" it turns exports off.
type instanceExportResource struct{ client *Client }

type instanceExportModel struct {
	ID                  types.String `tfsdk:"id"`
	InstanceID          types.String `tfsdk:"instance_id"`
	ExportDestinationID types.String `tfsdk:"export_destination_id"`
	Cadence             types.String `tfsdk:"cadence"`
}

func (r *instanceExportResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_instance_export"
}

func (r *instanceExportResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "Where a database's KDB exports are delivered, and how often it exports on its own.\n\n" +
			"With a destination set, every export of the database -- scheduled, or taken with " +
			"`kividb_kdb_snapshot` -- is also copied into that bucket. With a `cadence`, the database exports " +
			"itself daily or weekly. Destroying this resource turns both off. One per database: two would " +
			"overwrite each other.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The database's id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database whose exports this configures.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"export_destination_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The `kividb_export_destination` to deliver to. It must be verified, so reference " +
					"the `kividb_export_destination_verification`'s `id` (or depend on it) rather than the " +
					"destination's, or the API refuses a destination that has not passed yet. Unset, exports " +
					"stay in KiviDB's storage only.",
				Validators: []validator.String{stringvalidator.RegexMatches(uuidPattern, "must be an export destination id (a UUID)")},
			},
			"cadence": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("off"),
				MarkdownDescription: "How often the database exports on its own: `off`, `daily` or `weekly`. Defaults " +
					"to `off`. A schedule needs `export_destination_id`. KiviDB keeps its own copy of a " +
					"scheduled export for 7 days, since the bucket holds yours. Scheduled exports do not run " +
					"on free-tier databases, or on sharded ones.",
				Validators: []validator.String{stringvalidator.OneOf(kdbExportCadences...)},
			},
		},
	}
}

func (r *instanceExportResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, res *resource.ValidateConfigResponse) {
	var cfg instanceExportModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() {
		return
	}
	// The API's own refusal (destination_required), caught at plan. Only when
	// both are known: a destination from a resource not yet created is unknown.
	if cfg.Cadence.IsUnknown() || cfg.ExportDestinationID.IsUnknown() {
		return
	}
	if c := cfg.Cadence.ValueString(); c != "" && c != "off" && cfg.ExportDestinationID.IsNull() {
		res.Diagnostics.AddAttributeError(path.Root("export_destination_id"), "A schedule needs a destination",
			"Scheduled exports are delivered to a destination; set export_destination_id, or cadence = \"off\".")
	}
}

func (r *instanceExportResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
	client, ok := clientFrom(req.ProviderData, &res.Diagnostics)
	if ok {
		r.client = client
	}
}

func (r *instanceExportResource) set(ctx context.Context, plan *instanceExportModel, res diagnosticsHolder) bool {
	cfg := ExportConfig{KdbExportCadence: plan.Cadence.ValueString()}
	if cfg.KdbExportCadence == "" {
		cfg.KdbExportCadence = "off"
	}
	if !plan.ExportDestinationID.IsNull() && !plan.ExportDestinationID.IsUnknown() {
		id := plan.ExportDestinationID.ValueString()
		cfg.ExportDestinationID = &id
	}
	instanceID := plan.InstanceID.ValueString()
	if _, err := r.client.SetExportConfig(ctx, instanceID, cfg); err != nil {
		res.AddError("Could not set the database's exports", explainBackup(err))
		return false
	}
	inst, err := r.client.GetInstanceExport(ctx, instanceID)
	if err != nil {
		res.AddError("Could not read the database's exports back", explainBackup(err))
		return false
	}
	plan.ID = types.StringValue(inst.ID)
	plan.Cadence = types.StringValue(inst.KdbExportCadence)
	plan.ExportDestinationID = keepIDCase(plan.ExportDestinationID, inst.ExportDestinationID)
	if inst.FreeTier && inst.KdbExportCadence != "off" {
		res.AddWarning("Scheduled exports do not run on a free-tier database",
			"The schedule is saved, but KiviDB does not export free-tier databases on a schedule. "+
				"An export taken with kividb_kdb_snapshot is still delivered to the destination.")
	}
	return true
}

// diagnosticsHolder is the part of a response set() writes to, so Create and
// Update can share it.
type diagnosticsHolder interface {
	AddError(summary, detail string)
	AddWarning(summary, detail string)
}

// keepIDCase returns the configured id when the server's is the same id in
// another case: Terraform requires a configured value back exactly as written.
func keepIDCase(configured types.String, server *string) types.String {
	if server == nil || *server == "" {
		return types.StringNull()
	}
	if !configured.IsNull() && !configured.IsUnknown() && strings.EqualFold(configured.ValueString(), *server) {
		return configured
	}
	return types.StringValue(*server)
}

func (r *instanceExportResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan instanceExportModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	if !r.set(ctx, &plan, &res.Diagnostics) {
		return
	}
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *instanceExportResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state instanceExportModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.InstanceID.ValueString()
	if id == "" {
		id = state.ID.ValueString()
	}
	if id == "" {
		res.State.RemoveResource(ctx)
		return
	}
	inst, err := r.client.GetInstanceExport(ctx, id)
	if err != nil {
		if isBackupNotFound(err) {
			// The database is gone, and its export configuration with it.
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the database's exports", explainBackup(err))
		return
	}
	state.ID = types.StringValue(inst.ID)
	if state.InstanceID.IsNull() || !strings.EqualFold(state.InstanceID.ValueString(), inst.ID) {
		state.InstanceID = types.StringValue(inst.ID)
	}
	state.Cadence = types.StringValue(inst.KdbExportCadence)
	state.ExportDestinationID = keepIDCase(state.ExportDestinationID, inst.ExportDestinationID)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *instanceExportResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan instanceExportModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	if !r.set(ctx, &plan, &res.Diagnostics) {
		return
	}
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

// Delete turns exports off: no destination, no schedule. A database that is
// already gone has nothing to turn off.
func (r *instanceExportResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state instanceExportModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	_, err := r.client.SetExportConfig(ctx, state.InstanceID.ValueString(), ExportConfig{KdbExportCadence: "off"})
	if err != nil && !isBackupNotFound(err) {
		res.Diagnostics.AddError("Could not turn the database's exports off", explainBackup(err))
	}
}

// ImportState takes the database's id.
func (r *instanceExportResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("instance_id"), req.ID)...)
}
