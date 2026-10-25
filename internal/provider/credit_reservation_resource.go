package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                     = (*creditReservationResource)(nil)
	_ resource.ResourceWithConfigure        = (*creditReservationResource)(nil)
	_ resource.ResourceWithImportState      = (*creditReservationResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*creditReservationResource)(nil)
	_ resource.ResourceWithConfigValidators = (*creditReservationResource)(nil)
)

func NewCreditReservationResource() resource.Resource { return &creditReservationResource{} }

// creditReservationResource is part of the organization's credit set aside for
// one database (apps/api/src/routes/credit-reservations.ts,
// packages/billing/src/reservations.ts).
//
// Its plan is where the money is decided, so the plan asks the API's preview
// what the apply would meet: a refusal fails `terraform plan`, and a change
// that leaves the databases without a reservation closer to being stopped is
// a warning naming them.
type creditReservationResource struct{ client *Client }

type rvModel struct {
	ID          types.String `tfsdk:"id"`
	InstanceID  types.String `tfsdk:"instance_id"`
	Months      types.Int64  `tfsdk:"months"`
	AmountCents types.Int64  `tfsdk:"amount_cents"`
	Active      types.Bool   `tfsdk:"active"`

	ReservedCents     types.Int64  `tfsdk:"reserved_cents"`
	ConsumedCents     types.Int64  `tfsdk:"consumed_cents"`
	RemainingCents    types.Int64  `tfsdk:"remaining_cents"`
	Status            types.String `tfsdk:"status"`
	InstanceName      types.String `tfsdk:"instance_name"`
	DeactivatedReason types.String `tfsdk:"deactivated_reason"`
	DeactivatedAt     types.String `tfsdk:"deactivated_at"`
	CreatedAt         types.String `tfsdk:"created_at"`
}

// rvPrivateKey holds the reservation's id in private state. Terraform plans a
// replacement twice, the second time as a create with no prior state; the id
// is how that second plan knows which reservation the apply will deactivate
// first, so the preview counts what it still holds as unreserved.
const rvPrivateKey = "credit_reservation_id"

func (r *creditReservationResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_credit_reservation"
}

func (r *creditReservationResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "Part of the organization's credit set aside for one database, so other databases " +
			"cannot spend it.\n\n" +
			"**What it does to money.** Nothing is spent and nothing is added: the amount moves out of the " +
			"unreserved part of the balance you already have. The reserved database's own charges draw " +
			"its reservation down. Every other database sees the balance minus what is still reserved, " +
			"and when that unreserved part runs out, the databases without a reservation are stopped " +
			"first, so the reserved one keeps running on what was set aside for it.\n\n" +
			"The API refuses a reservation larger than the unreserved balance, and one that would leave " +
			"the running databases without a reservation less than a day of credit. `terraform plan` asks " +
			"the API the same question first: a refusal fails the plan, and a change that leaves other " +
			"databases closer to being stopped is a warning naming them and how many days of running the " +
			"unreserved balance leaves them.\n\n" +
			"Needs an API key with the owner or admin role.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The reservation's id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The database the credit is set aside for. A database has at most one " +
					"active reservation. Changing this replaces the reservation: the old one is " +
					"deactivated, so what it still holds returns to the unreserved balance, and deleted, " +
					"then the new one is created.",
				PlanModifiers: []planmodifier.String{replaceUnlessSameID()},
				Validators: []validator.String{
					stringvalidator.RegexMatches(uuidPattern, "must be a database id (a UUID)"),
				},
			},
			"months": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "How much to set aside, in months of the database's price, 1 to 12. Set this " +
					"or `amount_cents`. The API prices the months when they are set aside, from what the " +
					"database costs then; a later resize does not re-price them, and `reserved_cents` " +
					"keeps the amount actually set aside. Raising `months` sets aside the extra months at " +
					"the price of the day. Lowering it replaces the reservation (see `amount_cents`).",
				Validators: []validator.Int64{int64validator.Between(1, 12)},
			},
			"amount_cents": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "How much to set aside, in US cents: 100 ($1.00) to 100000000. Set this or " +
					"`months`. This is the reservation's total, and is read back, so credit added in the " +
					"console shows as a change. A reservation can only grow: raising this sets aside the " +
					"difference (at least 100 cents) in place. Lowering it replaces the reservation: the " +
					"old one is deactivated, returning only what it still holds (`remaining_cents`) to the " +
					"unreserved balance, and a new one sets aside the new amount in full. When less than " +
					"the new amount is left on the old one, that sets aside more, not less; the plan says " +
					"so. To stop protecting the database without a new reservation, set `active = false`.",
				Validators: []validator.Int64{int64validator.Between(100, 100_000_000)},
			},
			"active": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether the reservation protects its database. Defaults to true. False " +
					"deactivates it: what is left returns to the unreserved balance and the database is " +
					"no longer protected. True again re-activates it with what it still holds, which the " +
					"unreserved balance must cover. KiviDB deactivates a reservation itself when its " +
					"database is deleted; a refresh warns, and it cannot be activated again.",
			},
			"reserved_cents": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Everything set aside on this reservation, in cents, including what has since been drawn.",
			},
			"consumed_cents": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "What the database's charges have drawn from it. Metering moves it every " +
					"hour, so it changes between refreshes without ever being a change to apply.",
			},
			"remaining_cents": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "`reserved_cents` less `consumed_cents`: what is still set aside. Moves every hour, like `consumed_cents`.",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`active` or `inactive`.",
			},
			"instance_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The database's name, or the name it had once it is deleted.",
			},
			"deactivated_reason": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Why an inactive reservation is inactive: `user`, or `instance_deleted` when KiviDB deactivated it because its database was deleted. Null while active.",
			},
			"deactivated_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When it was deactivated. Null while active.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the reservation was created.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *creditReservationResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("months"), path.MatchRoot("amount_cents")),
	}
}

