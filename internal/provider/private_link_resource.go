package provider

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*privateLinkResource)(nil)
	_ resource.ResourceWithConfigure   = (*privateLinkResource)(nil)
	_ resource.ResourceWithImportState = (*privateLinkResource)(nil)
)

func NewPrivateLinkResource() resource.Resource {
	return &privateLinkResource{readyTimeout: 20 * time.Minute, pollInterval: 10 * time.Second}
}

type privateLinkResource struct {
	client *Client

	// How long to wait for the endpoint service, and how often to look.
	// Fields so a test can shorten them.
	readyTimeout time.Duration
	pollInterval time.Duration
}

type privateLinkModel struct {
	ID                  types.String `tfsdk:"id"`
	InstanceID          types.String `tfsdk:"instance_id"`
	AllowedPrincipals   types.Set    `tfsdk:"allowed_principals"`
	PublicAccessBlocked types.Bool   `tfsdk:"public_access_blocked"`
	WaitForReady        types.Bool   `tfsdk:"wait_for_ready"`
	ServiceName         types.String `tfsdk:"service_name"`
	Region              types.String `tfsdk:"region"`
	Ports               types.List   `tfsdk:"ports"`
}

var privateLinkPortType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"port":  types.Int64Type,
	"role":  types.StringType,
	"tls":   types.BoolType,
	"label": types.StringType,
}}

// Who may connect, in each cloud's own terms: an AWS principal ARN, an Azure
// subscription id, or a Google Cloud project id or number. Which one a
// database takes depends on its cloud, which the API checks; this only turns
// away something that is none of them.
var privateLinkPrincipal = regexp.MustCompile(`^(arn:aws[a-z-]*:iam::\d{12}:(root|(user|role)/.+)|` +
	`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|` +
	`[a-z][a-z0-9-]{4,28}[a-z0-9]|\d{1,20})$`)

func (r *privateLinkResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_private_link"
}

func (r *privateLinkResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: `Private connectivity to a database: an endpoint service your own network
connects to without crossing the internet -- AWS PrivateLink, Azure Private
Link Service or Google Cloud Private Service Connect, by the database's cloud.

**It is a paid add-on, $25 a month per database**, billed from the moment this
resource is created until it is destroyed. It is not available on the free
tier, and not needed for a database in your own cloud account
(` + "`cloud_account_id`" + `), which is already inside your network; the API refuses
both.

Creating it builds the endpoint service, which takes three to four minutes; the
apply waits for ` + "`service_name`" + `, the name you create your endpoint against.
Only the principals in ` + "`allowed_principals`" + ` may connect to it: an empty list is
a service nobody can reach yet.

With it in place, ` + "`public_access_blocked = true`" + ` closes the public endpoint
entirely, so the database is reachable only through your private endpoint.

**Destroying this resource turns private connectivity off and stops the
charge.** Endpoints created against it stop working. If public access was
blocked, it is opened again first -- to the addresses in the allowlist, not to
everyone -- because a database with neither path is unreachable, and the API
refuses to leave it that way.`,
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The database's id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database. **Changing this turns private connectivity off on the old database and on for the new one.**",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"allowed_principals": schema.SetAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Default:     setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
				MarkdownDescription: "Who may connect, at most 50, in the database's cloud's own terms: on AWS, " +
					"principal ARNs (`arn:aws:iam::123456789012:root`, `:role/<name>` or `:user/<name>`); on " +
					"Azure, subscription ids; on Google Cloud, project ids or numbers. The API refuses one that " +
					"is not valid for the database's cloud, naming it. Changed in place.",
				Validators: []validator.Set{
					setvalidator.SizeAtMost(50),
					setvalidator.ValueStringsAre(
						stringvalidator.LengthBetween(1, 2048),
						stringvalidator.RegexMatches(privateLinkPrincipal,
							"must be an AWS principal ARN, an Azure subscription id, or a Google Cloud project id or number"),
					),
				},
			},
			"public_access_blocked": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Close the public endpoint, so the database is reachable only through private " +
					"connectivity. Applied once the endpoint service exists. Changed in place.",
			},
			"wait_for_ready": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Wait for the endpoint service to be built (three to four minutes, at most 20) " +
					"before the apply completes. On by default, because `service_name` is unknown until then. " +
					"With `false`, `public_access_blocked` is still applied only after the service exists, so " +
					"the apply waits anyway when it is set.",
			},
			"service_name": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "What to create your endpoint against: the endpoint service name on AWS, the " +
					"Private Link Service alias on Azure, the service attachment on Google Cloud. Null until " +
					"it is built.",
			},
			"region": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The region the endpoint service is in; your endpoint must be in the same one.",
			},
			"ports": schema.ListAttribute{
				ElementType: privateLinkPortType,
				Computed:    true,
				MarkdownDescription: "The ports the service listens on: `port`, its `role` (`primary`, `replicas` " +
					"or `replica`), whether it is `tls`, and a `label`. A replica's port is listed only while " +
					"that replica exists.",
			},
		},
	}
}

