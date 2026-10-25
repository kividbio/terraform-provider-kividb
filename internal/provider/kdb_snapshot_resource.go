package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*kdbSnapshotResource)(nil)
	_ resource.ResourceWithConfigure   = (*kdbSnapshotResource)(nil)
	_ resource.ResourceWithImportState = (*kdbSnapshotResource)(nil)
)

func NewKdbSnapshotResource() resource.Resource { return &kdbSnapshotResource{} }

// kdbSnapshotResource is one on-demand KDB export: the database's data as a
// portable .kdb file. Nothing about an export can change once taken.
type kdbSnapshotResource struct{ client *Client }

type kdbSnapshotModel struct {
	ID               types.String `tfsdk:"id"`
	InstanceID       types.String `tfsdk:"instance_id"`
	Label            types.String `tfsdk:"label"`
	Deliver          types.Bool   `tfsdk:"deliver"`
	WaitForReady     types.Bool   `tfsdk:"wait_for_ready"`
	Status           types.String `tfsdk:"status"`
	SizeBytes        types.Int64  `tfsdk:"size_bytes"`
	InstanceName     types.String `tfsdk:"instance_name"`
	Cloud            types.String `tfsdk:"cloud"`
	Region           types.String `tfsdk:"region"`
	TriggeredBy      types.String `tfsdk:"triggered_by"`
	CreatedAt        types.String `tfsdk:"created_at"`
	CompletedAt      types.String `tfsdk:"completed_at"`
	ExpiresAt        types.String `tfsdk:"expires_at"`
	CloudAccountID   types.String `tfsdk:"cloud_account_id"`
	CloudAccountName types.String `tfsdk:"cloud_account_name"`
	DeliveryStatus   types.String `tfsdk:"delivery_status"`
	DeliveredURI     types.String `tfsdk:"delivered_uri"`
	DeliveryError    types.String `tfsdk:"delivery_error"`
}

func (r *kdbSnapshotResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_kdb_snapshot"
}

func (r *kdbSnapshotResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	computed := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	res.Schema = schema.Schema{
		MarkdownDescription: "A KDB export of a database: its data written out as a portable `.kdb` file, taken " +
			"on demand.\n\n" +
			"An export is taken once and never changes, so changing any argument plans a new one and " +
			"destroys this. Destroying it deletes the file from KiviDB's storage; a copy already delivered " +
			"to your own bucket is yours and stays. A new database can be made from it with " +
			"`kividb_instance.restore_from_kdb_snapshot_id`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Snapshot id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database to export. It must be running.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"label": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "A name for the export, up to 128 characters. The server generates one when this " +
					"is not set. There is no renaming an export, so changing it takes a new one.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplaceIfConfigured(), stringplanmodifier.UseStateForUnknown()},
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 128),
					stringvalidator.RegexMatches(noOuterSpaceRe,
						"must not begin or end with whitespace (the API trims it, and the label would never read back as written)"),
				},
			},
			"deliver": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Copy the export into the database's export destination (`kividb_instance_export`), " +
					"if it has a verified one. Defaults to true. Set false to keep this one in KiviDB's storage only.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"wait_for_ready": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Wait for the file to be written before the apply returns, for up to an hour. " +
					"Defaults to true, because an export still being written cannot be restored from or deleted. " +
					"Delivery to your bucket happens after that and is not waited for; see `delivery_status`.",
			},
			"status":        computed("`creating`, `ready`, `failed`, or `moving` while it is copied to another region."),
			"size_bytes":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Size of the file, once written."},
			"instance_name": computed("Name the database had when it was exported."),
			"cloud":         computed("Cloud the file is stored in: the database's."),
			"region":        computed("Region the file is stored in: the database's. A database made from it must be in this region."),
			"triggered_by":  computed("`manual` for an export taken here."),
			"created_at":    computed("When the export was requested."),
			"completed_at":  computed("When the file was written."),
			"expires_at": computed("When KiviDB deletes its copy on its own, if ever. Null for an export taken " +
				"here: it is kept until destroyed."),
			"cloud_account_id": computed("The cloud account the file is stored in: the database's own. Null for " +
				"KiviDB's cloud. A database made from this export is placed in this account."),
			"cloud_account_name": computed("The name of that cloud account, as the KiviDB console shows it."),
			"delivery_status": computed("Copy into your bucket: `pending`, `delivered` or `failed`. Null when the " +
				"export has no destination."),
			"delivered_uri":  computed("Where the copy was written in your bucket (`s3://...`), once delivered."),
			"delivery_error": computed("Why the copy into your bucket failed, as the API reported it."),
		},
	}
}