func (r *creditReservationResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
	client, ok := clientFrom(req.ProviderData, &res.Diagnostics)
	if ok {
		r.client = client
	}
}

// --- what a plan changes ---------------------------------------------------

// rvChange is the difference between the amount in state and the one planned.
type rvChange struct {
	unknown  bool
	decrease bool
	attr     string             // the attribute that lowers, for RequiresReplace
	add      *ReservationAmount // what to add in place, nil when nothing
	adopt    bool               // months set where state has none: recorded, not re-priced
}

func rvChangeOf(plan, state rvModel) rvChange {
	switch {
	case plan.AmountCents.IsUnknown() || plan.Months.IsUnknown():
		return rvChange{unknown: true}
	case !plan.AmountCents.IsNull():
		if state.ReservedCents.IsNull() || state.ReservedCents.IsUnknown() {
			return rvChange{unknown: true}
		}
		d := plan.AmountCents.ValueInt64() - state.ReservedCents.ValueInt64()
		switch {
		case d < 0:
			return rvChange{decrease: true, attr: "amount_cents"}
		case d > 0:
			return rvChange{add: &ReservationAmount{AmountCents: &d}}
		}
	case !plan.Months.IsNull():
		if state.Months.IsNull() {
			// Imported, or switched from amount_cents: there is nothing to
			// compare months with, and re-pricing would be a guess.
			return rvChange{adopt: true}
		}
		d := plan.Months.ValueInt64() - state.Months.ValueInt64()
		switch {
		case d < 0:
			return rvChange{decrease: true, attr: "months"}
		case d > 0:
			return rvChange{add: &ReservationAmount{Months: &d}}
		}
	}
	return rvChange{}
}

func (m rvModel) amount() ReservationAmount {
	if !m.Months.IsNull() {
		v := m.Months.ValueInt64()
		return ReservationAmount{Months: &v}
	}
	v := m.AmountCents.ValueInt64()
	return ReservationAmount{AmountCents: &v}
}

func rvUSD(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}

func rvDescribeAmount(a ReservationAmount) string {
	if a.Months != nil {
		if *a.Months == 1 {
			return "1 month of the database's current price"
		}
		return fmt.Sprintf("%d months of the database's current price", *a.Months)
	}
	return rvUSD(*a.AmountCents)
}

func (m rvModel) label() string {
	if n := m.InstanceName.ValueString(); n != "" && !m.InstanceName.IsUnknown() {
		return fmt.Sprintf("%q", n)
	}
	return "database " + m.InstanceID.ValueString()
}

