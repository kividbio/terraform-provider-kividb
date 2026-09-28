package provider

import (
	"context"
	"errors"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*diskSnapshotResource)(nil)
	_ resource.ResourceWithConfigure   = (*diskSnapshotResource)(nil)
	_ resource.ResourceWithImportState = (*diskSnapshotResource)(nil)
)

func NewDiskSnapshotResource() resource.Resource { return &diskSnapshotResource{} }

type diskSnapshotResource struct{ client *Client }

type diskSnapshotModel struct {
	ID           types.String  `tfsdk:"id"`
	InstanceID   types.String  `tfsdk:"instance_id"`
	Label        types.String  `tfsdk:"label"`
	WaitForReady types.Bool    `tfsdk:"wait_for_ready"`
	Status       types.String  `tfsdk:"status"`
	SizeGB       types.Float64 `tfsdk:"size_gb"`
	Region       types.String  `tfsdk:"region"`
	Cloud        types.String  `tfsdk:"cloud"`
	InstanceName types.String  `tfsdk:"instance_name"`
	CreatedAt    types.String  `tfsdk:"created_at"`
}

func (r *diskSnapshotResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_disk_snapshot"
}

func (r *diskSnapshotResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "A point-in-time copy of a database's data volume.\n\n" +
			"A snapshot is taken once and does not change afterwards, so everything about it " +
			"except its label is fixed at creation. Pointing `instance_id` at a different " +
			"database does not move this snapshot: it plans a new one and destroys this.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Snapshot id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database to snapshot.",
				// A snapshot belongs to the volume it was copied from. Nothing
				// re-points one, so changing this is a new snapshot rather than
				// an edit to this one.
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"label": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "A name for the snapshot. The server generates one when this " +
					"is not set, which is why it is also computed.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"wait_for_ready": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Wait for the volume copy to finish before the apply returns. " +
					"Defaults to true, because a snapshot still being written cannot be restored " +
					"from, and an apply that returned success would be saying otherwise.",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Snapshot state as the server reports it.",
			},
			"size_gb": schema.Float64Attribute{
				Computed:            true,
				MarkdownDescription: "Size of the captured volume, once the copy reports one.",
			},
			"region": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Region the snapshot is stored in.",
			},
			"cloud": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Cloud the snapshot is stored in.",
			},
			"instance_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Name the database had when the snapshot was taken.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the snapshot was requested.",
			},
		},
	}
}

func (r *diskSnapshotResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		res.Diagnostics.AddError("Unexpected provider data", "Could not read the configured client.")
		return
	}
	r.client = client
}

// apply copies the server's answer over the model, so every computed attribute
// comes from the row rather than from what was asked for.
func (m *diskSnapshotModel) apply(s *DiskSnapshot) {
	m.ID = types.StringValue(s.ID)
	m.InstanceID = types.StringValue(s.InstanceID)
	m.Label = types.StringValue(s.Label)
	m.Status = types.StringValue(s.Status)
	m.Region = types.StringValue(s.Region)
	m.Cloud = types.StringValue(s.Cloud)
	m.InstanceName = types.StringValue(s.InstanceName)
	m.CreatedAt = types.StringValue(s.CreatedAt)
	if s.SizeGB.Value != nil {
		m.SizeGB = types.Float64Value(*s.SizeGB.Value)
	} else {
		// Null rather than zero: the copy has not reported a size yet, and 0 GB
		// is a claim about the volume rather than an admission of not knowing.
		m.SizeGB = types.Float64Null()
	}
}

// settle polls until the copy finishes.
//
// Returns the last snapshot it saw even on failure, so a timeout still writes a
// real id into state. A snapshot the server is holding and Terraform has
// forgotten is one nobody deletes and everybody keeps paying for.
func (r *diskSnapshotResource) settle(ctx context.Context, id string) (*DiskSnapshot, error) {
	deadline := time.Now().Add(45 * time.Minute)
	var last *DiskSnapshot
	for {
		s, err := r.client.GetDiskSnapshot(ctx, id)
		if err != nil {
			return last, err
		}
		last = s
		switch s.Status {
		case "ready", "completed", "available":
			return s, nil
		case "failed", "error":
			msg := "the snapshot failed"
			if s.ErrorMessage != nil && *s.ErrorMessage != "" {
				msg = *s.ErrorMessage
			}
			return s, errors.New(msg)
		}
		if time.Now().After(deadline) {
			return last, errors.New(
				"the snapshot was still being written after 45 minutes; it may yet finish, and its id is in state")
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func (r *diskSnapshotResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan diskSnapshotModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}

	snap, err := r.client.CreateDiskSnapshot(ctx, plan.InstanceID.ValueString(), plan.Label.ValueString())
	if err != nil {
		res.Diagnostics.AddError("Could not take the snapshot", explain(err))
		return
	}
	plan.apply(snap)

	if plan.WaitForReady.IsNull() || plan.WaitForReady.ValueBool() {
		settled, werr := r.settle(ctx, snap.ID)
		// State is written either way once the server has given us a snapshot:
		// it exists and it is billable, so losing its id would strand it.
		if settled != nil {
			plan.apply(settled)
		}
		if werr != nil {
			res.Diagnostics.AddError("The snapshot did not finish", explain(werr))
			res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
			return
		}
	}
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *diskSnapshotResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state diskSnapshotModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if state.ID.ValueString() == "" {
		res.State.RemoveResource(ctx)
		return
	}
	snap, err := r.client.GetDiskSnapshot(ctx, state.ID.ValueString())
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			// Deleted outside Terraform, or aged out by a retention policy.
			// Drop it rather than failing every plan from here on.
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the snapshot", explain(err))
		return
	}
	state.apply(snap)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *diskSnapshotResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan, state diskSnapshotModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	// The label is the only part of a snapshot that can change. Everything else
	// describes a copy that was already taken.
	if !plan.Label.Equal(state.Label) && !plan.Label.IsUnknown() {
		if err := r.client.RenameDiskSnapshot(ctx, id, plan.Label.ValueString()); err != nil {
			res.Diagnostics.AddError("Could not rename the snapshot", explain(err))
			return
		}
	}
	snap, err := r.client.GetDiskSnapshot(ctx, id)
	if err != nil {
		res.Diagnostics.AddError("Could not read the snapshot back", explain(err))
		return
	}
	plan.apply(snap)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *diskSnapshotResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state diskSnapshotModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteDiskSnapshot(ctx, state.ID.ValueString()); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			return
		}
		res.Diagnostics.AddError("Could not delete the snapshot", explain(err))
	}
}

func (r *diskSnapshotResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, res)
}
