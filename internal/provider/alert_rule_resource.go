package provider

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/float64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*alertRuleResource)(nil)
	_ resource.ResourceWithConfigure      = (*alertRuleResource)(nil)
	_ resource.ResourceWithImportState    = (*alertRuleResource)(nil)
	_ resource.ResourceWithValidateConfig = (*alertRuleResource)(nil)
)

func NewAlertRuleResource() resource.Resource { return &alertRuleResource{} }

// alertRuleResource is what triggers an alert. Unlike a channel, a rule is
// replaced in place with PUT, so every attribute changes without a new id.
type alertRuleResource struct{ client *Client }

type alertRuleModel struct {
	ID              types.String  `tfsdk:"id"`
	Name            types.String  `tfsdk:"name"`
	Metric          types.String  `tfsdk:"metric"`
	InstanceID      types.String  `tfsdk:"instance_id"`
	Op              types.String  `tfsdk:"op"`
	Threshold       types.Float64 `tfsdk:"threshold"`
	ForMinutes      types.Int64   `tfsdk:"for_minutes"`
	CooldownMinutes types.Int64   `tfsdk:"cooldown_minutes"`
	ChannelIDs      types.Set     `tfsdk:"channel_ids"`
	Enabled         types.Bool    `tfsdk:"enabled"`
}

// alertMetric is one entry of the API's ALERT_METRICS
// (packages/shared/src/alerts.ts). The API publishes no endpoint listing them,
// so they are written out here and in the attribute's description.
type alertMetric struct {
	label            string
	unit             string
	organization     bool
	defaultOp        string
	defaultThreshold float64
}

var alertMetrics = map[string]alertMetric{
	"memory_pct":              {"Memory used", "%", false, "gt", 80},
	"ops_per_sec":             {"Operations per second", "ops/s", false, "gt", 10000},
	"latency_p99_ms":          {"p99 command latency", "ms", false, "gt", 5},
	"connected_clients":       {"Connected clients", "clients", false, "gt", 1000},
	"replication_lag_seconds": {"Replication lag", "s", false, "gt", 10},
	"failover":                {"Failover", "", false, "gt", 0},
	"credit_runway_days":      {"Credit runway", "days", true, "lt", 7},
}

func alertMetricNames() []string {
	names := make([]string, 0, len(alertMetrics))
	for n := range alertMetrics {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

const (
	defaultForMinutes      = 5
	defaultCooldownMinutes = 60
)

func (r *alertRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_alert_rule"
}

func (r *alertRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "An alert: a condition on one database, or on the organization's credit, held for " +
			"`for_minutes` before it fires, sent to one or more `kividb_alert_channel`s. It is sent once when it " +
			"fires, again only after `cooldown_minutes` if it is still firing, and once more when it clears.\n\n" +
			"Every change is applied in place, and any change starts the condition over: an alert that was " +
			"firing sends no resolved notice for the condition it no longer has.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Alert id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "A name for the alert, 1 to 80 characters. Each notice leads with it.",
				Validators:          alertNameValidators(),
			},
			"metric": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "What to watch, with its default condition when `op` and `threshold` are not set: " +
					"`memory_pct` (memory used, %; above 80), `ops_per_sec` (operations per second; above 10000), " +
					"`latency_p99_ms` (p99 command latency, ms; above 5), `connected_clients` (above 1000), " +
					"`replication_lag_seconds` (above 10), `failover` (a replica was promoted -- fires on the event, " +
					"so it takes no `op`, `threshold` or `for_minutes`) or `credit_runway_days` (days of credit left " +
					"at the current rate; below 7 -- about the organization, so it takes no `instance_id`).",
				Validators: []validator.String{stringvalidator.OneOf(alertMetricNames()...)},
			},
			"instance_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The database the alert is about. Required for every metric except " +
					"`credit_runway_days`, which is about the organization and must not have one. Deleting the " +
					"database deletes its alerts, and Terraform then plans to create this one again.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(uuidPattern, "must be a database id (a UUID)"),
				},
			},
			"op": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "`gt` fires when the value is above `threshold`, `lt` when it is below. " +
					"Defaults to the metric's usual direction (see `metric`). Not used by `failover`.",
				Validators:    []validator.String{stringvalidator.OneOf("gt", "lt")},
				PlanModifiers: []planmodifier.String{metricDefaultOp{}},
			},
			"threshold": schema.Float64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The value to compare against, in the metric's unit; zero or more. Defaults " +
					"to the metric's default (see `metric`). Not used by `failover`.",
				Validators:    []validator.Float64{float64validator.AtLeast(0), finiteFloat{}},
				PlanModifiers: []planmodifier.Float64{metricDefaultThreshold{}},
			},
			"for_minutes": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "How long the condition has to hold before the alert fires, 0 to 1440 " +
					"minutes. Defaults to 5. Not used by `failover`, which fires on the event.",
				Validators:    []validator.Int64{int64validator.Between(0, 1440)},
				PlanModifiers: []planmodifier.Int64{metricDefaultForMinutes{}},
			},
			"cooldown_minutes": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(defaultCooldownMinutes),
				MarkdownDescription: "How long to wait before telling the channels again about an alert that is " +
					"still firing, 5 to 10080 minutes (a week). Defaults to 60.",
				Validators: []validator.Int64{int64validator.Between(5, 10080)},
			},
			"channel_ids": schema.SetAttribute{
				ElementType: types.StringType,
				Required:    true,
				MarkdownDescription: "Where to send it: 1 to 10 `kividb_alert_channel` ids. A channel an alert " +
					"sends to cannot be deleted until the alert stops naming it.",
				Validators: []validator.Set{
					setvalidator.SizeBetween(1, 10),
					setvalidator.ValueStringsAre(stringvalidator.RegexMatches(uuidPattern, "must be an alert channel id (a UUID)")),
				},
			},
			"enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether the alert is evaluated. Defaults to true; set false to silence it " +
					"without losing its configuration.",
			},
		},
	}
}