func (r *creditReservationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, res *resource.ModifyPlanResponse) {
	if r.client == nil {
		return
	}
	// Destroy: never blocked, only explained.
	if req.Plan.Raw.IsNull() {
		var state rvModel
		res.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if res.Diagnostics.HasError() || !state.Active.ValueBool() {
			return
		}
		res.Diagnostics.AddWarning("Destroying a credit reservation",
			fmt.Sprintf("Destroying it deactivates it and deletes it. The %s it still holds returns to the "+
				"unreserved balance, and %s is no longer protected: when the unreserved balance runs out, "+
				"it is stopped with the other databases without a reservation.",
				rvUSD(state.RemainingCents.ValueInt64()), state.label()))
		return
	}

	var plan rvModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}

	if req.State.Raw.IsNull() {
		r.modifyCreatePlan(ctx, res, plan, rvReleaseID(ctx, req))
		return
	}

	var state rvModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	// A different database is a replacement; Terraform plans its create half
	// separately, and that is where the preview runs.
	// Terraform keeps only the errors of that second plan, not its warnings,
	// so the databases a replacement leaves at risk are named here.
	if plan.InstanceID.IsUnknown() || !strings.EqualFold(plan.InstanceID.ValueString(), state.InstanceID.ValueString()) {
		r.previewReplacement(ctx, res, plan, state)
		return
	}

	ch := rvChangeOf(plan, state)
	if ch.decrease {
		res.RequiresReplace = append(res.RequiresReplace, path.Root(ch.attr))
		newAmount := plan.amount()
		more := ""
		if newAmount.AmountCents != nil && *newAmount.AmountCents > state.RemainingCents.ValueInt64() {
			more = fmt.Sprintf(" Only %s is left on the old one, so this sets aside %s more than is reserved now, not less.",
				rvUSD(state.RemainingCents.ValueInt64()), rvUSD(*newAmount.AmountCents-state.RemainingCents.ValueInt64()))
		}
		res.Diagnostics.AddAttributeWarning(path.Root(ch.attr), "Lowering a credit reservation replaces it",
			fmt.Sprintf("A reservation can only grow, so this plan replaces the one for %s: the old reservation "+
				"is deactivated and deleted, returning the %s it still holds to the unreserved balance, and a "+
				"new one sets aside %s in full. Credit already drawn from the old one is not given back.%s "+
				"To stop protecting the database without a new reservation, set active = false instead.",
				state.label(), rvUSD(state.RemainingCents.ValueInt64()), rvDescribeAmount(newAmount), more))
		r.previewReplacement(ctx, res, plan, state)
		return
	}

	// What does not move keeps its value, so an unrelated change does not show
	// every computed attribute as unknown. consumed_cents and remaining_cents
	// stay unknown: metering moves them between plan and apply.
	if !ch.unknown && ch.add == nil {
		res.Diagnostics.Append(res.Plan.SetAttribute(ctx, path.Root("reserved_cents"), state.ReservedCents)...)
	}
	if !plan.Active.IsUnknown() {
		status, reason, at := types.StringValue("active"), types.StringNull(), types.StringNull()
		if !plan.Active.ValueBool() {
			status = types.StringValue("inactive")
			reason, at = state.DeactivatedReason, state.DeactivatedAt
			if state.Active.ValueBool() {
				reason, at = types.StringValue("user"), types.StringUnknown()
			}
		}
		res.Diagnostics.Append(res.Plan.SetAttribute(ctx, path.Root("status"), status)...)
		res.Diagnostics.Append(res.Plan.SetAttribute(ctx, path.Root("deactivated_reason"), reason)...)
		res.Diagnostics.Append(res.Plan.SetAttribute(ctx, path.Root("deactivated_at"), at)...)
	}
	if ch.unknown || plan.Active.IsUnknown() {
		return
	}
	wasActive, wantActive := state.Active.ValueBool(), plan.Active.ValueBool()

	if ch.add != nil && !wantActive {
		res.Diagnostics.AddAttributeError(path.Root("active"), "An inactive reservation cannot grow",
			"The API adds credit only to an active reservation. Set active = true to add to it, or leave "+
				"the amount as it is.")
		return
	}
	if ch.add != nil && ch.add.AmountCents != nil && *ch.add.AmountCents < 100 {
		res.Diagnostics.AddAttributeError(path.Root("amount_cents"), "Too small an increase",
			fmt.Sprintf("Raising a reservation adds the difference, and the API adds at least 100 cents ($1.00) at a "+
				"time; this adds %d. Raise amount_cents to at least %d.",
				*ch.add.AmountCents, state.ReservedCents.ValueInt64()+100))
		return
	}

	switch {
	case wasActive && !wantActive:
		res.Diagnostics.AddAttributeWarning(path.Root("active"), "Deactivating a credit reservation",
			fmt.Sprintf("%s is no longer protected: the %s left on its reservation returns to the unreserved "+
				"balance, and when the unreserved balance runs out it is stopped with the other databases "+
				"without a reservation.", capitalize(state.label()), rvUSD(state.RemainingCents.ValueInt64())))
	case !wasActive && wantActive:
		if state.DeactivatedReason.ValueString() == "instance_deleted" {
			res.Diagnostics.AddAttributeError(path.Root("active"), "This reservation cannot be activated again",
				"KiviDB deactivated it because its database was deleted. Remove the resource from the "+
					"configuration (destroying it deletes the record), or point a new reservation at another database.")
			return
		}
		amount := state.RemainingCents.ValueInt64()
		if ch.add != nil && ch.add.AmountCents != nil {
			amount += *ch.add.AmountCents
		}
		if amount < 100 {
			// The preview takes at least 100 cents, and so little moves almost nothing.
			return
		}
		r.preview(ctx, res, ReservationPreviewRequest{
			InstanceID:        strings.ToLower(plan.InstanceID.ValueString()),
			ReservationAmount: ReservationAmount{AmountCents: &amount},
		}, "Activating this reservation")
	case wantActive && ch.add != nil:
		r.preview(ctx, res, ReservationPreviewRequest{
			InstanceID:         strings.ToLower(plan.InstanceID.ValueString()),
			ReservationAmount:  *ch.add,
			AddToReservationID: state.ID.ValueString(),
		}, "Adding to this reservation")
	}
}

