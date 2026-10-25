package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/float64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*semanticCacheResource)(nil)
	_ resource.ResourceWithConfigure      = (*semanticCacheResource)(nil)
	_ resource.ResourceWithImportState    = (*semanticCacheResource)(nil)
	_ resource.ResourceWithValidateConfig = (*semanticCacheResource)(nil)
)

func NewSemanticCacheResource() resource.Resource { return &semanticCacheResource{} }

type semanticCacheResource struct{ client *Client }

type semanticCacheModel struct {
	ID                types.String  `tfsdk:"id"`
	InstanceID        types.String  `tfsdk:"instance_id"`
	Name              types.String  `tfsdk:"name"`
	EmbedProvider     types.String  `tfsdk:"embed_provider"`
	EmbedModel        types.String  `tfsdk:"embed_model"`
	EmbedBaseURL      types.String  `tfsdk:"embed_base_url"`
	EmbedAPIKey       types.String  `tfsdk:"embed_api_key"`
	HasEmbedAPIKey    types.Bool    `tfsdk:"has_embed_api_key"`
	Dimensions        types.Int64   `tfsdk:"dimensions"`
	Threshold         types.Float64 `tfsdk:"threshold"`
	DefaultTTLSeconds types.Int64   `tfsdk:"default_ttl_seconds"`
	CreatedAt         types.String  `tfsdk:"created_at"`
}

func (r *semanticCacheResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_semantic_cache"
}

func (r *semanticCacheResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "A semantic cache namespace on a database: a place to store an LLM's answer " +
			"to a prompt and find it again for a prompt that means the same thing.\n\n" +
			"The cached entries live in the database itself, under keys beginning " +
			"`kividb:semcache:{<name>}:`. **Destroying this resource deletes every entry cached in the " +
			"namespace**, and so does anything that replaces it (a different `instance_id` or `name`). " +
			"The database must be running to delete the namespace.\n\n" +
			"Entries are written and looked up by your application through the API " +
			"(`POST .../semantic-cache/<name>/entries` and `.../lookup`), not by Terraform.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<instance_id>/<name>`, which is also the import id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database the namespace lives in. Changing it makes a new, empty namespace there and deletes this one with everything in it.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.RegexMatches(uuidPattern, "must be a database id (a UUID)")},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The namespace's name, unique within the database: 1-63 lowercase letters, digits, " +
					"`-` or `_`, starting with a letter or digit. A database holds at most 50 namespaces. " +
					"Changing it makes a new, empty namespace and deletes this one with everything in it.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(aiNamePattern, aiNameRule)},
			},
			"embed_provider": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Where prompts are turned into vectors. `none`: your application sends its own " +
					"vector with every write and lookup, and KiviDB never calls a model. `openai`: OpenAI's " +
					"embeddings API, with your key. `openai_compatible`: any service with an OpenAI-style " +
					"`/embeddings` endpoint at `embed_base_url`, with your key. The API calls this `provider`. " +
					"Changes in place.",
				Validators: []validator.String{stringvalidator.OneOf("openai", "openai_compatible", "none")},
			},
			"embed_model": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The embedding model, e.g. `text-embedding-3-small`. Required unless " +
					"`embed_provider` is `none`, and not used when it is.",
				Validators: []validator.String{stringvalidator.LengthBetween(1, 200)},
			},
			"embed_base_url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "For `openai_compatible` only, and required for it: the https URL its " +
					"`/embeddings` sits under. Addresses on private or reserved networks are refused.",
				Validators: []validator.String{
					stringvalidator.LengthAtMost(2048),
					stringvalidator.RegexMatches(httpsPattern, "must be an https:// URL"),
				},
			},
			"embed_api_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Your API key for the embedding provider. Required to create a namespace with " +
					"a provider other than `none`. KiviDB stores it encrypted and never returns it, so " +
					"Terraform keeps the value you gave in state and cannot detect a key changed elsewhere. " +
					"Leaving it out on a later apply keeps the stored key, as long as the provider stays the same.",
				Validators: []validator.String{stringvalidator.LengthBetween(1, 4096)},
			},
			"has_embed_api_key": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether KiviDB holds an API key for the provider.",
			},
			"dimensions": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "The length of every vector in the namespace, 2-8192: what the model produces " +
					"(1536 for `text-embedding-3-small` unless it is asked for fewer), or what your application " +
					"sends with `none`. Changes in place only while the namespace is empty; with entries in it " +
					"the API refuses, because vectors of two sizes cannot be compared.",
				Validators: []validator.Int64{int64validator.Between(2, 8192)},
			},
			"threshold": schema.Float64Attribute{
				Optional: true,
				Computed: true,
				// No Default: a float64 default never compares equal to the same
				// number read back from state, which planned a change on every run.
				PlanModifiers: []planmodifier.Float64{float64planmodifier.UseStateForUnknown()},
				MarkdownDescription: "The cosine similarity a lookup must reach to count as a hit, above 0 and at " +
					"most 1. The API uses 0.6 when the namespace is created without one, which catches rewordings " +
					"with OpenAI's `text-embedding-3-small` while staying well above unrelated prompts; left out of " +
					"the configuration afterwards, the current value is kept. A lookup may override it. Changes in place.",
				Validators: []validator.Float64{
					float64validator.AtMost(1),
					float64validator.NoneOf(0),
					float64validator.AtLeast(0),
				},
			},
			"default_ttl_seconds": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(86400),
				MarkdownDescription: "How long an entry is kept when the write does not say, in seconds: 60 to " +
					"31536000 (a year). Defaults to 86400 (a day). Changes in place, for entries written " +
					"afterwards.",
				Validators: []validator.Int64{int64validator.Between(60, 31_536_000)},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the namespace was created.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *semanticCacheResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
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

