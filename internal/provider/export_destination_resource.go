package provider

import (
	"context"

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
	_ resource.Resource                = (*exportDestinationResource)(nil)
	_ resource.ResourceWithConfigure   = (*exportDestinationResource)(nil)
	_ resource.ResourceWithImportState = (*exportDestinationResource)(nil)
)

func NewExportDestinationResource() resource.Resource { return &exportDestinationResource{} }

// exportDestinationResource is a bucket in the customer's own AWS account that
// KDB exports are delivered to.
//
// Creating one does not verify it, and cannot: verification assumes a role
// whose trust policy names the destination's external id, and the API mints
// that id when the destination is created. So the role is created from this
// resource's outputs, and kividb_export_destination_verification runs the check
// once it exists. Verifying here would fail on every first apply.
type exportDestinationResource struct{ client *Client }

type exportDestinationModel struct {
	ID               types.String `tfsdk:"id"`
	Name             types.String `tfsdk:"name"`
	AWSAccountID     types.String `tfsdk:"aws_account_id"`
	Bucket           types.String `tfsdk:"bucket"`
	Prefix           types.String `tfsdk:"prefix"`
	RoleName         types.String `tfsdk:"role_name"`
	Cloud            types.String `tfsdk:"cloud"`
	RoleArn          types.String `tfsdk:"role_arn"`
	ExternalID       types.String `tfsdk:"external_id"`
	TrustPolicy      types.String `tfsdk:"trust_policy"`
	PermissionPolicy types.String `tfsdk:"permission_policy"`
	Status           types.String `tfsdk:"status"`
	VerifiedAt       types.String `tfsdk:"verified_at"`
	LastError        types.String `tfsdk:"last_error"`
	BucketRegion     types.String `tfsdk:"bucket_region"`
	CreatedAt        types.String `tfsdk:"created_at"`
}

func (r *exportDestinationResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_export_destination"
}

func (r *exportDestinationResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	res.Schema = schema.Schema{
		MarkdownDescription: "An S3 bucket in your own AWS account that KDB exports are delivered to.\n\n" +
			"KiviDB writes into it by assuming a role you create, with an external id unique to this " +
			"destination. The role's trust policy and permission policy are attributes of this resource " +
			"(`trust_policy`, `permission_policy`), so the role can be created from them in the same " +
			"configuration. Nothing is delivered until the destination is verified: add a " +
			"`kividb_export_destination_verification` that depends on the role.\n\n" +
			"The API cannot change a destination, so every argument forces a new one -- with a new " +
			"external id, which the role's trust policy then follows.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Destination id. Pass it to `kividb_instance_export.export_destination_id`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "A name for the destination, as the console shows it. 1 to 80 characters.",
				PlanModifiers:       replace,
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 80),
					stringvalidator.RegexMatches(noOuterSpaceRe,
						"must not begin or end with whitespace (the API trims it, and the name would never read back as written)"),
				},
			},
			"aws_account_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The 12-digit id of the AWS account the bucket and the role are in.",
				PlanModifiers:       replace,
				Validators:          []validator.String{stringvalidator.RegexMatches(awsAccountIDRe, "must be a 12-digit AWS account id")},
			},
			"bucket": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The bucket's name. It must be owned by `aws_account_id`; verification checks " +
					"the owner, so a bucket of the same name in another account is refused.",
				PlanModifiers: replace,
				Validators:    []validator.String{stringvalidator.RegexMatches(exportBucketRe, "must be an S3 bucket name")},
			},
			"prefix": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultExportPrefix),
				MarkdownDescription: "Where in the bucket exports are written: `\"\"` for the bucket's root, or a " +
					"path ending in `/`. Defaults to `kividb/`. The role may write only under it.",
				PlanModifiers: replace,
				Validators: []validator.String{stringvalidator.RegexMatches(exportPrefixRe,
					`must be "" or a path of up to 200 characters ending in "/"`)},
			},
			"role_name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The name of the role KiviDB assumes. It must start with `KiviDBManagement-Exports`. " +
					"Defaults to `KiviDBManagement-Exports-` and the first 8 characters of the destination's id. " +
					"Role names are unique within an AWS account, so two destinations in one account need two roles.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplaceIfConfigured(), stringplanmodifier.UseStateForUnknown()},
				Validators: []validator.String{
					stringvalidator.LengthAtMost(64),
					stringvalidator.RegexMatches(exportRoleNameRe, "must start with KiviDBManagement-Exports and use only the characters IAM allows in a role name"),
				},
			},
			"cloud": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Always `aws`: exports are delivered only to S3 for now.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"role_arn": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The ARN of the role KiviDB assumes: `role_name` in `aws_account_id`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"external_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The external id KiviDB presents when it assumes the role, unique to this destination. " +
					"It is what stops another KiviDB organization naming your bucket. Already part of `trust_policy`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"trust_policy": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The role's trust policy, as JSON: KiviDB's API and worker roles may assume it, only " +
					"with `external_id`. Use it as the role's `assume_role_policy`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"permission_policy": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The role's permission policy, as JSON: write and complete uploads under " +
					"`bucket`/`prefix`, and read the bucket's location. Attach it to the role. If the bucket " +
					"is encrypted with a KMS key of your own, the role also needs `kms:GenerateDataKey` on that key.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`pending` until verified, `verified` once KiviDB has written a test file under the " +
					"prefix, or `failed` with the reason in `last_error`. Only a verified destination receives exports.",
			},
			"verified_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the destination last passed verification.",
			},
			"last_error": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Why the last verification failed, as the API reported it. Null once verified.",
			},
			"bucket_region": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The bucket's region, found by verification. Null until then.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the destination was created.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *exportDestinationResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
	client, ok := clientFrom(req.ProviderData, &res.Diagnostics)
	if ok {
		r.client = client
	}
}