func (r *alertRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
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

// ValidateConfig holds the rules the API applies across fields. Each is a case
// where the server would either refuse the alert or quietly store something
// other than what was written, and the second would plan a change on every
// refresh.
func (r *alertRuleResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, res *resource.ValidateConfigResponse) {
	var cfg alertRuleModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() || cfg.Metric.IsUnknown() || cfg.Metric.IsNull() {
		return
	}
	metric := cfg.Metric.ValueString()
	m, ok := alertMetrics[metric]
	if !ok {
		return // the attribute's own validator reports it
	}
	if m.organization {
		if !cfg.InstanceID.IsNull() {
			res.Diagnostics.AddAttributeError(path.Root("instance_id"), "Not used by this metric",
				"`"+metric+"` is about the organization, not a database. Remove `instance_id`.")
		}
	} else if cfg.InstanceID.IsNull() {
		res.Diagnostics.AddAttributeError(path.Root("instance_id"), "Missing database",
			m.label+" alerts are about one database; set `instance_id`.")
	}
	if metric == "failover" {
		for _, a := range []struct {
			name string
			set  bool
		}{
			{"op", !cfg.Op.IsNull()},
			{"threshold", !cfg.Threshold.IsNull()},
			{"for_minutes", !cfg.ForMinutes.IsNull()},
		} {
			if a.set {
				res.Diagnostics.AddAttributeError(path.Root(a.name), "Not used by failover alerts",
					"A failover alert fires when a replica is promoted, so it has no `"+a.name+"`. Remove it.")
			}
		}
	}
}

// --- plan modifiers: the API's per-metric defaults --------------------------

type metricDefaultOp struct{}

func (metricDefaultOp) Description(context.Context) string {
	return "Defaults to the metric's usual direction."
}
func (m metricDefaultOp) MarkdownDescription(ctx context.Context) string { return m.Description(ctx) }

func (metricDefaultOp) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, res *planmodifier.StringResponse) {
	if !req.ConfigValue.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var metric types.String
	res.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("metric"), &metric)...)
	if m, ok := alertMetrics[metric.ValueString()]; ok && !metric.IsUnknown() {
		res.PlanValue = types.StringValue(m.defaultOp)
	}
}

type metricDefaultThreshold struct{}

func (metricDefaultThreshold) Description(context.Context) string {
	return "Defaults to the metric's default threshold."
}
func (m metricDefaultThreshold) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (metricDefaultThreshold) PlanModifyFloat64(ctx context.Context, req planmodifier.Float64Request, res *planmodifier.Float64Response) {
	if !req.ConfigValue.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var metric types.String
	res.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("metric"), &metric)...)
	if m, ok := alertMetrics[metric.ValueString()]; ok && !metric.IsUnknown() {
		res.PlanValue = types.Float64Value(m.defaultThreshold)
	}
}

type metricDefaultForMinutes struct{}

func (metricDefaultForMinutes) Description(context.Context) string {
	return "Defaults to 5, or 0 for a failover alert."
}
func (m metricDefaultForMinutes) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (metricDefaultForMinutes) PlanModifyInt64(ctx context.Context, req planmodifier.Int64Request, res *planmodifier.Int64Response) {
	if !req.ConfigValue.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var metric types.String
	res.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("metric"), &metric)...)
	if metric.IsUnknown() {
		return
	}
	if metric.ValueString() == "failover" {
		res.PlanValue = types.Int64Value(0)
	} else {
		res.PlanValue = types.Int64Value(defaultForMinutes)
	}
}

// finiteFloat mirrors zod's `.finite()`. Terraform numbers cannot be NaN, but
// they can be large enough to overflow a float64 into infinity.
type finiteFloat struct{}

func (finiteFloat) Description(context.Context) string { return "must be a finite number" }
func (v finiteFloat) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (finiteFloat) ValidateFloat64(_ context.Context, req validator.Float64Request, res *validator.Float64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if f := req.ConfigValue.ValueFloat64(); math.IsInf(f, 0) || math.IsNaN(f) {
		res.Diagnostics.AddAttributeError(req.Path, "Not a finite number", "The threshold must be a finite number.")
	}
}

// --- CRUD --------------------------------------------------------------------