func (r *kdbSnapshotResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
	client, ok := clientFrom(req.ProviderData, &res.Diagnostics)
	if ok {
		r.client = client
	}
}

func (m *kdbSnapshotModel) apply(s *KdbSnapshot) {
	m.ID = types.StringValue(s.ID)
	if s.InstanceID != nil && *s.InstanceID != "" &&
		(m.InstanceID.IsNull() || m.InstanceID.IsUnknown() || !strings.EqualFold(m.InstanceID.ValueString(), *s.InstanceID)) {
		m.InstanceID = types.StringValue(*s.InstanceID)
	}
	// A database deleted since leaves the export's instance_id null. The export
	// is still there, and still this resource; instance_id keeps what it was.
	m.Label = stringOrNull(s.Label)
	m.Status = types.StringValue(s.Status)
	m.SizeBytes = int64OrNull(s.SizeBytes)
	m.InstanceName = stringOrNull(s.InstanceName)
	m.Cloud = stringOrNull(s.Cloud)
	m.Region = stringOrNull(s.Region)
	m.TriggeredBy = types.StringValue(s.TriggeredBy)
	m.CreatedAt = types.StringValue(s.CreatedAt)
	m.CompletedAt = stringOrNull(s.CompletedAt)
	m.ExpiresAt = stringOrNull(s.TTD)
	m.CloudAccountID = stringOrNull(s.CloudAccountID)
	m.CloudAccountName = stringOrNull(s.CloudAccountName)
	m.DeliveryStatus = stringOrNull(s.DeliveryStatus)
	m.DeliveredURI = stringOrNull(s.DeliveredURI)
	m.DeliveryError = stringOrNull(s.DeliveryError)
}

// settle polls until the export has been written, or has failed. It returns the
// last row it saw on every path, so a timeout still records a real id.
func (r *kdbSnapshotResource) settle(ctx context.Context, id string) (*KdbSnapshot, error) {
	deadline := time.Now().Add(kdbSettleFor)
	var last *KdbSnapshot
	for {
		s, err := r.client.GetKdbSnapshot(ctx, id)
		if err != nil {
			return last, err
		}
		last = s
		switch s.Status {
		case "ready", "moving":
			return s, nil
		case "failed":
			msg := "the export failed"
			if s.ErrorMessage != nil && *s.ErrorMessage != "" {
				msg = *s.ErrorMessage
			}
			return s, errors.New(msg)
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("the export was still being written after %s; it may yet finish, and its id is in state", kdbSettleFor)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(kdbSettleEvery):
		}
	}
}

