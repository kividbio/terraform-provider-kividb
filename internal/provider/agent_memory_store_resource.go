package provider

import (
	"context"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/float64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

var (
	_ resource.Resource                   = (*agentMemoryStoreResource)(nil)
	_ resource.ResourceWithConfigure      = (*agentMemoryStoreResource)(nil)
	_ resource.ResourceWithImportState    = (*agentMemoryStoreResource)(nil)
	_ resource.ResourceWithValidateConfig = (*agentMemoryStoreResource)(nil)
)

func NewAgentMemoryStoreResource() resource.Resource { return &agentMemoryStoreResource{} }

type agentMemoryStoreResource struct{ client *Client }

type agentMemoryStoreModel struct {
	ID                     types.String  `tfsdk:"id"`
	InstanceID             types.String  `tfsdk:"instance_id"`
	Name                   types.String  `tfsdk:"name"`
	EmbedProvider          types.String  `tfsdk:"embed_provider"`
	EmbedModel             types.String  `tfsdk:"embed_model"`
	EmbedBaseURL           types.String  `tfsdk:"embed_base_url"`
	EmbedAPIKey            types.String  `tfsdk:"embed_api_key"`
	HasEmbedAPIKey         types.Bool    `tfsdk:"has_embed_api_key"`
	Dimensions             types.Int64   `tfsdk:"dimensions"`
	LLMProvider            types.String  `tfsdk:"llm_provider"`
	LLMModel               types.String  `tfsdk:"llm_model"`
	LLMBaseURL             types.String  `tfsdk:"llm_base_url"`
	LLMAPIKey              types.String  `tfsdk:"llm_api_key"`
	HasLLMAPIKey           types.Bool    `tfsdk:"has_llm_api_key"`
	ExtractionDelaySeconds types.Int64   `tfsdk:"extraction_delay_seconds"`
	SummarizeAfter         types.Int64   `tfsdk:"summarize_after"`
	KeepRecent             types.Int64   `tfsdk:"keep_recent"`
	SessionTTLSeconds      types.Int64   `tfsdk:"session_ttl_seconds"`
	MemoryTTLSeconds       types.Int64   `tfsdk:"memory_ttl_seconds"`
	DedupeThreshold        types.Float64 `tfsdk:"dedupe_threshold"`
	CustomTypes            types.List    `tfsdk:"custom_types"`
	MemoryTypes            types.List    `tfsdk:"memory_types"`
	RedactPII              types.Bool    `tfsdk:"redact_pii"`
	CreatedAt              types.String  `tfsdk:"created_at"`
}

type agentMemoryCustomTypeModel struct {
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
}

var agentMemoryCustomTypeAttrTypes = map[string]attr.Type{
	"name":        types.StringType,
	"description": types.StringType,
}

// customTypeNamePattern is the API's name rule for a kind of memory.
var customTypeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func (r *agentMemoryStoreResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_agent_memory_store"
}

func (r *agentMemoryStoreResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "An agent memory store on a database: what an AI agent should remember, within a " +
			"conversation (working memory: a session's recent messages and a running summary) and across " +
			"them (long-term memories, searchable by meaning or by words).\n\n" +
			"Sessions and memories live in the database itself. **Destroying this resource deletes every " +
			"session and every memory in the store**, and so does anything that replaces it (a different " +
			"`instance_id` or `name`). The database must be running to delete the store. Not available on " +
			"sharded databases.\n\n" +
			"Messages and memories are added and searched by your application through the API, not by Terraform.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<instance_id>/<name>`, which is also the import id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database the store lives in. Changing it makes a new, empty store there and deletes this one with everything in it.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.RegexMatches(uuidPattern, "must be a database id (a UUID)")},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The store's name, unique within the database: 1-63 lowercase letters, digits, " +
					"`-` or `_`, starting with a letter or digit. A database holds at most 20 stores. Changing " +
					"it makes a new, empty store and deletes this one with everything in it.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(aiNamePattern, aiNameRule)},
			},
			"embed_provider": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Where memories and queries are turned into vectors. `none`: your application " +
					"sends its own vector with each memory and each semantic search, and keyword search works " +
					"without one. `openai` or `openai_compatible`: that service, with your key. Changes in place.",
				Validators: []validator.String{stringvalidator.OneOf("openai", "openai_compatible", "none")},
			},
			"embed_model": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The embedding model, e.g. `text-embedding-3-small`. Required unless `embed_provider` is `none`, and not used when it is.",
				Validators:          []validator.String{stringvalidator.LengthBetween(1, 200)},
			},
			"embed_base_url": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "For `openai_compatible` only, and required for it: the https URL its `/embeddings` sits under. Addresses on private or reserved networks are refused.",
				Validators: []validator.String{
					stringvalidator.LengthAtMost(2048),
					stringvalidator.RegexMatches(httpsPattern, "must be an https:// URL"),
				},
			},
			"embed_api_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Your API key for the embedding provider. KiviDB stores it encrypted and never " +
					"returns it, so Terraform keeps the value you gave in state and cannot detect a key changed " +
					"elsewhere. Leaving it out on a later apply keeps the stored key, as long as the provider stays the same.",
				Validators: []validator.String{stringvalidator.LengthBetween(1, 4096)},
			},
			"has_embed_api_key": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether KiviDB holds an API key for the embedding provider.",
			},
			"dimensions": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "The length of every vector in the store, 2-8192. Changes in place only while " +
					"the store holds no long-term memories; with memories in it the API refuses.",
				Validators: []validator.Int64{int64validator.Between(2, 8192)},
			},
			"llm_provider": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The language model that summarizes long sessions and extracts long-term memories " +
					"from them in the background. `none`: no model is called; sessions are kept but not summarized, " +
					"and long-term memories are only those your application adds. `openai`, `anthropic` or " +
					"`openai_compatible`: that service, with your key. Changes in place.",
				Validators: []validator.String{stringvalidator.OneOf("openai", "anthropic", "openai_compatible", "none")},
			},
			"llm_model": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The language model, e.g. `gpt-4o-mini`. Required unless `llm_provider` is `none`, and not used when it is.",
				Validators:          []validator.String{stringvalidator.LengthBetween(1, 200)},
			},
			"llm_base_url": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "For `openai_compatible` only, and required for it: the https URL of its OpenAI-style API. Addresses on private or reserved networks are refused.",
				Validators: []validator.String{
					stringvalidator.LengthAtMost(2048),
					stringvalidator.RegexMatches(httpsPattern, "must be an https:// URL"),
				},
			},
			"llm_api_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Your API key for the language model. Stored encrypted and never returned, " +
					"like `embed_api_key`, and kept on a later apply that leaves it out, for the same provider.",
				Validators: []validator.String{stringvalidator.LengthBetween(1, 4096)},
			},
			"has_llm_api_key": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether KiviDB holds an API key for the language model.",
			},
			"extraction_delay_seconds": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(300),
				MarkdownDescription: "How long after a session's last new message long-term memories are extracted " +
					"from it, 30-3600 seconds. Defaults to 300.",
				Validators: []validator.Int64{int64validator.Between(30, 3600)},
			},
			"summarize_after": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(40),
				MarkdownDescription: "Summarize a session once it holds more than this many messages, 4-1000. " +
					"Defaults to 40.",
				Validators: []validator.Int64{int64validator.Between(4, 1000)},
			},
			"keep_recent": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(12),
				MarkdownDescription: "How many of the most recent messages a summary leaves as they are, 1-500, and " +
					"less than `summarize_after`. Defaults to 12.",
				Validators: []validator.Int64{int64validator.Between(1, 500)},
			},
			"session_ttl_seconds": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(3600),
				MarkdownDescription: "How long a session's working memory is kept after its last message, 60-2592000 " +
					"seconds (30 days). Defaults to 3600.",
				Validators: []validator.Int64{int64validator.Between(60, 2_592_000)},
			},
			"memory_ttl_seconds": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(31_536_000),
				MarkdownDescription: "How long a long-term memory is kept, 3600-157680000 seconds (5 years). " +
					"Defaults to 31536000 (a year).",
				Validators: []validator.Int64{int64validator.Between(3600, 157_680_000)},
			},
			"dedupe_threshold": schema.Float64Attribute{
				Optional: true,
				Computed: true,
				// No Default: a float64 default never compares equal to the same
				// number read back from state, which planned a change on every run.
				PlanModifiers: []planmodifier.Float64{float64planmodifier.UseStateForUnknown()},
				MarkdownDescription: "A new memory at least this similar (cosine, above 0 and at most 1) to one the " +
					"same actor already has is treated as a duplicate. The API uses 0.92 when the store is created " +
					"without one; left out of the configuration afterwards, the current value is kept.",
				Validators: []validator.Float64{float64validator.AtLeast(0), float64validator.NoneOf(0), float64validator.AtMost(1)},
			},
			"custom_types": schema.ListNestedAttribute{
				Optional: true,
				Computed: true,
				Default:  listdefault.StaticValue(types.ListValueMust(types.ObjectType{AttrTypes: agentMemoryCustomTypeAttrTypes}, []attr.Value{})),
				MarkdownDescription: "Up to three kinds of memory of your own, besides the built-in `semantic` " +
					"(facts and preferences) and `episodic` (what happened). The language model is told to " +
					"extract each, by its description.",
				Validators: []validator.List{listvalidator.SizeAtMost(3)},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "1-32 lowercase letters, digits and `_`, starting with a letter; " +
								"not `semantic` or `episodic`, and different from the others.",
							Validators: []validator.String{
								stringvalidator.RegexMatches(customTypeNamePattern, "must be 1-32 lowercase letters, digits and _, starting with a letter"),
								stringvalidator.NoneOf("semantic", "episodic"),
							},
						},
						"description": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "What to extract as this kind of memory, in plain words, 10-2000 characters. Given to the language model.",
							Validators:          []validator.String{stringvalidator.LengthBetween(10, 2000)},
						},
					},
				},
			},
			"memory_types": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Every kind of memory the store keeps: `semantic`, `episodic` and the names in `custom_types`.",
			},
			"redact_pii": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Replace common personal identifiers in a long-term memory before it is stored: " +
					"email addresses, phone numbers, payment card numbers, US social security numbers and IPv4 " +
					"addresses each become a marker such as `[email]`. These are patterns, so names and postal " +
					"addresses are not caught. Defaults to true.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the store was created.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *agentMemoryStoreResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
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

