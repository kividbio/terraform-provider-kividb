package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*contextAgentKeyResource)(nil)
	_ resource.ResourceWithConfigure   = (*contextAgentKeyResource)(nil)
	_ resource.ResourceWithImportState = (*contextAgentKeyResource)(nil)
)

func NewContextAgentKeyResource() resource.Resource { return &contextAgentKeyResource{} }

type contextAgentKeyResource struct{ client *Client }

type contextAgentKeyModel struct {
	ID        types.String `tfsdk:"id"`
	Model     types.String `tfsdk:"model"`
	Name      types.String `tfsdk:"name"`
	Entities  types.List   `tfsdk:"entities"`
	Filters   types.Map    `tfsdk:"filters"`
	Key       types.String `tfsdk:"key"`
	KeyPrefix types.String `tfsdk:"key_prefix"`
	MCPURL    types.String `tfsdk:"mcp_url"`
	CreatedAt types.String `tfsdk:"created_at"`
}

var filtersType = types.MapType{ElemType: types.StringType}

func (r *contextAgentKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_context_agent_key"
}

func (r *contextAgentKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "An agent key for a `kividb_context_model`: what an AI agent sends to call the model's " +
			"tools over the Model Context Protocol. A key can be limited to some of the model's entities, and " +
			"can carry fixed filters every query through it adds (one tenant's records, say).\n\n" +
			"The key itself is shown once, when it is made, and is kept in state as the sensitive `key`. " +
			"Nothing about a key can be changed, so any change makes a new key and revokes this one. " +
			"Destroying the resource revokes the key: an agent using it is refused from then on. A key " +
			"revoked elsewhere, or deleted with its model, disappears from state on the next refresh.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The key's id (not the key). Import with `<model>/<id>`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"model": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The name of the context model the key is for. A model has at most 50 keys.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.RegexMatches(aiNamePattern, aiNameRule)},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "A name for the key, 1-100 characters, so you can tell keys apart in the console.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.LengthBetween(1, 100)},
			},
			"entities": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Only these entities' tools, by name, 1-20 of them. Leave it out for all of " +
					"the model's entities, including ones added later.",
				PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace()},
				Validators: []validator.List{
					listvalidator.SizeBetween(1, 20),
					listvalidator.ValueStringsAre(stringvalidator.RegexMatches(contextEntityNamePattern, contextEntityNameRule)),
				},
			},
			"filters": schema.MapAttribute{
				Optional:    true,
				ElementType: filtersType,
				MarkdownDescription: "Fixed filters per entity, which every query through this key adds and the " +
					"agent cannot remove: `{ customer = { tenant = \"acme\" } }` lets the key see only acme's " +
					"customers. Only `tag` and `numeric` fields (or the entity's `id_field`) can be filtered; a " +
					"numeric field's value is written as a string, e.g. `\"5\"`.",
				PlanModifiers: []planmodifier.Map{mapplanmodifier.RequiresReplace()},
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringvalidator.RegexMatches(contextEntityNamePattern, contextEntityNameRule)),
					mapvalidator.ValueMapsAre(
						mapvalidator.KeysAre(stringvalidator.RegexMatches(contextFieldNamePattern, contextFieldNameRule)),
						mapvalidator.ValueStringsAre(stringvalidator.LengthBetween(1, 200)),
					),
				},
			},
			"key": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The agent key, `kvctx_...`. The API shows it only when it is made, so it is " +
					"null for an imported key. Anyone with state can read it.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"key_prefix": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The key's first characters, as the console lists it.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"mcp_url": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Where an agent with this key connects over the Model Context Protocol (JSON-RPC over HTTP POST), sending `Authorization: Bearer <key>`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the key was made.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *contextAgentKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
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

