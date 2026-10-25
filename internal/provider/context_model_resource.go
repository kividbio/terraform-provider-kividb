package provider

import (
	"context"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*contextModelResource)(nil)
	_ resource.ResourceWithConfigure   = (*contextModelResource)(nil)
	_ resource.ResourceWithImportState = (*contextModelResource)(nil)
)

func NewContextModelResource() resource.Resource { return &contextModelResource{} }

type contextModelResource struct{ client *Client }

// The entities are nested attributes rather than one jsonencode()d string.
// A plan then names the entity and field that changed ("entities[1].fields[2].type")
// instead of showing two walls of JSON, the API's defaults (storage, id_field,
// relationships) are filled in as known values rather than as a whitespace and
// key-order difference against what the server stored, and every name, type and
// length is checked at plan time rather than by a 400 halfway through an apply.
type contextModelModel struct {
	ID          types.String         `tfsdk:"id"`
	Name        types.String         `tfsdk:"name"`
	InstanceID  types.String         `tfsdk:"instance_id"`
	Description types.String         `tfsdk:"description"`
	Entities    []contextEntityModel `tfsdk:"entities"`
	Tools       types.List           `tfsdk:"tools"`
	MCPURL      types.String         `tfsdk:"mcp_url"`
	CreatedAt   types.String         `tfsdk:"created_at"`
}

type contextEntityModel struct {
	Name          types.String               `tfsdk:"name"`
	Description   types.String               `tfsdk:"description"`
	Prefix        types.String               `tfsdk:"prefix"`
	Storage       types.String               `tfsdk:"storage"`
	IDField       types.String               `tfsdk:"id_field"`
	Fields        []contextFieldModel        `tfsdk:"fields"`
	Relationships []contextRelationshipModel `tfsdk:"relationships"`
}

type contextFieldModel struct {
	Name        types.String `tfsdk:"name"`
	Type        types.String `tfsdk:"type"`
	Description types.String `tfsdk:"description"`
}

type contextRelationshipModel struct {
	Name        types.String `tfsdk:"name"`
	Entity      types.String `tfsdk:"entity"`
	Field       types.String `tfsdk:"field"`
	Description types.String `tfsdk:"description"`
}

var contextRelationshipAttrTypes = map[string]attr.Type{
	"name":        types.StringType,
	"entity":      types.StringType,
	"field":       types.StringType,
	"description": types.StringType,
}

// The API's patterns (packages/shared/src/context-retriever.ts).
var (
	contextEntityNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	contextFieldNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	contextPrefixPattern     = regexp.MustCompile(`^[^\s*?\[\]]+$`)
)

const (
	contextEntityNameRule = "must be 1-32 lowercase letters, digits and _, starting with a letter"
	contextFieldNameRule  = "must be 1-64 letters, digits and _, starting with a letter or _"
)

func (r *contextModelResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_context_model"
}

