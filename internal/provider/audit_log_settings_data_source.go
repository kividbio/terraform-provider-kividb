package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*auditLogSettingsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*auditLogSettingsDataSource)(nil)
)

func NewAuditLogSettingsDataSource() datasource.DataSource { return &auditLogSettingsDataSource{} }

// auditLogSettingsDataSource reads whether the organization keeps an audit log.
//
// A data source and not a resource, because the API decides that: turning the
// log on or off is accepted only from the organization's owner, signed in to
// the console -- never from an API key, whatever its role (routes/audit-log.ts,
// PUT /audit-log/settings). Whether an organization keeps a record of what its
// members do is the owner's decision as a person. A resource every apply of
// which failed with 403 would read as a control and be none; reading the
// setting is what a key can honestly do, and it is enough to make a plan fail
// on an organization that has the log off (see the example's precondition).
type auditLogSettingsDataSource struct{ client *Client }

type auditLogSettingsModel struct {
	Enabled       types.Bool   `tfsdk:"enabled"`
	EnabledAt     types.String `tfsdk:"enabled_at"`
	EnabledBy     types.String `tfsdk:"enabled_by"`
	HotDays       types.Int64  `tfsdk:"hot_days"`
	RetentionDays types.Int64  `tfsdk:"retention_days"`
}

func (d *auditLogSettingsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, res *datasource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_audit_log_settings"
}

func (d *auditLogSettingsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, res *datasource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "Whether the organization keeps an audit log, and for how long. Read with an API key " +
			"that has the owner or admin role.\n\n" +
			"Read-only. The audit log is turned on or off only by the organization's owner, signed in to the " +
			"console (Dashboard → Settings → Audit log); the API refuses the change from any API key.",
		Attributes: map[string]schema.Attribute{
			"enabled": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether actions in the organization are being recorded. Off until the owner " +
					"turns it on.",
			},
			"enabled_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When it was last turned on. Null while it is off.",
			},
			"enabled_by": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The email of the owner who turned it on. Null while it is off.",
			},
			"hot_days": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "How many days of events the console and `GET /audit-log` show. Older events " +
					"are moved to an S3 Object Lock archive and are still in the export.",
			},
			"retention_days": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "How many days an event is kept in all, archive included. Turning the log " +
					"off stops new events being recorded; it does not delete or shorten the ones already kept.",
			},
		},
	}
}

func (d *auditLogSettingsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, res *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		res.Diagnostics.AddError("Unexpected provider data", "Could not read the configured client.")
		return
	}
	d.client = client
}

func (d *auditLogSettingsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, res *datasource.ReadResponse) {
	s, err := d.client.GetAuditLogSettings(ctx)
	if err != nil {
		res.Diagnostics.AddError("Could not read the audit log settings", explain(err))
		return
	}
	state := auditLogSettingsModel{
		Enabled:       types.BoolValue(s.Enabled),
		EnabledAt:     stringOrNull(s.EnabledAt),
		EnabledBy:     stringOrNull(s.EnabledBy),
		HotDays:       types.Int64Value(s.HotDays),
		RetentionDays: types.Int64Value(s.RetentionDays),
	}
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}
