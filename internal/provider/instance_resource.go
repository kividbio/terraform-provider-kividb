package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var (
	_ resource.Resource                = (*instanceResource)(nil)
	_ resource.ResourceWithConfigure   = (*instanceResource)(nil)
	_ resource.ResourceWithImportState = (*instanceResource)(nil)
)

func NewInstanceResource() resource.Resource { return &instanceResource{} }

type instanceResource struct {
	client *Client
}

type instanceModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Tier           types.String `tfsdk:"tier"`
	Cloud          types.String `tfsdk:"cloud"`
	Region         types.String `tfsdk:"region"`
	DataSizeGB     types.Int64  `tfsdk:"data_size_gb"`
	ThroughputMode types.String `tfsdk:"throughput_mode"`
	ReplicaCount   types.Int64  `tfsdk:"replica_count"`
	KividbVersion  types.String `tfsdk:"kividb_version"`
	AofEnabled     types.Bool   `tfsdk:"aof_enabled"`
	LuaEnabled     types.Bool   `tfsdk:"lua_enabled"`
	TLSEnabled     types.Bool   `tfsdk:"tls_enabled"`
	TLSOnly        types.Bool   `tfsdk:"tls_only"`

	Status         types.String `tfsdk:"status"`
	Endpoint       types.String `tfsdk:"endpoint"`
	PublicEndpoint types.String `tfsdk:"public_endpoint"`
	OrgID          types.String `tfsdk:"org_id"`

	WaitForReady types.Bool `tfsdk:"wait_for_ready"`
}

func (r *instanceResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_instance"
}

func (r *instanceResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: `A KiviDB Cloud database.

### What can change without rebuilding

KiviDB changes a running database through a small set of separate operations,
and this resource mirrors them exactly. An attribute is updated in place only
where the API has an operation for it:

| Attribute | How it changes |
|---|---|
| ` + "`name`" + ` | renamed in place |
| ` + "`data_size_gb`, `throughput_mode`" + ` | resized in place |
| ` + "`replica_count`" + ` | scaled in place, 1-3 |
| ` + "`tier`" + ` | upgraded in place |
| ` + "`kividb_version`, `lua_enabled`, `tls_enabled`" + ` | applied by a rolling restart |
| ` + "`cloud`, `region`, `aof_enabled`, `tls_only`" + ` | **replaces the database** |

The last row is the important one. There is no operation that moves a database
between clouds or regions, or that turns append-only persistence on after the
fact, so Terraform will plan a destroy and create for those. That is shown in
the plan rather than discovered afterwards -- and because it destroys data, it
is worth reading a plan that mentions it.`,

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The instance's id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},

			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Hostname-safe name, unique across KiviDB. " +
					"Generated if omitted; changing it renames the database in place.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},

			"tier": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`essentials`, `pro` or `scale`. Upgrading is done in place.",
				Validators: []validator.String{
					stringvalidator.OneOf("essentials", "pro", "scale"),
				},
			},

			// Nothing in the API moves a database between clouds or regions, so
			// saying so in the schema is the difference between a plan that
			// shows a rebuild and an apply that silently does nothing.
			"cloud": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`aws` or `azure`. **Changing this replaces the database.**",
				Validators:          []validator.String{stringvalidator.OneOf("aws", "azure")},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"region": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Provider region, e.g. `us-east-1`. **Changing this replaces the database.**",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},

			"data_size_gb": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Logical data size in GB. Resized in place.",
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
			},
			"throughput_mode": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("standard"),
				MarkdownDescription: "`standard` or `high`. Changed in place.",
				Validators:          []validator.String{stringvalidator.OneOf("standard", "high")},
			},

			"replica_count": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(0),
				MarkdownDescription: "Read replicas. `0` at creation for a single node; " +
					"scaling afterwards accepts 1-3. Going back to `0` is not a scaling " +
					"operation and is refused -- see the error for what to do instead.",
				Validators: []validator.Int64{int64validator.Between(0, 3)},
			},

			"kividb_version": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Engine version, e.g. `1.0.4`. Applied by a rolling restart.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"lua_enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Lua scripting. Applied by a rolling restart.",
			},
			"tls_enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Serve TLS alongside plaintext. Applied by a rolling restart.",
			},

			"aof_enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Append-only persistence. **Changing this replaces the database**: " +
					"it is chosen when the volume is laid out and there is no operation to change it after.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"tls_only": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Refuse plaintext connections. **Changing this replaces the database.**",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},

			"wait_for_ready": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Wait for the database to finish provisioning before the apply " +
					"completes. On by default, because anything that depends on `endpoint` needs one " +
					"that answers. Set to `false` to return as soon as the work is accepted.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},

			"status":          schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle status."},
			"endpoint":        schema.StringAttribute{Computed: true, MarkdownDescription: "Private endpoint."},
			"public_endpoint": schema.StringAttribute{Computed: true, MarkdownDescription: "Public endpoint, when one is exposed."},
			"org_id":          schema.StringAttribute{Computed: true, MarkdownDescription: "Owning organization."},
		},
	}
}