func (r *kdbSnapshotResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan kdbSnapshotModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	body := ExportKdbSnapshotRequest{}
	if !plan.Label.IsNull() && !plan.Label.IsUnknown() {
		l := plan.Label.ValueString()
		body.Label = &l
	}
	if !plan.Deliver.IsNull() && !plan.Deliver.IsUnknown() {
		d := plan.Deliver.ValueBool()
		body.Deliver = &d
	}
	id, err := r.client.ExportKdbSnapshot(ctx, plan.InstanceID.ValueString(), body)
	if err != nil {
		res.Diagnostics.AddError("Could not export the database", explainBackup(err))
		return
	}
	plan.ID = types.StringValue(id)

	if !plan.WaitForReady.IsNull() && !plan.WaitForReady.ValueBool() {
		s, err := r.client.GetKdbSnapshot(ctx, id)
		if err != nil {
			res.Diagnostics.AddError("The export was queued but could not be read back", explainBackup(err))
			r.saveTainted(ctx, &plan, res)
			return
		}
		plan.apply(s)
		res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
		return
	}

	s, werr := r.settle(ctx, id)
	if s != nil {
		plan.apply(s)
	}
	if werr == nil {
		res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
		return
	}
	if s != nil && s.Status == "failed" {
		// A failed export holds no file and can be deleted, so it is: recording
		// it would only leave a row to destroy by hand, and leaving it out of
		// state with the row still there would be an orphan.
		if derr := r.client.DeleteKdbSnapshot(ctx, id); derr != nil && !isBackupNotFound(derr) {
			res.Diagnostics.AddError("The export failed", werr.Error()+
				"\n\nThe failed export could not be removed either ("+explainBackup(derr)+"); it is kept in state so a destroy removes it.")
			r.saveTainted(ctx, &plan, res)
			return
		}
		res.Diagnostics.AddError("The export failed", werr.Error())
		return
	}
	// Still being written, or the poll itself failed. The export exists; the
	// id goes into state with the error, so Terraform marks it tainted and the
	// next apply replaces it rather than forgetting it.
	res.Diagnostics.AddError("The export did not finish", explainBackup(werr))
	r.saveTainted(ctx, &plan, res)
}

// saveTainted writes what is known with every unknown made null. Terraform
// records a resource whose create returned an error together with state as
// tainted.
func (r *kdbSnapshotResource) saveTainted(ctx context.Context, plan *kdbSnapshotModel, res *resource.CreateResponse) {
	for _, s := range []*types.String{&plan.Label, &plan.Status, &plan.InstanceName, &plan.Cloud, &plan.Region,
		&plan.TriggeredBy, &plan.CreatedAt, &plan.CompletedAt, &plan.ExpiresAt, &plan.CloudAccountID,
		&plan.CloudAccountName, &plan.DeliveryStatus, &plan.DeliveredURI, &plan.DeliveryError} {
		if s.IsUnknown() {
			*s = types.StringNull()
		}
	}
	if plan.SizeBytes.IsUnknown() {
		plan.SizeBytes = types.Int64Null()
	}
	res.Diagnostics.Append(res.State.Set(ctx, plan)...)
}

func (r *kdbSnapshotResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state kdbSnapshotModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if state.ID.ValueString() == "" {
		res.State.RemoveResource(ctx)
		return
	}
	s, err := r.client.GetKdbSnapshot(ctx, state.ID.ValueString())
	if err != nil {
		if isBackupNotFound(err) {
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the export", explainBackup(err))
		return
	}
	state.apply(s)
	if state.Deliver.IsNull() {
		// An import. Whether delivery was asked for is not recorded anywhere
		// (an export without a destination looks the same either way), so it
		// reads as the default, which is what keeps an import from planning a
		// replacement of the export it just adopted.
		state.Deliver = types.BoolValue(true)
	}
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// Update only changes wait_for_ready, which is the provider's and not the
// API's; everything else replaces.
func (r *kdbSnapshotResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan, state kdbSnapshotModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	s, err := r.client.GetKdbSnapshot(ctx, state.ID.ValueString())
	if err != nil {
		res.Diagnostics.AddError("Could not read the export back", explainBackup(err))
		return
	}
	plan.ID = state.ID
	plan.apply(s)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

// Delete removes the file. An export still being written cannot be deleted
// (409 snapshot_in_progress), so that one is waited out, for a bounded time,
// and deleted when it finishes.
func (r *kdbSnapshotResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state kdbSnapshotModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	deadline := time.Now().Add(kdbDeleteWaitFor)
	for {
		err := r.client.DeleteKdbSnapshot(ctx, id)
		if err == nil || isBackupNotFound(err) {
			return
		}
		var apiErr *APIError
		inProgress := errors.As(err, &apiErr) && apiErr.Code == "snapshot_in_progress"
		if !inProgress || time.Now().After(deadline) {
			res.Diagnostics.AddError("Could not delete the export", explainBackup(err))
			return
		}
		select {
		case <-ctx.Done():
			res.Diagnostics.AddError("Could not delete the export", ctx.Err().Error())
			return
		case <-time.After(kdbDeleteWaitEvery):
		}
	}
}

func (r *kdbSnapshotResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, res)
}
