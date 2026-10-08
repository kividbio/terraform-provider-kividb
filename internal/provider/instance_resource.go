package provider

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
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

	CloudAccountID          types.String `tfsdk:"cloud_account_id"`
	PrivateEndpoint         types.String `tfsdk:"private_endpoint"`
	PrivateReadonlyEndpoint types.String `tfsdk:"private_readonly_endpoint"`
	PrivateReplicaEndpoints types.List   `tfsdk:"private_replica_endpoints"`

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
| ` + "`cloud`, `region`, `cloud_account_id`, `aof_enabled`, `tls_only`" + ` | **replaces the database** |

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
			//
			// The validator turns a typo into a plan-time error. It is not the
			// gate on what is sellable: that is decided in one place on the
			// server, which refuses an unavailable cloud or region at apply with
			// a message saying why. A narrower list here would be a second gate
			// that only a provider release can change.
			"cloud": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`aws`, `azure` or `gcp` (Google Cloud). **Changing this replaces the database.**",
				Validators:          []validator.String{stringvalidator.OneOf("aws", "azure", "gcp")},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"region": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "A region of the chosen cloud, e.g. `us-east-1` (AWS), `swedencentral` (Azure) or `us-central1` (Google Cloud). **Changing this replaces the database.**",
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
			// UseStateForUnknown because a database does not move between
			// organizations. Without it every plan that changes anything shows
			// `org_id = "..." -> (known after apply)`, which is noise in the
			// one place a practitioner is supposed to be reading carefully.
			"org_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Owning organization.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},

			// Bring your own cloud. Chosen at creation and never moved: there
			// is no operation that carries a database's volume from one
			// account to another.
			"cloud_account_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Run the database in your own cloud account: the id of an account " +
					"connected in the KiviDB console (see the `kividb_cloud_account` data source). " +
					"Omit it to run the database in KiviDB's cloud. Available on AWS, Azure and Google Cloud " +
					"(set `cloud` to the account's cloud). The account must be " +
					"verified and have the database's region enabled. **Changing this replaces the database.**",
				Validators: []validator.String{
					stringvalidator.RegexMatches(uuidPattern, "must be a cloud account id (a UUID)"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			// The private endpoints are derived from the name, the tier and
			// the replica count, so they are carried over from state exactly
			// when none of those change -- see useStateUnlessChanged.
			"private_endpoint": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Hostname that resolves to the primary inside your own cloud " +
					"account's network. Set only when `cloud_account_id` is. No port: use the same " +
					"port as `endpoint`.",
				PlanModifiers: []planmodifier.String{useStateUnlessChanged("name", "cloud_account_id")},
			},
			"private_readonly_endpoint": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Hostname that spreads reads across the replicas, inside your own " +
					"cloud account's network. Pro databases in your own cloud account only.",
				PlanModifiers: []planmodifier.String{useStateUnlessChanged("name", "cloud_account_id", "tier")},
			},
			"private_replica_endpoints": schema.ListAttribute{
				ElementType: types.StringType,
				Computed:    true,
				MarkdownDescription: "One hostname per replica, inside your own cloud account's network. " +
					"Pro databases in your own cloud account only; empty otherwise.",
				PlanModifiers: []planmodifier.List{
					useStateUnlessChanged("name", "cloud_account_id", "tier", "replica_count"),
				},
			},
		},
	}
}

