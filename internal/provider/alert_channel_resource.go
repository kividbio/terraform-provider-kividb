package provider

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*alertChannelResource)(nil)
	_ resource.ResourceWithConfigure      = (*alertChannelResource)(nil)
	_ resource.ResourceWithImportState    = (*alertChannelResource)(nil)
	_ resource.ResourceWithValidateConfig = (*alertChannelResource)(nil)
)

func NewAlertChannelResource() resource.Resource { return &alertChannelResource{} }

// alertChannelResource is where alerts are delivered.
//
// The API has no update for a channel: create, list, delete. So every
// configurable attribute replaces the channel, with one exception -- a URL or
// PagerDuty key filled in after an import, which the server never returns and
// the provider can only recognise by its hint.
type alertChannelResource struct{ client *Client }

type alertChannelModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Kind          types.String `tfsdk:"kind"`
	Roles         types.Set    `tfsdk:"roles"`
	URL           types.String `tfsdk:"url"`
	RoutingKey    types.String `tfsdk:"routing_key"`
	Region        types.String `tfsdk:"region"`
	URLHint       types.String `tfsdk:"url_hint"`
	KeyHint       types.String `tfsdk:"key_hint"`
	SigningSecret types.String `tfsdk:"signing_secret"`
	CreatedAt     types.String `tfsdk:"created_at"`
}

// The API's own patterns (packages/shared/src/alerts.ts).
var (
	slackWebhookPattern      = regexp.MustCompile(`^https://hooks\.slack\.com/services/[A-Za-z0-9/_-]{10,200}$`)
	pagerDutyRoutingKeyRegex = regexp.MustCompile(`^[A-Za-z0-9]{32}$`)
	teamsOfficeHost          = regexp.MustCompile(`(^|\.)webhook\.office\.com$`)
	teamsLogicHost           = regexp.MustCompile(`\.logic\.azure\.com$`)
	teamsPowerPlatformHost   = regexp.MustCompile(`\.api\.powerplatform\.com$`)
)

var defaultEmailRoles = []string{"owner", "admin"}

// alertNameValidators mirror `z.string().trim().min(1).max(80)`. The server
// trims, so a name with spaces around it would be stored without them and plan
// a replacement on every refresh; refusing it here is the honest version.
func alertNameValidators() []validator.String {
	return []validator.String{
		stringvalidator.LengthBetween(1, 80),
		stringvalidator.RegexMatches(regexp.MustCompile(`^\S(.*\S)?$`),
			"must not be blank or start or end with whitespace (the API trims it)"),
	}
}

func (r *alertChannelResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_alert_channel"
}