func (r *contextModelResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	entityName := []validator.String{stringvalidator.RegexMatches(contextEntityNamePattern, contextEntityNameRule)}
	fieldName := []validator.String{stringvalidator.RegexMatches(contextFieldNamePattern, contextFieldNameRule)}
	res.Schema = schema.Schema{
		MarkdownDescription: "A Context Retriever model: a description of the records already in a database " +
			"(customers, orders, tickets) that KiviDB turns into search indexes and into tools an AI agent " +
			"can call, over the Model Context Protocol, with a `kividb_context_agent_key`.\n\n" +
			"Every apply that creates or changes the model rebuilds its indexes, and the database must be " +
			"running for it. **Destroying this resource deletes the model, its search indexes and every agent " +
			"key made for it**, so agents using those keys stop working at once. The records themselves stay " +
			"in the database. Not available on sharded databases.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The model's name, which is also the import id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The model's name, unique within the organization: 1-63 lowercase letters, " +
					"digits, `-` or `_`, starting with a letter or digit. An organization has at most 20 models. " +
					"Changing it replaces the model, which revokes its agent keys.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(aiNamePattern, aiNameRule)},
			},
			"instance_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The database whose records the model describes. The API does not move a " +
					"model to another database, so changing this replaces the model, which revokes its agent keys.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(uuidPattern, "must be a database id (a UUID)")},
			},
			"description": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(""),
				MarkdownDescription: "What the data is, for the agent: it is sent as the MCP server's instructions. " +
					"Up to 2000 characters. Changes in place.",
				Validators: []validator.String{stringvalidator.LengthAtMost(2000)},
			},
			"entities": schema.ListNestedAttribute{
				Required: true,
				MarkdownDescription: "The kinds of record, 1-20, each with different names. Changes in place, " +
					"and the indexes are rebuilt.",
				Validators: []validator.List{listvalidator.SizeBetween(1, 20)},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "The entity's name, e.g. `customer`: 1-32 lowercase letters, digits and `_`, starting with a letter. It names the agent's tools.",
							Validators:          entityName,
						},
						"description": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "What it is, in a sentence, 5-1000 characters: the agent reads this to decide which tool to call.",
							Validators:          []validator.String{stringvalidator.LengthBetween(5, 1000)},
						},
						"prefix": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "The key prefix every record of this entity has, e.g. `customer:`. Up to 100 characters, with no spaces or `*`, `?`, `[`, `]`.",
							Validators: []validator.String{
								stringvalidator.LengthBetween(1, 100),
								stringvalidator.RegexMatches(contextPrefixPattern, "must have no spaces or wildcard characters (* ? [ ])"),
							},
						},
						"storage": schema.StringAttribute{
							Optional:            true,
							Computed:            true,
							Default:             stringdefault.StaticString("hash"),
							MarkdownDescription: "How each record is stored: `hash` (the default) or `json`.",
							Validators:          []validator.String{stringvalidator.OneOf("hash", "json")},
						},
						"id_field": schema.StringAttribute{
							Optional:            true,
							Computed:            true,
							Default:             stringdefault.StaticString("id"),
							MarkdownDescription: "The field holding the record's id, indexed as a tag so a record can be fetched by it. Defaults to `id`.",
							Validators:          fieldName,
						},
						"fields": schema.ListNestedAttribute{
							Required:            true,
							MarkdownDescription: "The fields to index, 1-50, each with a different name.",
							Validators:          []validator.List{listvalidator.SizeBetween(1, 50)},
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"name": schema.StringAttribute{
										Required:            true,
										MarkdownDescription: "The field's name in the record: 1-64 letters, digits and `_`, starting with a letter or `_`.",
										Validators:          fieldName,
									},
									"type": schema.StringAttribute{
										Required: true,
										MarkdownDescription: "`text`: searched by its words. `tag`: matched exactly (ids, statuses, " +
											"tenants). `numeric`: compared and ranged.",
										Validators: []validator.String{stringvalidator.OneOf("text", "tag", "numeric")},
									},
									"description": schema.StringAttribute{
										Optional:            true,
										MarkdownDescription: "What the field holds, for the agent. Up to 500 characters.",
										Validators:          []validator.String{stringvalidator.LengthAtMost(500)},
									},
								},
							},
						},
						"relationships": schema.ListNestedAttribute{
							Optional: true,
							Computed: true,
							Default: listdefault.StaticValue(types.ListValueMust(
								types.ObjectType{AttrTypes: contextRelationshipAttrTypes}, []attr.Value{})),
							MarkdownDescription: "Up to 10 links to other entities of this model, which become tools " +
								"that follow them (a customer's orders).",
							Validators: []validator.List{listvalidator.SizeAtMost(10)},
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"name": schema.StringAttribute{
										Required:            true,
										MarkdownDescription: "The relationship's name, e.g. `orders`: 1-32 lowercase letters, digits and `_`, starting with a letter.",
										Validators:          entityName,
									},
									"entity": schema.StringAttribute{
										Required:            true,
										MarkdownDescription: "The related entity, by its `name` in this model.",
										Validators:          entityName,
									},
									"field": schema.StringAttribute{
										Required: true,
										MarkdownDescription: "The field on the related entity that holds this entity's id: for " +
											"customer to orders, the orders' `customer_id`. It must be a `tag` or `numeric` " +
											"field of that entity, or its `id_field`.",
										Validators: fieldName,
									},
									"description": schema.StringAttribute{
										Optional:            true,
										MarkdownDescription: "What the link means, for the agent. Up to 500 characters.",
										Validators:          []validator.String{stringvalidator.LengthAtMost(500)},
									},
								},
							},
						},
					},
				},
			},
			"tools": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "The names of the tools the model gives an agent with full access. A key limited to some entities sees only theirs.",
			},
			"mcp_url": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Where an agent connects over the Model Context Protocol (JSON-RPC over HTTP POST), " +
					"sending one of this model's agent keys as `Authorization: Bearer kvctx_...`. The key names " +
					"the model, so the URL is the same for every model.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the model was created.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *contextModelResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
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

func (m *contextModelModel) request() ContextModelRequest {
	entities := make([]ContextEntity, 0, len(m.Entities))
	for _, e := range m.Entities {
		fields := make([]ContextField, 0, len(e.Fields))
		for _, f := range e.Fields {
			fields = append(fields, ContextField{Name: f.Name.ValueString(), Type: f.Type.ValueString(), Description: strPtr(f.Description)})
		}
		rels := make([]ContextRelationship, 0, len(e.Relationships))
		for _, rel := range e.Relationships {
			rels = append(rels, ContextRelationship{
				Name: rel.Name.ValueString(), Entity: rel.Entity.ValueString(), Field: rel.Field.ValueString(),
				Description: strPtr(rel.Description),
			})
		}
		entities = append(entities, ContextEntity{
			Name:          e.Name.ValueString(),
			Description:   e.Description.ValueString(),
			Prefix:        e.Prefix.ValueString(),
			Storage:       e.Storage.ValueString(),
			IDField:       e.IDField.ValueString(),
			Fields:        fields,
			Relationships: rels,
		})
	}
	return ContextModelRequest{
		InstanceID:  m.InstanceID.ValueString(),
		Description: m.Description.ValueString(),
		Entities:    entities,
	}
}