func (r *semanticCacheResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, res *resource.ValidateConfigResponse) {
	var cfg semanticCacheModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() {
		return
	}
	checkProvider(&res.Diagnostics, "embed_provider", cfg.EmbedProvider, cfg.EmbedModel, cfg.EmbedBaseURL, cfg.EmbedAPIKey,
		"embed_model", "embed_base_url", "embed_api_key")
}

func (m *semanticCacheModel) request() SemanticCacheRequest {
	return SemanticCacheRequest{
		Provider:          m.EmbedProvider.ValueString(),
		Model:             strPtr(m.EmbedModel),
		BaseURL:           strPtr(m.EmbedBaseURL),
		APIKey:            strPtr(m.EmbedAPIKey),
		Dimensions:        m.Dimensions.ValueInt64(),
		Threshold:         float64Ptr(m.Threshold),
		DefaultTTLSeconds: int64Ptr(m.DefaultTTLSeconds),
	}
}

// apply copies the server's answer over the model. The API key is not in the
// answer, so whatever the model already holds for it is kept.
func (m *semanticCacheModel) apply(instanceID string, n *SemanticCacheNamespace) {
	m.ID = types.StringValue(instanceID + "/" + n.Name)
	m.InstanceID = types.StringValue(instanceID)
	m.Name = types.StringValue(n.Name)
	m.EmbedProvider = types.StringValue(n.Provider)
	m.EmbedModel = keepOptionalSpelling(m.EmbedModel, n.Model)
	m.EmbedBaseURL = stringOrNull(n.BaseURL)
	m.HasEmbedAPIKey = types.BoolValue(n.HasAPIKey)
	m.Dimensions = types.Int64Value(n.Dimensions)
	m.Threshold = types.Float64Value(n.Threshold)
	m.DefaultTTLSeconds = types.Int64Value(n.DefaultTTLSeconds)
	m.CreatedAt = types.StringValue(n.CreatedAt)
	if n.Provider == "none" {
		m.EmbedAPIKey = types.StringNull()
	}
}

func (r *semanticCacheResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan semanticCacheModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	instanceID, name := plan.InstanceID.ValueString(), plan.Name.ValueString()

	if _, err := r.client.GetSemanticCache(ctx, instanceID, name); err == nil {
		alreadyExists(&res.Diagnostics, "The semantic cache namespace "+name, "kividb_semantic_cache.<name>", instanceID+"/"+name)
		return
	} else if !isGone(err) {
		res.Diagnostics.AddError("Could not check for an existing namespace", explainAI(err))
		return
	}

	n, err := r.client.PutSemanticCache(ctx, instanceID, name, plan.request())
	if err != nil {
		res.Diagnostics.AddError("Could not create the semantic cache namespace", explainAI(err))
		return
	}
	plan.apply(instanceID, n)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *semanticCacheResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state semanticCacheModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	instanceID := state.InstanceID.ValueString()
	n, err := r.client.GetSemanticCache(ctx, instanceID, state.Name.ValueString())
	if err != nil {
		if isGone(err) {
			// The namespace, or its whole database, was deleted elsewhere.
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the semantic cache namespace", explainAI(err))
		return
	}
	state.apply(instanceID, n)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *semanticCacheResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan semanticCacheModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	// The PUT replaces every setting at once, so it is sent whole. Only the
	// instance and the name are in the URL, and both force replacement.
	instanceID := plan.InstanceID.ValueString()
	n, err := r.client.PutSemanticCache(ctx, instanceID, plan.Name.ValueString(), plan.request())
	if err != nil {
		res.Diagnostics.AddError("Could not change the semantic cache namespace", explainAI(err))
		return
	}
	plan.apply(instanceID, n)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *semanticCacheResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state semanticCacheModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteSemanticCache(ctx, state.InstanceID.ValueString(), state.Name.ValueString()); err != nil {
		if isGone(err) {
			return
		}
		res.Diagnostics.AddError("Could not delete the semantic cache namespace", explainAI(err))
	}
}

func (r *semanticCacheResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	instanceID, name, err := splitImportID(req.ID, "<instance_id>/<name>")
	if err != nil {
		res.Diagnostics.AddError("Invalid import id", err.Error())
		return
	}
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("instance_id"), instanceID)...)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("name"), name)...)
}