func (r *alertChannelResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "Where alerts are delivered: the organization's members by email, a Slack or " +
			"Microsoft Teams channel, a PagerDuty service, or a signed webhook. A `kividb_alert_rule` names " +
			"one or more channels.\n\n" +
			"A channel cannot be edited once created. Changing anything about it plans a new channel and " +
			"destroys this one, and a channel an alert still sends to cannot be destroyed -- so give channels " +
			"`lifecycle { create_before_destroy = true }`, which lets the alert move to the new channel first.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Channel id. This is what `kividb_alert_rule.channel_ids` takes.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "A name for the channel, 1 to 80 characters, as the console and each notice show it.",
				Validators:          alertNameValidators(),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"kind": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "How the notice is delivered: `email`, `slack`, `teams`, `pagerduty` or `webhook`. " +
					"`email` takes `roles`; `slack`, `teams` and `webhook` take `url`; `pagerduty` takes " +
					"`routing_key` and `region`.",
				Validators: []validator.String{
					stringvalidator.OneOf("email", "slack", "teams", "pagerduty", "webhook"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"roles": schema.SetAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				MarkdownDescription: "For `email`: which of the organization's members receive it, by role -- any of " +
					"`owner`, `admin` and `member`. Defaults to owners and admins. An email channel " +
					"sends only to the organization's own members, never to an address you type, so " +
					"an alert cannot be used to mail somebody outside it. Null for other kinds.",
				Validators: []validator.Set{
					setvalidator.SizeBetween(1, 3),
					setvalidator.ValueStringsAre(stringvalidator.OneOf("owner", "admin", "member")),
				},
				PlanModifiers: []planmodifier.Set{defaultEmailRolesModifier{}, setplanmodifier.RequiresReplace()},
			},
			"url": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "For `slack`, `teams` and `webhook`: where to post. Sensitive, because whoever " +
					"holds a Slack or Teams webhook URL can post to that channel; the API never returns it " +
					"whole, only `url_hint`. For `slack`, a Slack incoming-webhook URL " +
					"(`https://hooks.slack.com/services/...`); for `teams`, a Teams Workflows webhook URL (on " +
					"`logic.azure.com` or `api.powerplatform.com`) or an Office 365 connector URL (on " +
					"`webhook.office.com`), over https; for `webhook`, any https URL up to 2048 characters on a " +
					"public address, to which each notice is POSTed as JSON signed with `signing_secret`.",
				PlanModifiers: []planmodifier.String{secretReplaceModifier{hint: path.Root("url_hint"), hintOf: maskURL}},
			},
			"routing_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "For `pagerduty`: the 32-character Integration Key of an Events API v2 " +
					"integration on the PagerDuty service. Anyone holding it can open incidents on that " +
					"service, so the API keeps it sealed and returns only its last four characters, `key_hint`.",
				PlanModifiers: []planmodifier.String{secretReplaceModifier{hint: path.Root("key_hint"), hintOf: pagerDutyKeyHint}},
			},
			"region": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "For `pagerduty`: the PagerDuty service region, `us` or `eu`. Defaults to `us`; " +
					"a PagerDuty account in the EU service region needs `eu`. Null for other kinds.",
				Validators:    []validator.String{stringvalidator.OneOf("us", "eu")},
				PlanModifiers: []planmodifier.String{defaultPagerDutyRegionModifier{}, stringplanmodifier.RequiresReplace()},
			},
			"url_hint": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The URL as the API shows it: its origin and the start of its path. Enough to " +
					"tell channels apart, not enough to post to one. Null for `email` and `pagerduty`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"key_hint": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The PagerDuty key as the API shows it: its last four characters. Null for other kinds.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"signing_secret": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "For `webhook`: the secret each delivery is signed with. The header is " +
					"`X-KiviDB-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of \"<t>.<body>\">`. The API shows " +
					"it once, in the response that creates the channel, and Terraform keeps that copy in state -- " +
					"so treat the state as a secret. Null for other kinds, and null after an import, because " +
					"there is no way to read it again; replace the channel to get a new one.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the channel was created.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *alertChannelResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
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

// ValidateConfig checks what belongs to which kind, and each kind's URL or key
// the way the API does, so a typo is a plan error rather than a 400 mid-apply.
// Whether a webhook's host is a public address is the server's to decide.
func (r *alertChannelResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, res *resource.ValidateConfigResponse) {
	var cfg alertChannelModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() || cfg.Kind.IsUnknown() || cfg.Kind.IsNull() {
		return
	}
	kind := cfg.Kind.ValueString()
	notFor := func(attr string, set bool) {
		if set {
			res.Diagnostics.AddAttributeError(path.Root(attr), "Not used by this kind of channel",
				"`"+attr+"` does not apply to a `"+kind+"` channel. Remove it.")
		}
	}
	isSet := func(v interface{ IsNull() bool }) bool { return !v.IsNull() }

	switch kind {
	case "email":
		notFor("url", isSet(cfg.URL))
		notFor("routing_key", isSet(cfg.RoutingKey))
		notFor("region", isSet(cfg.Region))
	case "pagerduty":
		notFor("url", isSet(cfg.URL))
		notFor("roles", isSet(cfg.Roles))
		if cfg.RoutingKey.IsNull() {
			res.Diagnostics.AddAttributeError(path.Root("routing_key"), "Missing routing key",
				"A `pagerduty` channel needs `routing_key`: the Integration Key of an Events API v2 integration.")
		} else if !cfg.RoutingKey.IsUnknown() && !pagerDutyRoutingKeyRegex.MatchString(strings.TrimSpace(cfg.RoutingKey.ValueString())) {
			res.Diagnostics.AddAttributeError(path.Root("routing_key"), "Not a PagerDuty integration key",
				"Use the 32-character Integration Key of an Events API v2 integration.")
		}
	default: // slack, teams, webhook
		notFor("roles", isSet(cfg.Roles))
		notFor("routing_key", isSet(cfg.RoutingKey))
		notFor("region", isSet(cfg.Region))
		if cfg.URL.IsNull() {
			res.Diagnostics.AddAttributeError(path.Root("url"), "Missing URL",
				"A `"+kind+"` channel needs `url`.")
		} else if !cfg.URL.IsUnknown() {
			if why := checkChannelURL(kind, cfg.URL.ValueString()); why != "" {
				res.Diagnostics.AddAttributeError(path.Root("url"), "URL not accepted", why)
			}
		}
	}
}