// uuidPattern accepts a UUID in either case. The API answers in lower case;
// applyInstance keeps the configured spelling so an upper-case id in a file
// does not plan a replacement on every refresh.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

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
	if !plan.CloudAccountID.IsNull() && !plan.CloudAccountID.IsUnknown() {
		v := strings.ToLower(plan.CloudAccountID.ValueString())
		create.CloudAccountID = &v
	}

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
			nullUnknownComputed(&plan)
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

	/*
	 * Each step below waits for the one before it to finish.
	 *
	 * KiviDB has no "update the instance" endpoint -- it has a rename, a
	 * resize, a replica scale, a tier upgrade and a binary upgrade, and most of
	 * them refuse an instance that is not settled. Issued back to back, the
	 * first one moves the database to `scaling` or `modifying` and the next is
	 * refused by the server.
	 *
	 * Found by changing replica_count and tls_enabled in one apply against
	 * production. The scale was accepted, the engine change that followed a
	 * moment later came back "Instance must be running or stopped to upgrade the
	 * engine", and the apply failed having applied half of what was planned --
	 * with the practitioner left to work out which half.
	 *
	 * `settled` is called before each step after the first that did something,
	 * so a single-change apply is exactly as fast as it was.
	 */
	didSomething := false
	settled := func() bool {
		if !didSomething {
			return true
		}
		if _, err := r.settle(ctx, id, true); err != nil {
			res.Diagnostics.AddError(
				"The database did not settle between changes",
				"An earlier part of this change was applied and the database did not come back to rest, "+
					"so the rest was not attempted: "+explain(err),
			)
			return false
		}
		return true
	}

	if !plan.Name.Equal(state.Name) && !plan.Name.IsUnknown() {
		if err := r.client.Rename(ctx, id, plan.Name.ValueString()); err != nil {
			res.Diagnostics.AddError("Could not rename the database", explain(err))
			return
		}
		didSomething = true
	}

	// One call for both, because the server takes them together and applying
	// them separately would restart the database twice.
	if !plan.DataSizeGB.Equal(state.DataSizeGB) || !plan.ThroughputMode.Equal(state.ThroughputMode) {
		if !settled() {
			return
		}
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
		didSomething = true
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
		if !settled() {
			return
		}
		if err := r.client.UpgradeToPro(ctx, id, replicas); err != nil {
			res.Diagnostics.AddError("Could not change the tier", explain(err))
			return
		}
		didSomething = true
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
		if !settled() {
			return
		}
		if err := r.client.ScaleReplicas(ctx, id, want); err != nil {
			res.Diagnostics.AddError("Could not scale replicas", explain(err))
			return
		}
		didSomething = true
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
		if !settled() {
			return
		}
		if err := r.client.UpgradeBinary(ctx, id, br); err != nil {
			res.Diagnostics.AddError("Could not apply the engine change", explain(err))
			return
		}
	}

	inst, err := r.settleUntil(ctx, id, plan.WaitForReady.ValueBool(), wantedBy(&plan, &state))
	if err != nil {
		// State is written from what was read, not from the plan, before the
		// error is returned. The database exists and has some shape; recording
		// the shape that was asked for would leave state describing something
		// that is not there, and the next plan would then see no drift and
		// never try again.
		applyInstance(&state, inst)
		res.Diagnostics.Append(res.State.Set(ctx, &state)...)
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
	return r.settleUntil(ctx, id, wait, nil)
}