func (r *agentMemoryStoreResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, res *resource.ValidateConfigResponse) {
	var cfg agentMemoryStoreModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() {
		return
	}
	checkProvider(&res.Diagnostics, "embed_provider", cfg.EmbedProvider, cfg.EmbedModel, cfg.EmbedBaseURL, cfg.EmbedAPIKey,
		"embed_model", "embed_base_url", "embed_api_key")
	checkProvider(&res.Diagnostics, "llm_provider", cfg.LLMProvider, cfg.LLMModel, cfg.LLMBaseURL, cfg.LLMAPIKey,
		"llm_model", "llm_base_url", "llm_api_key")

	// keep_recent < summarize_after, with the API's defaults for whichever is unset.
	keep, after := int64(12), int64(40)
	if cfg.KeepRecent.IsUnknown() || cfg.SummarizeAfter.IsUnknown() {
		return
	}
	if !cfg.KeepRecent.IsNull() {
		keep = cfg.KeepRecent.ValueInt64()
	}
	if !cfg.SummarizeAfter.IsNull() {
		after = cfg.SummarizeAfter.ValueInt64()
	}
	if keep >= after {
		res.Diagnostics.AddAttributeError(path.Root("keep_recent"), "keep_recent too large",
			"`keep_recent` must be less than `summarize_after` (which defaults to 40).")
	}

	if cfg.CustomTypes.IsNull() || cfg.CustomTypes.IsUnknown() {
		return
	}
	var types_ []agentMemoryCustomTypeModel
	if d := cfg.CustomTypes.ElementsAs(ctx, &types_, false); d.HasError() {
		return
	}
	seen := map[string]bool{}
	for _, t := range types_ {
		if t.Name.IsUnknown() || t.Name.IsNull() {
			continue
		}
		if seen[t.Name.ValueString()] {
			res.Diagnostics.AddAttributeError(path.Root("custom_types"), "Duplicate memory type",
				"Each custom type needs a different name; "+t.Name.ValueString()+" is listed twice.")
		}
		seen[t.Name.ValueString()] = true
	}
}

