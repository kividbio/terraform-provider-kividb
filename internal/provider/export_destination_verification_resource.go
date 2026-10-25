package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*exportDestinationVerificationResource)(nil)
	_ resource.ResourceWithConfigure   = (*exportDestinationVerificationResource)(nil)
	_ resource.ResourceWithImportState = (*exportDestinationVerificationResource)(nil)
)

func NewExportDestinationVerificationResource() resource.Resource {
	return &exportDestinationVerificationResource{}
}

// exportDestinationVerificationResource is the act of verifying a destination,
// kept apart from the destination for the reason given on
// exportDestinationResource: the role it checks is built from the
// destination's outputs.
//
// A verification that does not pass fails the apply with the API's own words
// and records nothing. The destination is not left orphaned by that: it is in
// state as kividb_export_destination, with status "failed" and the reason in
// last_error, and the next apply verifies again.
type exportDestinationVerificationResource struct{ client *Client }

type exportDestinationVerificationModel struct {
	ID            types.String `tfsdk:"id"`
	DestinationID types.String `tfsdk:"destination_id"`
	Triggers      types.Map    `tfsdk:"triggers"`
	Status        types.String `tfsdk:"status"`
	VerifiedAt    types.String `tfsdk:"verified_at"`
	BucketRegion  types.String `tfsdk:"bucket_region"`
}

func (r *exportDestinationVerificationResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_export_destination_verification"
}

func (r *exportDestinationVerificationResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "Verifies a `kividb_export_destination`: KiviDB assumes the role with the destination's " +
			"external id, confirms the role is in the account named, finds the bucket as owned by that account, " +
			"and writes a small test file (`<prefix>.kividb-verify`) under the prefix. A destination receives " +
			"exports only once this has passed.\n\n" +
			"Make it depend on the role and its policy, so it runs after they exist. IAM takes a little while " +
			"to make a new role assumable, so a failure is retried for up to five minutes before the apply " +
			"fails with the API's reason. Destroying this resource changes nothing in KiviDB: a verified " +
			"destination stays verified.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The destination's id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"destination_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The `id` of the `kividb_export_destination` to verify.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.RegexMatches(uuidPattern, "must be an export destination id (a UUID)")},
			},
			"triggers": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Any values that should make Terraform verify again when they change -- the role's " +
					"policies, for example. Changing one replaces this resource, which verifies afresh.",
				PlanModifiers: []planmodifier.Map{mapplanmodifier.RequiresReplace()},
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`verified`. A destination that is no longer verified is removed from state, so the next plan verifies it again.",
			},
			"verified_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the destination passed verification.",
			},
			"bucket_region": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The bucket's region, as verification found it.",
			},
		},
	}
}

func (r *exportDestinationVerificationResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
	client, ok := clientFrom(req.ProviderData, &res.Diagnostics)
	if ok {
		r.client = client
	}
}

func (m *exportDestinationVerificationModel) apply(d *ExportDestination) {
	m.ID = types.StringValue(d.ID)
	m.Status = types.StringValue(d.Status)
	m.VerifiedAt = stringOrNull(d.VerifiedAt)
	m.BucketRegion = stringOrNull(d.BucketRegion)
}

// verify runs the check, again while it fails, until it passes or the window
// closes. Only a verification that ran and did not pass is retried: that is
// what a role IAM has not finished propagating looks like. Anything else -- the
// destination gone, the key refused -- will not change by waiting.
func (r *exportDestinationVerificationResource) verify(ctx context.Context, id string) (*ExportDestination, error) {
	deadline := time.Now().Add(verifyRetryFor)
	for {
		d, err := r.client.VerifyExportDestination(ctx, id)
		if err == nil {
			return d, nil
		}
		var ve *VerificationError
		if !errors.As(err, &ve) || time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(verifyRetryEvery):
		}
	}
}

func (r *exportDestinationVerificationResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan exportDestinationVerificationModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	d, err := r.verify(ctx, plan.DestinationID.ValueString())
	if err != nil {
		var ve *VerificationError
		if errors.As(err, &ve) {
			res.Diagnostics.AddAttributeError(path.Root("destination_id"), "The export destination did not pass verification",
				fmt.Sprintf("%s\n\nStopped at: %s. Nothing is delivered to this destination until it passes. "+
					"The destination itself is unchanged and still in state; fix the role or the bucket and "+
					"apply again to verify it again.", ve.Message, ve.Step))
			return
		}
		res.Diagnostics.AddError("Could not verify the export destination", explainBackup(err))
		return
	}
	plan.apply(d)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *exportDestinationVerificationResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state exportDestinationVerificationModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.DestinationID.ValueString()
	if id == "" {
		// An import names the destination through the id.
		id = state.ID.ValueString()
		state.DestinationID = state.ID
	}
	d, err := r.client.GetExportDestination(ctx, id)
	if err != nil {
		if isBackupNotFound(err) {
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the export destination", explainBackup(err))
		return
	}
	if d.Status != "verified" {
		// Verified once and failed since (re-verified in the console against a
		// role that has changed). Forgetting the verification is what makes the
		// next plan run it again, instead of reporting a destination that no
		// longer receives anything as fine.
		res.State.RemoveResource(ctx)
		return
	}
	state.apply(d)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// Update only carries state forward: every argument that matters replaces.
func (r *exportDestinationVerificationResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan, state exportDestinationVerificationModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	plan.ID, plan.Status, plan.VerifiedAt, plan.BucketRegion = state.ID, state.Status, state.VerifiedAt, state.BucketRegion
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

// Delete forgets the verification. There is no un-verifying a destination;
// deleting the kividb_export_destination is what stops deliveries.
func (r *exportDestinationVerificationResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
}

func (r *exportDestinationVerificationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, res)
}