// rvReleaseID is the reservation a create replaces, from private state. Only
// the create half of a replacement has one.
func rvReleaseID(ctx context.Context, req resource.ModifyPlanRequest) string {
	if req.Private == nil {
		return ""
	}
	raw, d := req.Private.GetKey(ctx, rvPrivateKey)
	if d.HasError() || len(raw) == 0 {
		return ""
	}
	var old string
	if json.Unmarshal(raw, &old) != nil {
		return ""
	}
	return old
}

func (r *creditReservationResource) modifyCreatePlan(ctx context.Context, res *resource.ModifyPlanResponse, plan rvModel, releaseID string) {
	if plan.InstanceID.IsUnknown() || plan.Months.IsUnknown() || plan.AmountCents.IsUnknown() ||
		(plan.Months.IsNull() && plan.AmountCents.IsNull()) {
		return
	}
	preq := ReservationPreviewRequest{
		InstanceID:        strings.ToLower(plan.InstanceID.ValueString()),
		ReservationAmount: plan.amount(),
	}
	what := "This reservation"
	// The create half of a replacement: the apply deactivates the old
	// reservation first, so what it still holds counts as unreserved.
	if releaseID != "" {
		preq.ReleaseReservationID = releaseID
		what = "The replacement reservation"
	}
	r.preview(ctx, res, preq, what)
}

// previewReplacement warns, from the first of a replacement's two plans, which
// databases the new reservation leaves at risk. A refusal is left to the
// second plan, the create half, whose errors Terraform does show; reporting
// it here as well would say it twice.
func (r *creditReservationResource) previewReplacement(ctx context.Context, res *resource.ModifyPlanResponse, plan, state rvModel) {
	if plan.InstanceID.IsUnknown() || plan.Months.IsUnknown() || plan.AmountCents.IsUnknown() ||
		(plan.Months.IsNull() && plan.AmountCents.IsNull()) {
		return
	}
	p, err := r.client.PreviewCreditReservation(ctx, ReservationPreviewRequest{
		InstanceID:           strings.ToLower(plan.InstanceID.ValueString()),
		ReservationAmount:    plan.amount(),
		ReleaseReservationID: state.ID.ValueString(),
	})
	if err != nil || !p.Allowed {
		return
	}
	if w := rvAtRiskWarning(p); w != "" {
		res.Diagnostics.AddWarning("Databases without a reservation are left closer to being stopped",
			fmt.Sprintf("The replacement reservation sets aside %s. %s", rvUSD(p.AmountCents), w))
	}
}

// preview asks the API what the apply would meet. A refusal is a plan error in
// the API's words; databases left closer to being stopped are a warning.
func (r *creditReservationResource) preview(ctx context.Context, res *resource.ModifyPlanResponse, preq ReservationPreviewRequest, what string) {
	p, err := r.client.PreviewCreditReservation(ctx, preq)
	if err != nil {
		res.Diagnostics.AddError("Could not check the reservation against the balance", explain(err))
		return
	}
	if !p.Allowed {
		msg := "The API would refuse it."
		if p.Refusal != nil && p.Refusal.Message != "" {
			msg = p.Refusal.Message
		}
		res.Diagnostics.AddError(what+" would be refused", msg)
		return
	}
	if w := rvAtRiskWarning(p); w != "" {
		res.Diagnostics.AddWarning("Databases without a reservation are left closer to being stopped",
			fmt.Sprintf("%s sets aside %s. %s", what, rvUSD(p.AmountCents), w))
	}
}