func (m *agentMemoryStoreModel) request(ctx context.Context) (AgentMemoryRequest, diag.Diagnostics) {
	var diags diag.Diagnostics
	custom := []AgentMemoryCustomType{}
	if !m.CustomTypes.IsNull() && !m.CustomTypes.IsUnknown() {
		var ts []agentMemoryCustomTypeModel
		diags.Append(m.CustomTypes.ElementsAs(ctx, &ts, false)...)
		for _, t := range ts {
			custom = append(custom, AgentMemoryCustomType{Name: t.Name.ValueString(), Description: t.Description.ValueString()})
		}
	}
	var redact *bool
	if !m.RedactPII.IsNull() && !m.RedactPII.IsUnknown() {
		v := m.RedactPII.ValueBool()
		redact = &v
	}
	return AgentMemoryRequest{
		EmbedProvider:          m.EmbedProvider.ValueString(),
		EmbedModel:             strPtr(m.EmbedModel),
		EmbedBaseURL:           strPtr(m.EmbedBaseURL),
		EmbedAPIKey:            strPtr(m.EmbedAPIKey),
		Dimensions:             m.Dimensions.ValueInt64(),
		LLMProvider:            m.LLMProvider.ValueString(),
		LLMModel:               strPtr(m.LLMModel),
		LLMBaseURL:             strPtr(m.LLMBaseURL),
		LLMAPIKey:              strPtr(m.LLMAPIKey),
		ExtractionDelaySeconds: int64Ptr(m.ExtractionDelaySeconds),
		SummarizeAfter:         int64Ptr(m.SummarizeAfter),
		KeepRecent:             int64Ptr(m.KeepRecent),
		SessionTTLSeconds:      int64Ptr(m.SessionTTLSeconds),
		MemoryTTLSeconds:       int64Ptr(m.MemoryTTLSeconds),
		DedupeThreshold:        float64Ptr(m.DedupeThreshold),
		CustomTypes:            custom,
		RedactPII:              redact,
	}, diags
}

