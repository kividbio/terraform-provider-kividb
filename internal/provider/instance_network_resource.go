package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*instanceNetworkResource)(nil)
	_ resource.ResourceWithConfigure      = (*instanceNetworkResource)(nil)
	_ resource.ResourceWithImportState    = (*instanceNetworkResource)(nil)
	_ resource.ResourceWithValidateConfig = (*instanceNetworkResource)(nil)
)

func NewInstanceNetworkResource() resource.Resource { return &instanceNetworkResource{} }

type instanceNetworkResource struct{ client *Client }

type instanceNetworkModel struct {
	ID         types.String `tfsdk:"id"`
	InstanceID types.String `tfsdk:"instance_id"`
	Allowlist  types.Set    `tfsdk:"allowlist"`
	Port       types.Int64  `tfsdk:"port"`
	Ports      types.List   `tfsdk:"ports"`
	Synced     types.Bool   `tfsdk:"synced"`
	SyncError  types.String `tfsdk:"sync_error"`
}

type allowlistEntry struct {
	CIDR  types.String `tfsdk:"cidr"`
	Label types.String `tfsdk:"label"`
}

var allowlistEntryType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"cidr":  types.StringType,
	"label": types.StringType,
}}

// An IPv4 CIDR written the way the API stores it: an explicit prefix and no
// leading zeros. The API accepts "10.0.0.1" and "010.0.0.1" too and stores
// both as "10.0.0.1/32", which would then differ from the configuration on
// every plan; asking for the stored form up front avoids that.
var canonicalCIDR = regexp.MustCompile(`^(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3}/(3[0-2]|[12]?[0-9])$`)

// The route's limit on rules (putNetworkSchema).
const maxAllowlistEntries = 20

func (r *instanceNetworkResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_instance_network"
}

func (r *instanceNetworkResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: `Which addresses may connect to a database over its public endpoint: the
allowlist, applied to the database's firewall (a security group, network
security group or firewall rule, depending on the cloud).

The allowlist is the whole list. Applying this resource replaces whatever the
database had -- including the address KiviDB added for whoever created it -- so
list every address that needs to connect. An empty allowlist lets no client in
over the public endpoint; the plan warns about it.

Two settings that share this endpoint are kept where they belong instead:
` + "`tls_only`" + ` on ` + "`kividb_instance`" + `, because it is tied to the database's TLS and
client-certificate settings, and blocking public access on
` + "`kividb_private_link`" + `, because it needs private connectivity to exist. This
resource never changes either.

**Destroying this resource leaves the allowlist as it is**, and only stops
Terraform managing it. Both alternatives would change who can connect as a
side effect of removing configuration: an empty list locks every client out,
and an open one exposes the database to the internet.`,
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The database's id: a database has one allowlist.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database. **Changing this sets the new database's allowlist and leaves the old one's as it is.**",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"allowlist": schema.SetNestedAttribute{
				Required: true,
				MarkdownDescription: fmt.Sprintf("The addresses allowed to connect, at most %d. Each is an IPv4 `cidr` "+
					"with its prefix, such as `203.0.113.7/32` for one address, and an optional `label`. "+
					"`0.0.0.0/0` allows any address; the password is then the only thing in the way.",
					maxAllowlistEntries),
				Validators: []validator.Set{setvalidator.SizeAtMost(maxAllowlistEntries)},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"cidr": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "An IPv4 address range, e.g. `203.0.113.0/24`. Written with its prefix and without leading zeros, as the API stores it.",
							Validators: []validator.String{stringvalidator.RegexMatches(canonicalCIDR,
								"must be an IPv4 CIDR with its prefix and no leading zeros, e.g. 203.0.113.7/32")},
						},
						"label": schema.StringAttribute{
							Optional:            true,
							MarkdownDescription: "What the address is, up to 64 characters, e.g. `office` or `ci runners`.",
							Validators:          []validator.String{stringvalidator.LengthAtMost(64)},
						},
					},
				},
			},
			"port": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "The port clients connect to: 6379, or 6380 when the database is TLS-only. 0 " +
					"while public access is blocked.",
			},
			"ports": schema.ListAttribute{
				ElementType:         types.Int64Type,
				Computed:            true,
				MarkdownDescription: "Every port the allowlist is open on: both 6379 and 6380 when TLS is on but not required.",
			},
			"synced": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the database's firewall matches the allowlist. False for a moment while a " +
					"database is still being created; the allowlist is applied once it is.",
			},
			"sync_error": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Why the firewall does not match yet, when `synced` is false.",
			},
		},
	}
}