// settleUntil waits for the database to come to rest, and for `want` to hold.
//
// Waiting on the status alone is not enough. A change is queued, the status goes
// to "modifying" and comes back to "running" -- and whether the change actually
// landed is a separate question that the status does not answer. When the work
// behind it fails, the database settles at its old shape and reports itself
// healthy, because it is: it simply is not what was asked for.
//
// That produced the worst available failure. The provider wrote back what it
// read, which was honest, and Terraform compared that against the plan:
//
//	Error: Provider produced inconsistent result after apply
//	.data_size_gb: was cty.NumberIntVal(2), but now cty.NumberIntVal(1)
//	This is a bug in the provider, which should be reported in the provider's
//	own issue tracker.
//
// It was not a bug in the provider. A resize had been accepted and then dropped
// on the way to the node, and the practitioner was sent to the wrong issue
// tracker for a control-plane problem they could do nothing about from here.
//
// So `want` is polled alongside the status, on the same deadline: a change that
// is merely slow is waited for, and one that never arrives is reported as what
// it is, naming the field, what was asked for, and what the database says.
func (r *instanceResource) settleUntil(
	ctx context.Context,
	id string,
	wait bool,
	want func(*Instance) error,
) (*Instance, error) {
	inst, err := r.client.GetInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	if !wait {
		return inst, nil
	}

	deadline := time.Now().Add(30 * time.Minute)
	for {
		atRest := false
		switch inst.Status {
		case "running", "stopped":
			atRest = true
		case "failed":
			return inst, fmt.Errorf("the database reported status %q", inst.Status)
		}

		// At rest and holding what was asked for: done. At rest but not holding
		// it: keep waiting, because the status settles before some changes land
		// and erroring on the first look would fail applies that were only slow.
		var unmet error
		if atRest {
			if want == nil {
				return inst, nil
			}
			if unmet = want(inst); unmet == nil {
				return inst, nil
			}
		}

		if time.Now().After(deadline) {
			if unmet != nil {
				return inst, fmt.Errorf(
					"the change was accepted but never took effect: %w. The database is healthy and "+
						"reports status %q, so the work behind the change did not reach it",
					unmet, inst.Status,
				)
			}
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

// wantedBy builds the condition an update has to reach before it is finished.
//
// Only attributes the plan actually changed are checked. Checking everything
// would turn any server-side normalisation the practitioner never asked about
// into a failed apply.
func wantedBy(plan, state *instanceModel) func(*Instance) error {
	type expectation struct {
		field string
		want  string
		got   func(*Instance) string
	}
	var expected []expectation

	if !plan.DataSizeGB.Equal(state.DataSizeGB) && !plan.DataSizeGB.IsUnknown() {
		expected = append(expected, expectation{
			"data_size_gb", strconv.FormatInt(plan.DataSizeGB.ValueInt64(), 10),
			func(i *Instance) string { return strconv.FormatInt(i.DataSizeGB, 10) },
		})
	}
	if !plan.ThroughputMode.Equal(state.ThroughputMode) && !plan.ThroughputMode.IsUnknown() {
		expected = append(expected, expectation{
			"throughput_mode", plan.ThroughputMode.ValueString(),
			func(i *Instance) string { return i.ThroughputMode },
		})
	}
	if !plan.Tier.Equal(state.Tier) && !plan.Tier.IsUnknown() {
		expected = append(expected, expectation{
			"tier", plan.Tier.ValueString(),
			func(i *Instance) string { return i.Tier },
		})
	}
	if !plan.ReplicaCount.Equal(state.ReplicaCount) && !plan.ReplicaCount.IsUnknown() {
		expected = append(expected, expectation{
			"replica_count", strconv.FormatInt(plan.ReplicaCount.ValueInt64(), 10),
			func(i *Instance) string { return strconv.FormatInt(i.ReplicaCount, 10) },
		})
	}
	if !plan.Name.Equal(state.Name) && !plan.Name.IsUnknown() && !plan.Name.IsNull() {
		expected = append(expected, expectation{
			"name", plan.Name.ValueString(),
			func(i *Instance) string {
				if i.Name == nil {
					return ""
				}
				return *i.Name
			},
		})
	}

	if len(expected) == 0 {
		return nil
	}
	return func(i *Instance) error {
		for _, e := range expected {
			if got := e.got(i); got != e.want {
				return fmt.Errorf("%s is still %s, not %s", e.field, got, e.want)
			}
		}
		return nil
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

	// Keep the configured spelling of the same id; see uuidPattern.
	if inst.CloudAccountID == nil || m.CloudAccountID.IsNull() || m.CloudAccountID.IsUnknown() ||
		!strings.EqualFold(m.CloudAccountID.ValueString(), *inst.CloudAccountID) {
		m.CloudAccountID = stringOrNull(inst.CloudAccountID)
	}
	m.PrivateEndpoint = stringOrNull(inst.PrivateEndpoint)
	m.PrivateReadonlyEndpoint = stringOrNull(inst.PrivateReadonlyEndpoint)
	m.PrivateReplicaEndpoints = stringList(inst.PrivateReplicaEndpoints)
}

// nullUnknownComputed replaces the plan's unknowns with nulls, for the one path
// that writes state without having read the database back. Terraform refuses
// unknown values in state after an apply; the next refresh fills them in.
func nullUnknownComputed(m *instanceModel) {
	for _, s := range []*types.String{
		&m.Name, &m.KividbVersion, &m.Status, &m.Endpoint, &m.PublicEndpoint, &m.OrgID,
		&m.PrivateEndpoint, &m.PrivateReadonlyEndpoint,
	} {
		if s.IsUnknown() {
			*s = types.StringNull()
		}
	}
	if m.PrivateReplicaEndpoints.IsUnknown() {
		m.PrivateReplicaEndpoints = types.ListNull(types.StringType)
	}
}

// stringList is never null: a database with no replica endpoints has an empty
// list, so the attribute reads the same after create, refresh and import and
// `length(...)` works without a null check.
func stringList(in []string) types.List {
	elems := make([]attr.Value, 0, len(in))
	for _, s := range in {
		elems = append(elems, types.StringValue(s))
	}
	return types.ListValueMust(types.StringType, elems)
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
	key := fmt.Sprintf("tf:%s:%s:%s:%s:%d",
		name,
		plan.Cloud.ValueString(),
		plan.Region.ValueString(),
		plan.Tier.ValueString(),
		plan.DataSizeGB.ValueInt64(),
	)
	// The same database in a different account is a different database. Only
	// appended when set, so keys for databases in KiviDB's cloud are unchanged.
	if v := plan.CloudAccountID.ValueString(); v != "" {
		key += ":" + strings.ToLower(v)
	}
	return key
}
