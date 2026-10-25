package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*creditReservationsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*creditReservationsDataSource)(nil)
)

func NewCreditReservationsDataSource() datasource.DataSource { return &creditReservationsDataSource{} }

// creditReservationsDataSource is GET /credits/reservations: the balance, how
// much of it is set aside, and every reservation, active or not.
type creditReservationsDataSource struct{ client *Client }

type rvListModel struct {
	BalanceCents    types.Int64       `tfsdk:"balance_cents"`
	ReservedCents   types.Int64       `tfsdk:"reserved_cents"`
	UnreservedCents types.Int64       `tfsdk:"unreserved_cents"`
	Reservations    []rvListItemModel `tfsdk:"reservations"`
}

type rvListItemModel struct {
	ID                types.String `tfsdk:"id"`
	InstanceID        types.String `tfsdk:"instance_id"`
	InstanceName      types.String `tfsdk:"instance_name"`
	InstanceStatus    types.String `tfsdk:"instance_status"`
	AmountCents       types.Int64  `tfsdk:"amount_cents"`
	ConsumedCents     types.Int64  `tfsdk:"consumed_cents"`
	RemainingCents    types.Int64  `tfsdk:"remaining_cents"`
	Status            types.String `tfsdk:"status"`
	DeactivatedAt     types.String `tfsdk:"deactivated_at"`
	DeactivatedReason types.String `tfsdk:"deactivated_reason"`
	CreatedAt         types.String `tfsdk:"created_at"`
}

func (d *creditReservationsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, res *datasource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_credit_reservations"
}

func (d *creditReservationsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, res *datasource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "The organization's credit balance, how much of it is set aside for particular " +
			"databases, and every credit reservation, active or not. Reading spends nothing. Needs an API " +
			"key with the owner or admin role. To make or change a reservation, use `kividb_credit_reservation`.",
		Attributes: map[string]schema.Attribute{
			"balance_cents": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "The whole credit balance, in US cents.",
			},
			"reserved_cents": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "What active reservations still hold. Only their own databases can spend it.",
			},
			"unreserved_cents": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "`balance_cents` less `reserved_cents`: what every database without a " +
					"reservation runs on. When it runs out, those databases are stopped first.",
			},
			"reservations": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Every reservation: active ones first, newest first within each.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":              schema.StringAttribute{Computed: true, MarkdownDescription: "The reservation's id."},
						"instance_id":     schema.StringAttribute{Computed: true, MarkdownDescription: "The database it is for. Null once that database has been deleted."},
						"instance_name":   schema.StringAttribute{Computed: true, MarkdownDescription: "The database's name, or the name it had."},
						"instance_status": schema.StringAttribute{Computed: true, MarkdownDescription: "The database's status. Null once it has been deleted."},
						"amount_cents":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Everything set aside on it, in cents."},
						"consumed_cents":  schema.Int64Attribute{Computed: true, MarkdownDescription: "What the database's charges have drawn from it."},
						"remaining_cents": schema.Int64Attribute{Computed: true, MarkdownDescription: "What it still holds."},
						"status":          schema.StringAttribute{Computed: true, MarkdownDescription: "`active` or `inactive`. Only an active reservation protects its database."},
						"deactivated_at":  schema.StringAttribute{Computed: true, MarkdownDescription: "When it was deactivated."},
						"deactivated_reason": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "`user`, or `instance_deleted` when KiviDB deactivated it because its database was deleted.",
						},
						"created_at": schema.StringAttribute{Computed: true, MarkdownDescription: "When it was created."},
					},
				},
			},
		},
	}
}

func (d *creditReservationsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, res *datasource.ConfigureResponse) {
	client, ok := clientFrom(req.ProviderData, &res.Diagnostics)
	if ok {
		d.client = client
	}
}

func (d *creditReservationsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, res *datasource.ReadResponse) {
	all, err := d.client.ListCreditReservations(ctx)
	if err != nil {
		res.Diagnostics.AddError("Could not read the credit reservations", explain(err))
		return
	}
	state := rvListModel{
		BalanceCents:    types.Int64Value(all.BalanceCents),
		ReservedCents:   types.Int64Value(all.ReservedCents),
		UnreservedCents: types.Int64Value(all.UnreservedCents),
		Reservations:    make([]rvListItemModel, 0, len(all.Reservations)),
	}
	for _, c := range all.Reservations {
		state.Reservations = append(state.Reservations, rvListItemModel{
			ID:                types.StringValue(c.ID),
			InstanceID:        stringOrNull(c.InstanceID),
			InstanceName:      stringOrNull(c.InstanceName),
			InstanceStatus:    stringOrNull(c.InstanceStatus),
			AmountCents:       types.Int64Value(c.AmountCents),
			ConsumedCents:     types.Int64Value(c.ConsumedCents),
			RemainingCents:    types.Int64Value(c.RemainingCents),
			Status:            types.StringValue(c.Status),
			DeactivatedAt:     stringOrNull(c.DeactivatedAt),
			DeactivatedReason: stringOrNull(c.DeactivatedReason),
			CreatedAt:         types.StringValue(c.CreatedAt),
		})
	}
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}