func (r *privateLinkResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
	if client, ok := clientFrom(req.ProviderData, &res.Diagnostics); ok {
		r.client = client
	}
}

func (m *privateLinkModel) apply(instanceID string, p *PrivateLink) {
	m.ID = types.StringValue(instanceID)
	m.InstanceID = types.StringValue(instanceID)
	principals := make([]attr.Value, 0, len(p.AllowedPrincipals))
	for _, s := range p.AllowedPrincipals {
		principals = append(principals, types.StringValue(s))
	}
	m.AllowedPrincipals = types.SetValueMust(types.StringType, principals)
	m.PublicAccessBlocked = types.BoolValue(p.PublicAccessBlocked)
	m.ServiceName = stringOrNull(p.ServiceName)
	if p.ServiceName != nil && *p.ServiceName == "" {
		m.ServiceName = types.StringNull()
	}
	m.Region = types.StringValue(p.Region)
	ports := make([]attr.Value, 0, len(p.Ports))
	for _, port := range p.Ports {
		ports = append(ports, types.ObjectValueMust(privateLinkPortType.AttrTypes, map[string]attr.Value{
			"port": types.Int64Value(port.Port), "role": types.StringValue(port.Role),
			"tls": types.BoolValue(port.TLS), "label": types.StringValue(port.Label),
		}))
	}
	m.Ports = types.ListValueMust(privateLinkPortType, ports)
}

func principalsOf(ctx context.Context, s types.Set, diags *diag.Diagnostics) []string {
	out := []string{}
	if s.IsNull() || s.IsUnknown() {
		return out
	}
	diags.Append(s.ElementsAs(ctx, &out, false)...)
	return out
}

// waitForService polls until the endpoint service has a name. The job that
// builds it reports nowhere this API key can read, so a build that fails shows
// here as one that never finishes, and the bound is what says so.
func (r *privateLinkResource) waitForService(ctx context.Context, id string) (*PrivateLink, error) {
	deadline := time.Now().Add(r.readyTimeout)
	for {
		p, err := r.client.GetPrivateLink(ctx, id)
		if err != nil {
			return nil, err
		}
		if p.ServiceName != nil && *p.ServiceName != "" {
			return p, nil
		}
		if time.Now().After(deadline) {
			return p, errors.New("the endpoint service was still being built after " + r.readyTimeout.String() +
				". It is usually ready in three to four minutes; check the database's notifications in the " +
				"console, and apply again to keep waiting")
		}
		select {
		case <-ctx.Done():
			return p, ctx.Err()
		case <-time.After(r.pollInterval):
		}
	}
}