// apply copies the server's answer over the model. Element by element, it keeps
// the configured spelling of any description the server only trimmed.
func (m *contextModelModel) apply(d *ContextModelDetail, mcpURL string) {
	prior := m.Entities
	m.ID = types.StringValue(d.Model.Name)
	m.Name = types.StringValue(d.Model.Name)
	m.InstanceID = types.StringValue(d.Model.Instance.ID)
	m.Description = keepSpelling(m.Description, d.Model.Description)
	m.CreatedAt = types.StringValue(d.Model.CreatedAt)
	m.MCPURL = types.StringValue(mcpURL)

	entities := make([]contextEntityModel, 0, len(d.Model.Entities))
	for i, e := range d.Model.Entities {
		var pe contextEntityModel
		if i < len(prior) {
			pe = prior[i]
		}
		fields := make([]contextFieldModel, 0, len(e.Fields))
		for j, f := range e.Fields {
			var pf contextFieldModel
			if j < len(pe.Fields) {
				pf = pe.Fields[j]
			}
			fields = append(fields, contextFieldModel{
				Name: types.StringValue(f.Name), Type: types.StringValue(f.Type),
				Description: keepOptionalSpelling(pf.Description, f.Description),
			})
		}
		rels := make([]contextRelationshipModel, 0, len(e.Relationships))
		for j, rel := range e.Relationships {
			var pr contextRelationshipModel
			if j < len(pe.Relationships) {
				pr = pe.Relationships[j]
			}
			rels = append(rels, contextRelationshipModel{
				Name: types.StringValue(rel.Name), Entity: types.StringValue(rel.Entity), Field: types.StringValue(rel.Field),
				Description: keepOptionalSpelling(pr.Description, rel.Description),
			})
		}
		storage, idField := e.Storage, e.IDField
		if storage == "" {
			storage = "hash"
		}
		if idField == "" {
			idField = "id"
		}
		entities = append(entities, contextEntityModel{
			Name:          types.StringValue(e.Name),
			Description:   keepSpelling(pe.Description, e.Description),
			Prefix:        types.StringValue(e.Prefix),
			Storage:       types.StringValue(storage),
			IDField:       types.StringValue(idField),
			Fields:        fields,
			Relationships: rels,
		})
	}
	m.Entities = entities

	tools := make([]attr.Value, 0, len(d.Tools))
	for _, t := range d.Tools {
		tools = append(tools, types.StringValue(t.Name))
	}
	m.Tools = types.ListValueMust(types.StringType, tools)
}

func (r *contextModelResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan contextModelModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()

	if _, err := r.client.GetContextModel(ctx, name); err == nil {
		alreadyExists(&res.Diagnostics, "The context model "+name, "kividb_context_model.<name>", name)
		return
	} else if !isGone(err) {
		res.Diagnostics.AddError("Could not check for an existing context model", explainAI(err))
		return
	}

	d, err := r.client.PutContextModel(ctx, name, plan.request())
	if err != nil {
		res.Diagnostics.AddError("Could not create the context model", explainContextModel(err))
		// index_failed means the model was saved. Keep it in state (Terraform
		// marks it tainted and replaces it on the next apply) rather than
		// leaving a model that the next create would refuse as already existing.
		if apiErr, ok := err.(*APIError); ok && apiErr.Code == "index_failed" {
			if saved, gerr := r.client.GetContextModel(ctx, name); gerr == nil {
				plan.apply(saved, r.client.ContextMCPURL())
				res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
			}
		}
		return
	}
	plan.apply(d, r.client.ContextMCPURL())
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *contextModelResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state contextModelModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	d, err := r.client.GetContextModel(ctx, state.Name.ValueString())
	if err != nil {
		if isGone(err) {
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the context model", explainAI(err))
		return
	}
	state.apply(d, r.client.ContextMCPURL())
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *contextModelResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan contextModelModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	d, err := r.client.PutContextModel(ctx, plan.Name.ValueString(), plan.request())
	if err != nil {
		res.Diagnostics.AddError("Could not change the context model", explainContextModel(err))
		return
	}
	plan.apply(d, r.client.ContextMCPURL())
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *contextModelResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state contextModelModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteContextModel(ctx, state.Name.ValueString()); err != nil {
		if isGone(err) {
			return
		}
		res.Diagnostics.AddError("Could not delete the context model", explainAI(err))
	}
}

func (r *contextModelResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	if !aiNamePattern.MatchString(req.ID) {
		res.Diagnostics.AddError("Invalid import id", "Import a context model by its name, which "+aiNameRule+".")
		return
	}
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
}

// explainContextModel adds the one refusal specific to a model's save: the row
// was saved and only the indexes failed, so applying again retries just those.
func explainContextModel(err error) string {
	if apiErr, ok := err.(*APIError); ok && apiErr.Code == "index_failed" {
		return apiErr.Message + "\n\nThe model's settings were saved, but its indexes were not rebuilt, and a " +
			"refresh will read the saved settings as up to date. Once the database answers, rebuild them by " +
			"saving the model in the console, or with `terraform apply -replace=<address>` (which also " +
			"revokes the model's agent keys)."
	}
	return explainAI(err)
}