func (m *alertRuleModel) request(ctx context.Context) (AlertRuleRequest, error) {
	req := AlertRuleRequest{
		Name:            m.Name.ValueString(),
		Metric:          m.Metric.ValueString(),
		ForMinutes:      m.ForMinutes.ValueInt64(),
		CooldownMinutes: m.CooldownMinutes.ValueInt64(),
		Enabled:         m.Enabled.ValueBool(),
	}
	if m.CooldownMinutes.IsNull() || m.CooldownMinutes.IsUnknown() {
		req.CooldownMinutes = defaultCooldownMinutes
	}
	if m.Enabled.IsNull() || m.Enabled.IsUnknown() {
		req.Enabled = true
	}
	if m.ForMinutes.IsNull() || m.ForMinutes.IsUnknown() {
		req.ForMinutes = defaultForMinutes
	}
	if !m.InstanceID.IsNull() && !m.InstanceID.IsUnknown() {
		// Lower case, because the API matches ids as text against Postgres's
		// spelling of a UUID: an id written in upper case passes its schema and
		// then is "no such database".
		id := strings.ToLower(m.InstanceID.ValueString())
		req.InstanceID = &id
	}
	if req.Metric != "failover" {
		if !m.Op.IsNull() && !m.Op.IsUnknown() {
			op := m.Op.ValueString()
			req.Op = &op
		}
		if !m.Threshold.IsNull() && !m.Threshold.IsUnknown() {
			t := m.Threshold.ValueFloat64()
			req.Threshold = &t
		}
	}
	if d := m.ChannelIDs.ElementsAs(ctx, &req.ChannelIDs, false); d.HasError() {
		return req, errors.New("could not read channel_ids")
	}
	for i := range req.ChannelIDs {
		req.ChannelIDs[i] = strings.ToLower(req.ChannelIDs[i])
	}
	sort.Strings(req.ChannelIDs)
	return req, nil
}

// apply copies the server's answer over the model. Ids keep the spelling they
// were written in when the server's differs only in case: the API answers in
// lower case, and an id written in upper case is the same id.
func (m *alertRuleModel) apply(r *AlertRule) {
	m.ID = types.StringValue(r.ID)
	m.Name = types.StringValue(r.Name)
	m.Metric = types.StringValue(r.Metric)
	if r.InstanceID == nil || *r.InstanceID == "" {
		m.InstanceID = types.StringNull()
	} else if m.InstanceID.IsNull() || m.InstanceID.IsUnknown() || !strings.EqualFold(m.InstanceID.ValueString(), *r.InstanceID) {
		m.InstanceID = types.StringValue(*r.InstanceID)
	}
	m.Op = types.StringValue(r.Op)
	if r.Threshold.Value != nil {
		m.Threshold = types.Float64Value(*r.Threshold.Value)
	} else {
		m.Threshold = types.Float64Value(0)
	}
	m.ForMinutes = types.Int64Value(r.ForMinutes)
	m.CooldownMinutes = types.Int64Value(r.CooldownMinutes)
	m.Enabled = types.BoolValue(r.Enabled)

	prior := map[string]string{}
	if !m.ChannelIDs.IsNull() && !m.ChannelIDs.IsUnknown() {
		for _, e := range m.ChannelIDs.Elements() {
			if s, ok := e.(types.String); ok {
				prior[strings.ToLower(s.ValueString())] = s.ValueString()
			}
		}
	}
	elems := make([]attr.Value, 0, len(r.ChannelIDs))
	for _, id := range r.ChannelIDs {
		if spelled, ok := prior[strings.ToLower(id)]; ok {
			id = spelled
		}
		elems = append(elems, types.StringValue(id))
	}
	m.ChannelIDs = types.SetValueMust(types.StringType, elems)
}

func (r *alertRuleResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan alertRuleModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	body, err := plan.request(ctx)
	if err != nil {
		res.Diagnostics.AddError("Could not create the alert", err.Error())
		return
	}
	rule, err := r.client.CreateAlertRule(ctx, body)
	if err != nil {
		res.Diagnostics.AddError("Could not create the alert", explain(err))
		return
	}
	plan.apply(rule)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *alertRuleResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state alertRuleModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if state.ID.ValueString() == "" {
		res.State.RemoveResource(ctx)
		return
	}
	rule, err := r.client.GetAlertRule(ctx, state.ID.ValueString())
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			// Deleted in the console, or with the database it watched.
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the alert", explain(err))
		return
	}
	state.apply(rule)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *alertRuleResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan, state alertRuleModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	body, err := plan.request(ctx)
	if err != nil {
		res.Diagnostics.AddError("Could not change the alert", err.Error())
		return
	}
	rule, err := r.client.ReplaceAlertRule(ctx, state.ID.ValueString(), body)
	if err != nil {
		res.Diagnostics.AddError("Could not change the alert", explain(err))
		return
	}
	plan.ID = state.ID
	plan.apply(rule)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *alertRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state alertRuleModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteAlertRule(ctx, state.ID.ValueString()); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			return
		}
		res.Diagnostics.AddError("Could not delete the alert", explain(err))
	}
}

func (r *alertRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, res)
}