// apply copies the server's answer over the model, keeping the API keys the
// model already holds: the answer never carries them.
func (m *agentMemoryStoreModel) apply(instanceID string, s *AgentMemoryStore) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(instanceID + "/" + s.Name)
	m.InstanceID = types.StringValue(instanceID)
	m.Name = types.StringValue(s.Name)
	m.EmbedProvider = types.StringValue(s.EmbedProvider)
	m.EmbedModel = keepOptionalSpelling(m.EmbedModel, s.EmbedModel)
	m.EmbedBaseURL = stringOrNull(s.EmbedBaseURL)
	m.HasEmbedAPIKey = types.BoolValue(s.HasEmbedAPIKey)
	m.Dimensions = types.Int64Value(s.Dimensions)
	m.LLMProvider = types.StringValue(s.LLMProvider)
	m.LLMModel = keepOptionalSpelling(m.LLMModel, s.LLMModel)
	m.LLMBaseURL = stringOrNull(s.LLMBaseURL)
	m.HasLLMAPIKey = types.BoolValue(s.HasLLMAPIKey)
	m.ExtractionDelaySeconds = types.Int64Value(s.ExtractionDelaySeconds)
	m.SummarizeAfter = types.Int64Value(s.SummarizeAfter)
	m.KeepRecent = types.Int64Value(s.KeepRecent)
	m.SessionTTLSeconds = types.Int64Value(s.SessionTTLSeconds)
	m.MemoryTTLSeconds = types.Int64Value(s.MemoryTTLSeconds)
	m.DedupeThreshold = types.Float64Value(s.DedupeThreshold)
	m.RedactPII = types.BoolValue(s.RedactPII)
	m.CreatedAt = types.StringValue(s.CreatedAt)
	if s.EmbedProvider == "none" {
		m.EmbedAPIKey = types.StringNull()
	}
	if s.LLMProvider == "none" {
		m.LLMAPIKey = types.StringNull()
	}

	// The API trims each description; the configured spelling of the same
	// text is kept, element by element.
	var prior []agentMemoryCustomTypeModel
	if !m.CustomTypes.IsNull() && !m.CustomTypes.IsUnknown() {
		diags.Append(m.CustomTypes.ElementsAs(context.Background(), &prior, false)...)
	}
	objType := types.ObjectType{AttrTypes: agentMemoryCustomTypeAttrTypes}
	custom := make([]attr.Value, 0, len(s.CustomTypes))
	for i, t := range s.CustomTypes {
		desc := types.StringValue(t.Description)
		if i < len(prior) {
			desc = keepSpelling(prior[i].Description, t.Description)
		}
		o, d := types.ObjectValue(agentMemoryCustomTypeAttrTypes, map[string]attr.Value{
			"name":        types.StringValue(t.Name),
			"description": desc,
		})
		diags.Append(d...)
		custom = append(custom, o)
	}
	var d diag.Diagnostics
	m.CustomTypes, d = types.ListValue(objType, custom)
	diags.Append(d...)

	memTypes := s.MemoryTypes
	if memTypes == nil {
		memTypes = []string{}
	}
	var mt basetypes.ListValue
	mt, d = types.ListValueFrom(context.Background(), types.StringType, memTypes)
	diags.Append(d...)
	m.MemoryTypes = mt
	return diags
}