// apply copies the listed key over the model. The key itself is never listed,
// so whatever the model holds for it is kept.
func (m *contextAgentKeyModel) apply(ctx context.Context, model string, k *ContextAgentKey, mcpURL string) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(k.ID)
	m.Model = types.StringValue(model)
	// The API trims the name; keep the configured spelling of the same text.
	if m.Name.IsNull() || m.Name.IsUnknown() || strings.TrimSpace(m.Name.ValueString()) != k.Name {
		m.Name = types.StringValue(k.Name)
	}
	m.KeyPrefix = types.StringValue(k.KeyPrefix)
	m.CreatedAt = types.StringValue(k.CreatedAt)
	m.MCPURL = types.StringValue(mcpURL)
	if m.Key.IsUnknown() {
		m.Key = types.StringNull()
	}

	if k.Entities == nil {
		m.Entities = types.ListNull(types.StringType)
	} else {
		var d diag.Diagnostics
		m.Entities, d = types.ListValueFrom(ctx, types.StringType, k.Entities)
		diags.Append(d...)
	}

	// No filters reads as {} from the API. That is the same as leaving the
	// attribute out, so it stays null unless an empty map was configured.
	if len(k.Filters) == 0 {
		if m.Filters.IsNull() || m.Filters.IsUnknown() || len(m.Filters.Elements()) != 0 {
			m.Filters = types.MapNull(filtersType)
		}
		return diags
	}
	filters := map[string]map[string]string{}
	for entity, fields := range k.Filters {
		filters[entity] = map[string]string{}
		for f, v := range fields {
			filters[entity][f] = filterString(v)
		}
	}
	var d diag.Diagnostics
	m.Filters, d = types.MapValueFrom(ctx, filtersType, filters)
	diags.Append(d...)
	return diags
}

func (r *contextAgentKeyResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan contextAgentKeyModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	body := ContextAgentKeyRequest{Name: plan.Name.ValueString()}
	if !plan.Entities.IsNull() && !plan.Entities.IsUnknown() {
		res.Diagnostics.Append(plan.Entities.ElementsAs(ctx, &body.Entities, false)...)
	}
	if !plan.Filters.IsNull() && !plan.Filters.IsUnknown() {
		res.Diagnostics.Append(plan.Filters.ElementsAs(ctx, &body.Filters, false)...)
	}
	if res.Diagnostics.HasError() {
		return
	}
	model := plan.Model.ValueString()
	key, k, err := r.client.CreateContextAgentKey(ctx, model, body)
	if err != nil {
		res.Diagnostics.AddError("Could not make the agent key", explainAI(err))
		return
	}
	plan.Key = types.StringValue(key)
	res.Diagnostics.Append(plan.apply(ctx, model, k, r.client.ContextMCPURL())...)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *contextAgentKeyResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state contextAgentKeyModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	// There is no read of one key: the model lists its keys that are not revoked.
	model := state.Model.ValueString()
	d, err := r.client.GetContextModel(ctx, model)
	if err != nil {
		if isGone(err) {
			// The model was deleted, and its keys with it.
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the agent key's model", explainAI(err))
		return
	}
	for i := range d.Keys {
		if d.Keys[i].ID == state.ID.ValueString() && !d.Keys[i].Revoked {
			res.Diagnostics.Append(state.apply(ctx, model, &d.Keys[i], r.client.ContextMCPURL())...)
			res.Diagnostics.Append(res.State.Set(ctx, &state)...)
			return
		}
	}
	// Revoked elsewhere: a revoked key never works again, so it is gone.
	res.State.RemoveResource(ctx)
}

// Update is never reached with a change, because every configurable attribute
// forces replacement; it only carries the plan into state.
func (r *contextAgentKeyResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan contextAgentKeyModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *contextAgentKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state contextAgentKeyModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if err := r.client.RevokeContextAgentKey(ctx, state.Model.ValueString(), state.ID.ValueString()); err != nil {
		if isGone(err) {
			// Already revoked, or the model is gone and its keys with it.
			return
		}
		res.Diagnostics.AddError("Could not revoke the agent key", explainAI(err))
	}
}

func (r *contextAgentKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	model, id, err := splitImportID(req.ID, "<model>/<key_id>")
	if err != nil {
		res.Diagnostics.AddError("Invalid import id", err.Error())
		return
	}
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("id"), id)...)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("model"), model)...)
}