func rvAtRiskWarning(p *ReservationPreview) string {
	if len(p.AtRisk) == 0 {
		return ""
	}
	names := make([]string, 0, len(p.AtRisk))
	for _, a := range p.AtRisk {
		if a.Name != nil && *a.Name != "" {
			names = append(names, *a.Name)
		} else {
			names = append(names, a.InstanceID)
		}
	}
	noun := "databases"
	if len(names) == 1 {
		noun = "database"
	}
	s := fmt.Sprintf("After this, %s stays unreserved for %d %s without a reservation (%s)",
		rvUSD(p.UnreservedAfterCents), len(names), noun, strings.Join(names, ", "))
	if p.UnreservedRunwayDaysAfter != nil {
		s += fmt.Sprintf(": about %s of running before they are stopped.", rvDays(*p.UnreservedRunwayDaysAfter))
	} else {
		s += "."
	}
	before := p.UnreservedRunwayDaysBefore
	if before != nil && (p.UnreservedRunwayDaysAfter == nil || *before != *p.UnreservedRunwayDaysAfter) {
		s += fmt.Sprintf(" Before this change: %s unreserved, about %s.", rvUSD(p.UnreservedBeforeCents), rvDays(*before))
	}
	return s
}

func rvDays(d float64) string {
	if d == 1 {
		return "1 day"
	}
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", d), "0"), ".") + " days"
}

// --- applying it -------------------------------------------------------------

// apply copies the server's view over the model. months is the
// configuration's and is never read back: the API keeps only cents.
func (m *rvModel) apply(c *CreditReservation) {
	m.ID = types.StringValue(c.ID)
	if c.InstanceID != nil && (m.InstanceID.IsNull() || m.InstanceID.IsUnknown() ||
		!strings.EqualFold(m.InstanceID.ValueString(), *c.InstanceID)) {
		m.InstanceID = types.StringValue(*c.InstanceID)
	}
	// Once its database is deleted the API reports no instance; the id in
	// state stays, or the reservation would plan a replacement.
	if m.Months.IsNull() || m.Months.IsUnknown() {
		m.Months = types.Int64Null()
		m.AmountCents = types.Int64Value(c.AmountCents)
	}
	m.Active = types.BoolValue(c.Status == "active")
	m.ReservedCents = types.Int64Value(c.AmountCents)
	m.ConsumedCents = types.Int64Value(c.ConsumedCents)
	m.RemainingCents = types.Int64Value(c.RemainingCents)
	m.Status = types.StringValue(c.Status)
	m.InstanceName = stringOrNull(c.InstanceName)
	m.DeactivatedReason = stringOrNull(c.DeactivatedReason)
	m.DeactivatedAt = stringOrNull(c.DeactivatedAt)
	m.CreatedAt = types.StringValue(c.CreatedAt)
}

type rvPrivateSetter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

func rvRemember(ctx context.Context, priv rvPrivateSetter, id string, diags *diag.Diagnostics) {
	if priv == nil || id == "" {
		return
	}
	raw, _ := json.Marshal(strings.ToLower(id))
	diags.Append(priv.SetKey(ctx, rvPrivateKey, raw)...)
}