func (r *agentMemoryStoreResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan agentMemoryStoreModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	instanceID, name := plan.InstanceID.ValueString(), plan.Name.ValueString()

	if _, err := r.client.GetAgentMemoryStore(ctx, instanceID, name); err == nil {
		alreadyExists(&res.Diagnostics, "The agent memory store "+name, "kividb_agent_memory_store.<name>", instanceID+"/"+name)
		return
	} else if !isGone(err) {
		res.Diagnostics.AddError("Could not check for an existing memory store", explainAI(err))
		return
	}

	body, d := plan.request(ctx)
	res.Diagnostics.Append(d...)
	if res.Diagnostics.HasError() {
		return
	}
	s, err := r.client.PutAgentMemoryStore(ctx, instanceID, name, body)
	if err != nil {
		res.Diagnostics.AddError("Could not create the agent memory store", explainAI(err))
		return
	}
	res.Diagnostics.Append(plan.apply(instanceID, s)...)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *agentMemoryStoreResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state agentMemoryStoreModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	instanceID := state.InstanceID.ValueString()
	s, err := r.client.GetAgentMemoryStore(ctx, instanceID, state.Name.ValueString())
	if err != nil {
		if isGone(err) {
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the agent memory store", explainAI(err))
		return
	}
	res.Diagnostics.Append(state.apply(instanceID, s)...)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *agentMemoryStoreResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan agentMemoryStoreModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	instanceID := plan.InstanceID.ValueString()
	body, d := plan.request(ctx)
	res.Diagnostics.Append(d...)
	if res.Diagnostics.HasError() {
		return
	}
	s, err := r.client.PutAgentMemoryStore(ctx, instanceID, plan.Name.ValueString(), body)
	if err != nil {
		res.Diagnostics.AddError("Could not change the agent memory store", explainAI(err))
		return
	}
	res.Diagnostics.Append(plan.apply(instanceID, s)...)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *agentMemoryStoreResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state agentMemoryStoreModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteAgentMemoryStore(ctx, state.InstanceID.ValueString(), state.Name.ValueString()); err != nil {
		if isGone(err) {
			return
		}
		res.Diagnostics.AddError("Could not delete the agent memory store", explainAI(err))
	}
}

func (r *agentMemoryStoreResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	instanceID, name, err := splitImportID(req.ID, "<instance_id>/<name>")
	if err != nil {
		res.Diagnostics.AddError("Invalid import id", err.Error())
		return
	}
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("instance_id"), instanceID)...)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("name"), name)...)
}