// checkChannelURL is the API's per-kind URL check, word for word where it has words.
func checkChannelURL(kind, raw string) string {
	switch kind {
	case "slack":
		if !slackWebhookPattern.MatchString(raw) {
			return "Use a Slack incoming-webhook URL (https://hooks.slack.com/services/...)."
		}
	case "teams":
		if len(raw) > 2048 || !isTeamsWebhookURL(raw) {
			return "Use a Teams Workflows webhook URL (…logic.azure.com or …api.powerplatform.com) or an Office 365 connector URL (…webhook.office.com)."
		}
	case "webhook":
		if len(raw) > 2048 {
			return "A webhook URL can be at most 2048 characters."
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "Not a URL."
		}
		if !strings.HasPrefix(raw, "https://") {
			return "Webhook URLs must use https."
		}
	}
	return ""
}

func isTeamsWebhookURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false
	}
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return teamsOfficeHost.MatchString(h) || teamsLogicHost.MatchString(h) || teamsPowerPlatformHost.MatchString(h)
}

// pagerDutyKeyHint is the API's key_hint for a key: its last four characters
// after the dots it prints.
func pagerDutyKeyHint(key string) string {
	k := strings.TrimSpace(key)
	if len(k) < 4 {
		return ""
	}
	return "••••" + k[len(k)-4:]
}

// --- plan modifiers ----------------------------------------------------------

// secretReplaceModifier replaces the channel when its URL or key changes, which
// is the only way to change either.
//
// After an import the secret is null in state, because the API never returns
// it. Writing it into configuration then is not a change to the channel, it is
// telling Terraform what the channel already holds, so it is accepted in place
// when it matches the server's hint. A value that does not match replaces the
// channel, which is the safe way to be wrong.
type secretReplaceModifier struct {
	hint   path.Path
	hintOf func(string) string
}

func (m secretReplaceModifier) Description(context.Context) string {
	return "Replaces the channel when the value changes, except to fill in a value an import could not read."
}

func (m secretReplaceModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m secretReplaceModifier) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, res *planmodifier.StringResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy
	}
	if req.PlanValue.Equal(req.StateValue) {
		return
	}
	if req.StateValue.IsNull() && !req.PlanValue.IsUnknown() && !req.PlanValue.IsNull() {
		var hint types.String
		res.Diagnostics.Append(req.State.GetAttribute(ctx, m.hint, &hint)...)
		if !hint.IsNull() && hint.ValueString() != "" && m.hintOf(req.PlanValue.ValueString()) == hint.ValueString() {
			return
		}
	}
	res.RequiresReplace = true
}

// defaultEmailRolesModifier plans owners and admins for an email channel that
// names no roles, which is what the API does, and null for other kinds.
type defaultEmailRolesModifier struct{}

func (defaultEmailRolesModifier) Description(context.Context) string {
	return "Defaults to owner and admin for an email channel."
}

func (m defaultEmailRolesModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (defaultEmailRolesModifier) PlanModifySet(ctx context.Context, req planmodifier.SetRequest, res *planmodifier.SetResponse) {
	if !req.ConfigValue.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var kind types.String
	res.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("kind"), &kind)...)
	if kind.IsUnknown() {
		return
	}
	if kind.ValueString() != "email" {
		res.PlanValue = types.SetNull(types.StringType)
		return
	}
	res.PlanValue = stringSet(defaultEmailRoles)
}

// defaultPagerDutyRegionModifier plans `us` for a PagerDuty channel that names
// no region, as the API does, and null for other kinds.
type defaultPagerDutyRegionModifier struct{}

func (defaultPagerDutyRegionModifier) Description(context.Context) string {
	return "Defaults to us for a PagerDuty channel."
}

func (m defaultPagerDutyRegionModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (defaultPagerDutyRegionModifier) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, res *planmodifier.StringResponse) {
	if !req.ConfigValue.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var kind types.String
	res.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("kind"), &kind)...)
	if kind.IsUnknown() {
		return
	}
	if kind.ValueString() != "pagerduty" {
		res.PlanValue = types.StringNull()
		return
	}
	res.PlanValue = types.StringValue("us")
}

func stringSet(vs []string) types.Set {
	elems := make([]attr.Value, 0, len(vs))
	for _, v := range vs {
		elems = append(elems, types.StringValue(v))
	}
	return types.SetValueMust(types.StringType, elems)
}

// --- CRUD --------------------------------------------------------------------

// apply copies the server's answer over the model. The URL, the key and the
// signing secret are never in a read, so they stay as they are.
func (m *alertChannelModel) apply(c *AlertChannel) {
	m.ID = types.StringValue(c.ID)
	m.Name = types.StringValue(c.Name)
	m.Kind = types.StringValue(c.Kind)
	if c.Kind == "email" {
		roles := c.Roles
		if len(roles) == 0 {
			roles = defaultEmailRoles
		}
		m.Roles = stringSet(roles)
	} else {
		m.Roles = types.SetNull(types.StringType)
	}
	if c.Kind == "pagerduty" {
		region := "us"
		if c.Region != nil && *c.Region != "" {
			region = *c.Region
		}
		m.Region = types.StringValue(region)
	} else {
		m.Region = types.StringNull()
	}
	m.URLHint = stringOrNull(c.URLHint)
	m.KeyHint = stringOrNull(c.KeyHint)
	m.CreatedAt = types.StringValue(c.CreatedAt)
	if m.SigningSecret.IsUnknown() {
		m.SigningSecret = types.StringNull()
	}
}

func (r *alertChannelResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan alertChannelModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}

	body := CreateAlertChannelRequest{Kind: plan.Kind.ValueString(), Name: plan.Name.ValueString()}
	switch body.Kind {
	case "email":
		if !plan.Roles.IsNull() && !plan.Roles.IsUnknown() {
			res.Diagnostics.Append(plan.Roles.ElementsAs(ctx, &body.Roles, false)...)
			if res.Diagnostics.HasError() {
				return
			}
		}
	case "pagerduty":
		body.RoutingKey = strings.TrimSpace(plan.RoutingKey.ValueString())
		if !plan.Region.IsNull() && !plan.Region.IsUnknown() {
			body.Region = plan.Region.ValueString()
		}
	default:
		body.URL = plan.URL.ValueString()
	}

	created, err := r.client.CreateAlertChannel(ctx, body)
	if err != nil {
		res.Diagnostics.AddError("Could not create the alert channel", explain(err))
		return
	}
	plan.SigningSecret = stringOrNull(created.SigningSecret)
	plan.apply(&created.Channel)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *alertChannelResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state alertChannelModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if state.ID.ValueString() == "" {
		res.State.RemoveResource(ctx)
		return
	}
	ch, err := r.client.GetAlertChannel(ctx, state.ID.ValueString())
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			// Deleted in the console. Drop it so the next apply recreates it.
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the alert channel", explain(err))
		return
	}
	state.apply(ch)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// Update is reached only when nothing about the channel changes: every
// configurable attribute replaces it, except a URL or key written in after an
// import to match what the channel already holds. That is recorded, not sent.
func (r *alertChannelResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan, state alertChannelModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	ch, err := r.client.GetAlertChannel(ctx, state.ID.ValueString())
	if err != nil {
		res.Diagnostics.AddError("Could not read the alert channel", explain(err))
		return
	}
	plan.SigningSecret = state.SigningSecret
	plan.apply(ch)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *alertChannelResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state alertChannelModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteAlertChannel(ctx, state.ID.ValueString()); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			if apiErr.IsNotFound() {
				return
			}
			if apiErr.Code == "channel_in_use" {
				res.Diagnostics.AddError("The alert channel is still in use", apiErr.Message+
					"\n\nAn alert still sends to this channel. If Terraform is replacing the channel, add "+
					"`lifecycle { create_before_destroy = true }` to it so the alert moves to the new channel "+
					"before this one is removed.")
				return
			}
		}
		res.Diagnostics.AddError("Could not delete the alert channel", explain(err))
	}
}

func (r *alertChannelResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, res)
}