func (r *creditReservationResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan rvModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	c, err := r.client.CreateCreditReservation(ctx, strings.ToLower(plan.InstanceID.ValueString()), plan.amount())
	if err != nil {
		res.Diagnostics.AddError("Could not reserve the credit", explain(err))
		return
	}
	if !plan.Active.ValueBool() {
		d, derr := r.client.DeactivateCreditReservation(ctx, c.ID)
		if derr != nil {
			res.Diagnostics.AddError("The reservation was created but could not be deactivated",
				"It is active and protects its database. "+explain(derr))
		} else {
			c = d
		}
	}
	plan.apply(c)
	if res.Private != nil {
		rvRemember(ctx, res.Private, c.ID, &res.Diagnostics)
	}
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *creditReservationResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state rvModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	c, err := r.client.GetCreditReservation(ctx, state.ID.ValueString())
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the credit reservation", explain(err))
		return
	}
	wasActive := state.Active.ValueBool()
	state.apply(c)
	rvRefreshWarnings(&res.Diagnostics, state, wasActive)
	if res.Private != nil {
		rvRemember(ctx, res.Private, c.ID, &res.Diagnostics)
	}
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// rvRefreshWarnings says what KiviDB did to a reservation on its own.
func rvRefreshWarnings(diags *diag.Diagnostics, m rvModel, wasActive bool) {
	reason := m.DeactivatedReason.ValueString()
	if m.Status.ValueString() == "inactive" && reason != "" && reason != "user" {
		why := "KiviDB deactivated it (" + reason + ")"
		if reason == "instance_deleted" {
			why = "KiviDB deactivated it because its database was deleted"
		}
		diags.AddWarning("Credit reservation deactivated by KiviDB",
			fmt.Sprintf("The reservation %s for %s is inactive: %s. What it still held (%s) is back in the "+
				"unreserved balance. It cannot be activated again; remove it from the configuration to delete "+
				"the record.", m.ID.ValueString(), m.label(), why, rvUSD(m.RemainingCents.ValueInt64())))
		return
	}
	if wasActive && m.Status.ValueString() == "active" && m.RemainingCents.ValueInt64() == 0 {
		diags.AddWarning("Credit reservation used up",
			fmt.Sprintf("%s has drawn all %s set aside for it, so the reservation no longer protects it. "+
				"Raise months or amount_cents to set more aside.", capitalize(m.label()), rvUSD(m.ReservedCents.ValueInt64())))
	}
}

func (r *creditReservationResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan, state rvModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	ch := rvChangeOf(plan, state)
	if ch.decrease {
		res.Diagnostics.AddError("Lowering a reservation needs a new plan",
			"The amount was not known when this was planned, and it turned out lower than what is reserved. "+
				"A reservation can only grow; run terraform plan again, which plans a replacement.")
		res.Diagnostics.Append(res.State.Set(ctx, &state)...)
		return
	}

	var current *CreditReservation
	fail := func(summary string, err error) {
		res.Diagnostics.AddError(summary, explain(err))
		// What the server last said, so a step that failed is planned again.
		saved := state
		if current != nil {
			saved.apply(current)
		}
		res.Diagnostics.Append(res.State.Set(ctx, &saved)...)
	}

	wasActive, wantActive := state.Active.ValueBool(), plan.Active.ValueBool()
	if wasActive && !wantActive {
		c, err := r.client.DeactivateCreditReservation(ctx, id)
		if err != nil {
			fail("Could not deactivate the reservation", err)
			return
		}
		current = c
	}
	if !wasActive && wantActive {
		c, err := r.client.ActivateCreditReservation(ctx, id)
		if err != nil {
			fail("Could not activate the reservation", err)
			return
		}
		current = c
	}
	if ch.add != nil {
		c, err := r.client.AddToCreditReservation(ctx, id, *ch.add)
		if err != nil {
			fail("Could not add to the reservation", err)
			return
		}
		current = c
	}
	if current == nil {
		c, err := r.client.GetCreditReservation(ctx, id)
		if err != nil {
			res.Diagnostics.AddError("Could not read the credit reservation", explain(err))
			return
		}
		current = c
	}
	plan.apply(current)
	if res.Private != nil {
		rvRemember(ctx, res.Private, current.ID, &res.Diagnostics)
	}
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

// Delete deactivates, then deletes: the API deletes only an inactive
// reservation. What it still holds returns to the unreserved balance.
func (r *creditReservationResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state rvModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	var apiErr *APIError
	if _, err := r.client.DeactivateCreditReservation(ctx, id); err != nil {
		// 404 here means "no active reservation with that id": already
		// inactive, or gone. The delete below tells them apart.
		if !errors.As(err, &apiErr) || !apiErr.IsNotFound() {
			res.Diagnostics.AddError("Could not deactivate the reservation", explain(err))
			return
		}
	}
	if err := r.client.DeleteCreditReservation(ctx, id); err != nil {
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			return
		}
		res.Diagnostics.AddError("Could not delete the reservation", explain(err))
	}
}

func (r *creditReservationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	c, err := r.client.GetCreditReservation(ctx, req.ID)
	if err != nil {
		res.Diagnostics.AddError("Could not import that credit reservation", explain(err))
		return
	}
	// Imported in cents: the API keeps no months, and turning cents back into
	// months would be a guess at a price that may have changed since.
	state := rvModel{Months: types.Int64Null(), AmountCents: types.Int64Null(), InstanceID: types.StringNull()}
	state.apply(c)
	if res.Private != nil {
		rvRemember(ctx, res.Private, c.ID, &res.Diagnostics)
	}
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}