func (m *exportDestinationModel) apply(d *ExportDestination) {
	m.ID = types.StringValue(d.ID)
	m.Name = types.StringValue(d.Name)
	m.AWSAccountID = types.StringValue(d.AWSAccountID)
	m.Bucket = types.StringValue(d.Bucket)
	m.Prefix = types.StringValue(d.Prefix)
	m.RoleName = types.StringValue(d.Setup.RoleName)
	m.Cloud = types.StringValue(d.Cloud)
	m.RoleArn = types.StringValue(d.RoleArn())
	m.ExternalID = types.StringValue(d.Setup.ExternalID)
	m.TrustPolicy = types.StringValue(compactJSON(d.Setup.TrustPolicy))
	m.PermissionPolicy = types.StringValue(compactJSON(d.Setup.PermissionPolicy))
	m.Status = types.StringValue(d.Status)
	m.VerifiedAt = stringOrNull(d.VerifiedAt)
	m.LastError = stringOrNull(d.LastError)
	m.BucketRegion = stringOrNull(d.BucketRegion)
	m.CreatedAt = types.StringValue(d.CreatedAt)
}

func (r *exportDestinationResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan exportDestinationModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	body := CreateExportDestinationRequest{
		Name:         plan.Name.ValueString(),
		AWSAccountID: plan.AWSAccountID.ValueString(),
		Bucket:       plan.Bucket.ValueString(),
	}
	if !plan.Prefix.IsNull() && !plan.Prefix.IsUnknown() {
		p := plan.Prefix.ValueString()
		body.Prefix = &p
	}
	if !plan.RoleName.IsNull() && !plan.RoleName.IsUnknown() {
		n := plan.RoleName.ValueString()
		body.RoleName = &n
	}
	d, err := r.client.CreateExportDestination(ctx, body)
	if err != nil {
		res.Diagnostics.AddError("Could not create the export destination", explainBackup(err))
		return
	}
	plan.apply(d)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *exportDestinationResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state exportDestinationModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if state.ID.ValueString() == "" {
		res.State.RemoveResource(ctx)
		return
	}
	d, err := r.client.GetExportDestination(ctx, state.ID.ValueString())
	if err != nil {
		if isBackupNotFound(err) {
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the export destination", explainBackup(err))
		return
	}
	state.apply(d)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// Update has nothing to send: every argument forces replacement, and the API
// has no way to change a destination. It only carries the state forward.
func (r *exportDestinationResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var state exportDestinationModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	d, err := r.client.GetExportDestination(ctx, state.ID.ValueString())
	if err != nil {
		res.Diagnostics.AddError("Could not read the export destination back", explainBackup(err))
		return
	}
	state.apply(d)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *exportDestinationResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state exportDestinationModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	// Databases that deliver here lose their destination, and any schedule
	// they had is switched off by the API: there would be nowhere to put it.
	if err := r.client.DeleteExportDestination(ctx, state.ID.ValueString()); err != nil && !isBackupNotFound(err) {
		res.Diagnostics.AddError("Could not delete the export destination", explainBackup(err))
	}
}

func (r *exportDestinationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, res)
}