func (r *instanceNetworkResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, res *resource.ValidateConfigResponse) {
	var list types.Set
	res.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("allowlist"), &list)...)
	if res.Diagnostics.HasError() || list.IsNull() || list.IsUnknown() {
		return
	}
	var entries []allowlistEntry
	res.Diagnostics.Append(list.ElementsAs(ctx, &entries, false)...)
	if res.Diagnostics.HasError() {
		return
	}
	if len(entries) == 0 {
		res.Diagnostics.AddAttributeWarning(path.Root("allowlist"), "No address may connect",
			"An empty allowlist closes the public endpoint to every client. Connections over private "+
				"connectivity (kividb_private_link) are not affected.")
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.CIDR.IsUnknown() {
			continue
		}
		c := e.CIDR.ValueString()
		if seen[c] {
			// The API keeps the first and drops the rest, so the read-back
			// would never match.
			res.Diagnostics.AddAttributeError(path.Root("allowlist"), "The same address twice",
				fmt.Sprintf("%s is in the allowlist more than once. List each address once, with one label.", c))
		}
		seen[c] = true
		if c == "0.0.0.0/0" {
			res.Diagnostics.AddAttributeWarning(path.Root("allowlist"), "Any address may connect",
				"0.0.0.0/0 opens the public endpoint to the whole internet; the database's password is the "+
					"only thing in the way.")
		}
	}
}

func (r *instanceNetworkResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
	if client, ok := clientFrom(req.ProviderData, &res.Diagnostics); ok {
		r.client = client
	}
}

// apply copies the API's answer over the model. It returns a warning when the
// database holds deny rules: the API stores them but only allow rules reach
// the firewall, so they block nothing, and the next change here drops them.
func (m *instanceNetworkModel) apply(instanceID string, n *Network) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(instanceID)
	m.InstanceID = types.StringValue(instanceID)

	elems := []attr.Value{}
	var denied []string
	for _, rule := range n.Rules {
		if rule.Action != "" && rule.Action != "allow" {
			denied = append(denied, rule.CIDR)
			continue
		}
		label := types.StringNull()
		if rule.Label != nil {
			label = types.StringValue(*rule.Label)
		}
		elems = append(elems, types.ObjectValueMust(allowlistEntryType.AttrTypes, map[string]attr.Value{
			"cidr": types.StringValue(rule.CIDR), "label": label,
		}))
	}
	m.Allowlist = types.SetValueMust(allowlistEntryType, elems)
	if len(denied) > 0 {
		diags.AddWarning("The database has deny rules, which are not enforced",
			fmt.Sprintf("%s: these were saved as deny rules, but only the allowlist reaches the firewall, so they "+
				"block nothing. Applying this resource removes them.", strings.Join(denied, ", ")))
	}

	m.Port = types.Int64Value(n.Port)
	ports := make([]attr.Value, 0, len(n.Ports))
	for _, p := range n.Ports {
		ports = append(ports, types.Int64Value(p))
	}
	m.Ports = types.ListValueMust(types.Int64Type, ports)
	m.Synced = types.BoolValue(n.Reconcile.Synced)
	m.SyncError = stringOrNull(n.Reconcile.Error)
	return diags
}

func (r *instanceNetworkResource) write(ctx context.Context, m *instanceNetworkModel) diag.Diagnostics {
	var diags diag.Diagnostics
	var entries []allowlistEntry
	diags.Append(m.Allowlist.ElementsAs(ctx, &entries, false)...)
	if diags.HasError() {
		return diags
	}
	rules := make([]NetworkRuleInput, 0, len(entries))
	for _, e := range entries {
		rule := NetworkRuleInput{CIDR: e.CIDR.ValueString(), Action: "allow"}
		if !e.Label.IsNull() && !e.Label.IsUnknown() {
			v := e.Label.ValueString()
			rule.Label = &v
		}
		rules = append(rules, rule)
	}
	id := m.InstanceID.ValueString()
	n, err := r.client.ReplaceAllowlist(ctx, id, rules)
	if err != nil {
		diags.AddError("Could not set the allowlist", explainNetwork(err))
		return diags
	}
	m.apply(id, n)
	if !n.Reconcile.Synced && n.Reconcile.Error != nil {
		diags.AddWarning("The allowlist is saved but not applied yet", *n.Reconcile.Error)
	}
	return diags
}

func (r *instanceNetworkResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan instanceNetworkModel
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

func (r *instanceNetworkResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state instanceNetworkModel
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
	n, err := r.client.GetNetwork(ctx, id)
	if err != nil {
		if isNotFound(err) {
			res.State.RemoveResource(ctx) // the database is gone
			return
		}
		res.Diagnostics.AddError("Could not read the allowlist", explainNetwork(err))
		return
	}
	// The configured spelling of the database's id is kept.
	keep := state.InstanceID
	res.Diagnostics.Append(state.apply(id, n)...)
	if !keep.IsNull() && strings.EqualFold(keep.ValueString(), id) {
		state.InstanceID = keep
		state.ID = keep
	}
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *instanceNetworkResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan instanceNetworkModel
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

// Delete changes nothing on the database; see the resource description.
func (r *instanceNetworkResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *instanceNetworkResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("instance_id"), req, res)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// explainNetwork adds what a firewall-sync failure means for the apply: the
// API changed nothing it would have to undo, so applying again is safe.
func explainNetwork(err error) string {
	apiErr, ok := err.(*APIError)
	if ok && apiErr.Code == "sg_sync_failed" {
		return apiErr.Error() + "\n\nThe database's firewall could not be updated, so the allowlist was not " +
			"saved. Apply again; if it keeps failing, the cloud provider may be having trouble."
	}
	return explain(err)
}