func (r *instanceResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
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

func (r *instanceResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan instanceModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}

	create := CreateInstanceRequest{
		Tier:           plan.Tier.ValueString(),
		Cloud:          plan.Cloud.ValueString(),
		Region:         plan.Region.ValueString(),
		DataSizeGB:     plan.DataSizeGB.ValueInt64(),
		ThroughputMode: plan.ThroughputMode.ValueString(),
		ReplicaCount:   plan.ReplicaCount.ValueInt64(),
	}
	if !plan.Name.IsNull() && !plan.Name.IsUnknown() {
		v := plan.Name.ValueString()
		create.Name = &v
	}
	if !plan.KividbVersion.IsNull() && !plan.KividbVersion.IsUnknown() {
		v := plan.KividbVersion.ValueString()
		create.KividbVersion = &v
	}
	create.AofEnabled = boolPtr(plan.AofEnabled)
	create.LuaEnabled = boolPtr(plan.LuaEnabled)
	create.TLSEnabled = boolPtr(plan.TLSEnabled)
	create.TLSOnly = boolPtr(plan.TLSOnly)

	// A key derived from the plan, not a fresh random one: Terraform retries a
	// failed apply with the same plan, and the point is that the retry is
	// recognised as the same request rather than creating a second database.
	idemKey := idempotencyKeyFor(plan)

	id, err := r.client.CreateInstance(ctx, create, idemKey)
	if err != nil {
		res.Diagnostics.AddError("Could not create the database", explain(err))
		return
	}
	tflog.Info(ctx, "kividb instance created", map[string]any{"id": id})

	inst, err := r.settle(ctx, id, plan.WaitForReady.ValueBool())
	if err != nil {
		// The database exists, so it has to be written to state before the error
		// is returned -- otherwise a timeout leaves an orphan Terraform has
		// forgotten and the customer is still paying for.
		//
		// Written from what was actually read, not from the plan. The plan still
		// holds unknowns for every computed attribute, and Terraform cannot
		// persist an unknown: it stores an empty string instead, which is how
		// the next refresh ended up asking for the instance with no id.
		if inst != nil {
			applyInstance(&plan, inst)
		} else {
			plan.ID = types.StringValue(id)
		}
		res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
		res.Diagnostics.AddError("The database was created but did not become ready", explain(err))
		return
	}

	applyInstance(&plan, inst)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *instanceResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state instanceModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}

	id := state.ID.ValueString()
	if id == "" {
		// State written by an older, broken apply. There is nothing to refresh
		// and asking the API for an empty id is a 500, so drop it and let the
		// next plan create the resource properly.
		res.State.RemoveResource(ctx)
		return
	}

	inst, err := r.client.GetInstance(ctx, id)
	if err != nil {
		if apiErr, ok := err.(*APIError); ok && apiErr.IsNotFound() {
			// Deleted elsewhere. Removing it from state lets the next plan
			// recreate it, which is what Terraform is for.
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the database", explain(err))
		return
	}

	applyInstance(&state, inst)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// Update decomposes the diff into the operations KiviDB actually has.
//
// This is the part worth reading. The API deliberately has no "update the
// instance" endpoint -- it has a rename, a resize, a replica scale, a tier
// upgrade and a binary upgrade, each with its own refusals. Collapsing them into
// one PATCH here would mean reimplementing those refusals in the provider and
// getting them subtly wrong; instead each changed attribute is routed to the
// operation that owns it, and anything with no operation was already marked
// RequiresReplace in the schema, so it never reaches this function.
func (r *instanceResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan, state instanceModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	if !plan.Name.Equal(state.Name) && !plan.Name.IsUnknown() {
		if err := r.client.Rename(ctx, id, plan.Name.ValueString()); err != nil {
			res.Diagnostics.AddError("Could not rename the database", explain(err))
			return
		}
	}

	// One call for both, because the server takes them together and applying
	// them separately would restart the database twice.
	if !plan.DataSizeGB.Equal(state.DataSizeGB) || !plan.ThroughputMode.Equal(state.ThroughputMode) {
		var rr ResizeRequest
		if !plan.DataSizeGB.Equal(state.DataSizeGB) {
			v := plan.DataSizeGB.ValueInt64()
			rr.DataSizeGB = &v
		}
		if !plan.ThroughputMode.Equal(state.ThroughputMode) {
			v := plan.ThroughputMode.ValueString()
			rr.ThroughputMode = &v
		}
		if err := r.client.Resize(ctx, id, rr); err != nil {
			res.Diagnostics.AddError("Could not resize the database", explain(err))
			return
		}
	}

	// Set when the tier change already carried the replica count, so the scale
	// below does not repeat it. Upgrading queues work that takes minutes, and a
	// second call made while it is still running is refused -- the database is
	// not Pro yet, which is exactly what the upgrade is busy fixing.
	replicasSetByUpgrade := false

	if !plan.Tier.Equal(state.Tier) {
		// Pro is the only tier this endpoint reaches, and it needs the replica
		// count to bring up with it. Anything else is refused here rather than
		// sent, so the error names the tier instead of arriving as a 4xx about a
		// field the practitioner did not write.
		if plan.Tier.ValueString() != "pro" {
			res.Diagnostics.AddError(
				"Cannot change to that tier",
				"An existing database can only be upgraded to `pro`. Moving to `"+
					plan.Tier.ValueString()+"` is not something the API can do in place.",
			)
			return
		}
		// Default to one replica when the configuration does not say: Pro is
		// replicated by definition, and zero is not a shape it has.
		replicas := int64(1)
		if !plan.ReplicaCount.IsNull() && !plan.ReplicaCount.IsUnknown() && plan.ReplicaCount.ValueInt64() > 0 {
			replicas = plan.ReplicaCount.ValueInt64()
		}
		if err := r.client.UpgradeToPro(ctx, id, replicas); err != nil {
			res.Diagnostics.AddError("Could not change the tier", explain(err))
			return
		}
		replicasSetByUpgrade = true
	}

	if !replicasSetByUpgrade && !plan.ReplicaCount.Equal(state.ReplicaCount) {
		want := plan.ReplicaCount.ValueInt64()
		if want == 0 {
			// Said here rather than sent as a 400, because the useful answer is
			// what to do instead, and the provider is the only place that knows
			// the customer wrote `replica_count = 0` in a file.
			res.Diagnostics.AddError(
				"Replicas cannot be scaled to zero",
				"KiviDB scales replicas between 1 and 3. Removing replication entirely changes the "+
					"shape of the database rather than its size, so it is not an in-place operation. "+
					"Create a new database without replicas and migrate, or keep at least one replica.",
			)
			return
		}
		if err := r.client.ScaleReplicas(ctx, id, want); err != nil {
			res.Diagnostics.AddError("Could not scale replicas", explain(err))
			return
		}
	}

	// Version, Lua and TLS travel together: one restart applies all three, so
	// sending them separately would restart the database up to three times.
	if !plan.KividbVersion.Equal(state.KividbVersion) ||
		!plan.LuaEnabled.Equal(state.LuaEnabled) ||
		!plan.TLSEnabled.Equal(state.TLSEnabled) {

		version := plan.KividbVersion.ValueString()
		if plan.KividbVersion.IsUnknown() || version == "" {
			// Only the flags changed. The endpoint requires a version, so the
			// one already running is what keeps this from being a downgrade.
			version = state.KividbVersion.ValueString()
		}
		if version == "" {
			res.Diagnostics.AddError(
				"Cannot change these settings without a version",
				"Lua and TLS are applied by the same restart that installs the engine binary, so "+
					"KiviDB needs to know which version to run. Set `kividb_version` explicitly.",
			)
			return
		}
		br := BinaryRequest{KividbVersion: version}
		if !plan.LuaEnabled.Equal(state.LuaEnabled) {
			br.LuaEnabled = boolPtr(plan.LuaEnabled)
		}
		if !plan.TLSEnabled.Equal(state.TLSEnabled) {
			br.TLSEnabled = boolPtr(plan.TLSEnabled)
		}
		if err := r.client.UpgradeBinary(ctx, id, br); err != nil {
			res.Diagnostics.AddError("Could not apply the engine change", explain(err))
			return
		}
	}

	inst, err := r.settle(ctx, id, plan.WaitForReady.ValueBool())
	if err != nil {
		res.Diagnostics.AddError("The change was accepted but the database did not settle", explain(err))
		return
	}
	applyInstance(&plan, inst)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *instanceResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state instanceModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteInstance(ctx, state.ID.ValueString()); err != nil {
		if apiErr, ok := err.(*APIError); ok && apiErr.IsNotFound() {
			return // Already gone; nothing to report.
		}
		res.Diagnostics.AddError("Could not delete the database", explain(err))
	}
}

func (r *instanceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	inst, err := r.client.GetInstance(ctx, req.ID)
	if err != nil {
		res.Diagnostics.AddError("Could not import that database", explain(err))
		return
	}
	var state instanceModel
	state.ID = types.StringValue(inst.ID)
	// Defaulted rather than read, because the API has no opinion about it: it
	// describes how this provider behaves, not how the database is configured.
	state.WaitForReady = types.BoolValue(true)
	applyInstance(&state, inst)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// settle waits for provisioning to finish, when asked to.
//
// Polling rather than holding a request open: creating a database queues work
// that takes minutes, and an HTTP request that waited that long would be killed
// by every load balancer between here and the control plane.
// Returns the last instance it saw even when it returns an error, so a caller
// can still write complete state for a database that exists but is not ready.
// Returning only the error is what left a half-created database in state with
// every computed attribute empty -- and the next plan then read
// `GET /instances/` with no id at all.
func (r *instanceResource) settle(ctx context.Context, id string, wait bool) (*Instance, error) {
	inst, err := r.client.GetInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	if !wait {
		return inst, nil
	}

	deadline := time.Now().Add(30 * time.Minute)
	for {
		switch inst.Status {
		case "running", "stopped":
			return inst, nil
		case "failed":
			return inst, fmt.Errorf("the database reported status %q", inst.Status)
		}
		if time.Now().After(deadline) {
			return inst, fmt.Errorf("still %q after 30 minutes", inst.Status)
		}
		select {
		case <-ctx.Done():
			return inst, ctx.Err()
		case <-time.After(10 * time.Second):
		}
		next, err := r.client.GetInstance(ctx, id)
		if err != nil {
			return inst, err
		}
		inst = next
	}
}

func applyInstance(m *instanceModel, inst *Instance) {
	m.ID = types.StringValue(inst.ID)
	m.Tier = types.StringValue(inst.Tier)
	m.Cloud = types.StringValue(inst.Cloud)
	m.Region = types.StringValue(inst.Region)
	m.DataSizeGB = types.Int64Value(inst.DataSizeGB)
	m.ThroughputMode = types.StringValue(inst.ThroughputMode)
	m.ReplicaCount = types.Int64Value(inst.ReplicaCount)
	m.AofEnabled = types.BoolValue(inst.AofEnabled)
	m.LuaEnabled = types.BoolValue(inst.LuaEnabled)
	m.TLSEnabled = types.BoolValue(inst.TLSEnabled)
	m.TLSOnly = types.BoolValue(inst.TLSOnly)
	m.Status = types.StringValue(inst.Status)

	m.Name = stringOrNull(inst.Name)
	m.KividbVersion = stringOrNull(inst.KividbVersion)
	m.Endpoint = stringOrNull(inst.Endpoint)
	m.PublicEndpoint = stringOrNull(inst.PublicEndpoint)
	m.OrgID = stringOrNull(inst.OrgID)
}

func stringOrNull(s *string) types.String {
	if s == nil {
		return types.StringNull()
	}
	return types.StringValue(*s)
}

func boolPtr(b types.Bool) *bool {
	if b.IsNull() || b.IsUnknown() {
		return nil
	}
	v := b.ValueBool()
	return &v
}

// explain turns an API refusal into something actionable.
//
// The server writes its refusals for whoever holds the key, so they are used as
// given; only the ones that mean something specific in a Terraform context get
// extra words about what to do in the configuration.
func explain(err error) string {
	apiErr, ok := err.(*APIError)
	if !ok {
		return err.Error()
	}
	switch apiErr.Code {
	case "forbidden":
		return apiErr.Message + "\n\nMint a key with the admin role under Dashboard → API keys."
	case "free_tier_requires_a_user":
		return apiErr.Message + "\n\nFree-tier databases cannot be managed by Terraform; " +
			"they are one per person and are created from the console."
	case "insufficient_credits":
		return apiErr.Message + "\n\nAdd credits under Dashboard → Billing, then apply again."
	default:
		return apiErr.Error()
	}
}

// idempotencyKeyFor derives a stable key from what is being created.
//
// Stable across a retry of the same plan, different for a genuinely different
// database. Using the name where there is one is what makes it stable: a random
// key would make every retry a new create, which is the failure this exists to
// prevent.
func idempotencyKeyFor(plan instanceModel) string {
	name := plan.Name.ValueString()
	if name == "" {
		name = "unnamed"
	}
	return fmt.Sprintf("tf:%s:%s:%s:%s:%d",
		name,
		plan.Cloud.ValueString(),
		plan.Region.ValueString(),
		plan.Tier.ValueString(),
		plan.DataSizeGB.ValueInt64(),
	)
}