func (r *privateLinkResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan privateLinkModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := plan.InstanceID.ValueString()
	principals := principalsOf(ctx, plan.AllowedPrincipals, &res.Diagnostics)
	if res.Diagnostics.HasError() {
		return
	}
	on := true
	if err := r.client.SetPrivateLink(ctx, id, &on, principals); err != nil {
		res.Diagnostics.AddError("Could not turn private connectivity on", explain(err))
		return
	}
	// From here the add-on is on and billed, so whatever happens next, it is
	// written to state: one Terraform forgot would be paid for and never
	// turned off.
	r.finish(ctx, &plan, id, true, &res.Diagnostics)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

// finish waits for the service when it has to, applies the public-access
// block when one is wanted, and reads the result into m. Errors are added to
// diags, but m always holds what the API last said.
func (r *privateLinkResource) finish(ctx context.Context, m *privateLinkModel, id string, changedBlock bool, diags *diag.Diagnostics) {
	wantBlocked := m.PublicAccessBlocked.ValueBool()
	var p *PrivateLink
	var err error
	if m.WaitForReady.ValueBool() || (wantBlocked && changedBlock) {
		p, err = r.waitForService(ctx, id)
	} else {
		p, err = r.client.GetPrivateLink(ctx, id)
	}
	if p != nil {
		m.apply(id, p)
	}
	if err != nil {
		if p == nil {
			m.ID = types.StringValue(id)
			nullUnknownPrivateLink(m)
		}
		diags.AddError("Private connectivity is on, but the endpoint service is not ready", explain(err))
		return
	}
	if changedBlock && p.PublicAccessBlocked != wantBlocked {
		if _, err := r.client.SetPublicAccessBlocked(ctx, id, wantBlocked); err != nil {
			diags.AddError("Could not change public access", explainNetwork(err))
			return
		}
		if p, err = r.client.GetPrivateLink(ctx, id); err != nil {
			diags.AddError("Could not read private connectivity back", explain(err))
			return
		}
		m.apply(id, p)
	}
}

func nullUnknownPrivateLink(m *privateLinkModel) {
	if m.ServiceName.IsUnknown() {
		m.ServiceName = types.StringNull()
	}
	if m.Region.IsUnknown() {
		m.Region = types.StringNull()
	}
	if m.Ports.IsUnknown() {
		m.Ports = types.ListNull(privateLinkPortType)
	}
}

func (r *privateLinkResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state privateLinkModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.InstanceID.ValueString()
	if id == "" {
		res.State.RemoveResource(ctx)
		return
	}
	p, err := r.client.GetPrivateLink(ctx, id)
	if err != nil {
		if isNotFound(err) {
			res.State.RemoveResource(ctx) // the database is gone
			return
		}
		res.Diagnostics.AddError("Could not read private connectivity", explain(err))
		return
	}
	if !p.Enabled {
		// Turned off in the console. The next plan turns it on again.
		res.State.RemoveResource(ctx)
		return
	}
	state.apply(id, p)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *privateLinkResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan, state privateLinkModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.InstanceID.ValueString()
	blockChanged := !plan.PublicAccessBlocked.Equal(state.PublicAccessBlocked)

	// Opening public access goes first: a principal change cannot strand a
	// database that is reachable publicly. Closing it goes last, in finish,
	// once the service is known to exist.
	if blockChanged && !plan.PublicAccessBlocked.ValueBool() {
		if _, err := r.client.SetPublicAccessBlocked(ctx, id, false); err != nil {
			res.Diagnostics.AddError("Could not allow public access", explainNetwork(err))
			return
		}
		blockChanged = false
	}
	if !plan.AllowedPrincipals.Equal(state.AllowedPrincipals) {
		principals := principalsOf(ctx, plan.AllowedPrincipals, &res.Diagnostics)
		if res.Diagnostics.HasError() {
			return
		}
		if err := r.client.SetPrivateLink(ctx, id, nil, principals); err != nil {
			res.Diagnostics.AddError("Could not change who may connect", explain(err))
			return
		}
	}
	r.finish(ctx, &plan, id, blockChanged, &res.Diagnostics)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

// Delete opens public access when it was blocked, then turns private
// connectivity off; see the resource description for why in that order.
func (r *privateLinkResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state privateLinkModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.InstanceID.ValueString()
	current, err := r.client.GetPrivateLink(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return
		}
		res.Diagnostics.AddError("Could not read private connectivity", explain(err))
		return
	}
	if current.PublicAccessBlocked {
		if _, err := r.client.SetPublicAccessBlocked(ctx, id, false); err != nil && !isNotFound(err) {
			res.Diagnostics.AddError("Could not allow public access before turning private connectivity off",
				explainNetwork(err))
			return
		}
	}
	off := false
	if err := r.client.SetPrivateLink(ctx, id, &off, nil); err != nil && !isNotFound(err) {
		res.Diagnostics.AddError("Could not turn private connectivity off", explain(err))
	}
}

func (r *privateLinkResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("instance_id"), req, res)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("wait_for_ready"), true)...)
}
