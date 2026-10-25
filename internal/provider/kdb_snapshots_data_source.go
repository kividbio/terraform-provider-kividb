package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*kdbSnapshotsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*kdbSnapshotsDataSource)(nil)
)

func NewKdbSnapshotsDataSource() datasource.DataSource { return &kdbSnapshotsDataSource{} }

// kdbSnapshotsDataSource lists KDB exports -- the organization's, or one
// database's -- so one can be named in kividb_instance.restore_from_kdb_snapshot_id
// without copying an id out of the console.
type kdbSnapshotsDataSource struct{ client *Client }

type kdbSnapshotsModel struct {
	InstanceID types.String           `tfsdk:"instance_id"`
	Status     types.String           `tfsdk:"status"`
	Snapshots  []kdbSnapshotItemModel `tfsdk:"snapshots"`
}

type kdbSnapshotItemModel struct {
	ID               types.String `tfsdk:"id"`
	InstanceID       types.String `tfsdk:"instance_id"`
	InstanceName     types.String `tfsdk:"instance_name"`
	Label            types.String `tfsdk:"label"`
	Status           types.String `tfsdk:"status"`
	SizeBytes        types.Int64  `tfsdk:"size_bytes"`
	TriggeredBy      types.String `tfsdk:"triggered_by"`
	Cloud            types.String `tfsdk:"cloud"`
	Region           types.String `tfsdk:"region"`
	CreatedAt        types.String `tfsdk:"created_at"`
	CompletedAt      types.String `tfsdk:"completed_at"`
	ExpiresAt        types.String `tfsdk:"expires_at"`
	CloudAccountID   types.String `tfsdk:"cloud_account_id"`
	CloudAccountName types.String `tfsdk:"cloud_account_name"`
	DeliveryStatus   types.String `tfsdk:"delivery_status"`
	DeliveredURI     types.String `tfsdk:"delivered_uri"`
}

func (d *kdbSnapshotsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, res *datasource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_kdb_snapshots"
}

func (d *kdbSnapshotsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, res *datasource.SchemaResponse) {
	c := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	res.Schema = schema.Schema{
		MarkdownDescription: "KDB exports of your databases, newest first: those taken on demand " +
			"(`kividb_kdb_snapshot`, the console, an assistant), on a schedule, and uploaded. " +
			"Use one's `id` as `kividb_instance.restore_from_kdb_snapshot_id` to make a new database from it.",
		Attributes: map[string]schema.Attribute{
			"instance_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "List only this database's exports. Unset, every export in the organization is " +
					"listed, including those of databases since deleted.",
			},
			"status": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "List only exports with this status: `creating`, `ready`, `failed` or `moving`. " +
					"Only a `ready` export can be restored from.",
				Validators: []validator.String{stringvalidator.OneOf("creating", "ready", "failed", "moving")},
			},
			"snapshots": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The exports, newest first.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":            c("Snapshot id."),
						"instance_id":   c("The database it was exported from. Null once that database is deleted."),
						"instance_name": c("Name the database had when it was exported."),
						"label":         c("The export's name."),
						"status":        c("`creating`, `ready`, `failed` or `moving`."),
						"size_bytes":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Size of the file, once written."},
						"triggered_by":  c("`manual`, `schedule` or `free_tier_expiry`."),
						"cloud":         c("Cloud the file is stored in."),
						"region":        c("Region the file is stored in. A database made from it must be in this region."),
						"created_at":    c("When the export was requested."),
						"completed_at":  c("When the file was written."),
						"expires_at":    c("When KiviDB deletes it on its own. Null when it is kept until deleted."),
						"cloud_account_id": c("The cloud account it is stored in; null for KiviDB's cloud. A database " +
							"made from it is placed in this account."),
						"cloud_account_name": c("The name of that cloud account."),
						"delivery_status":    c("Copy into your own bucket: `pending`, `delivered` or `failed`; null when none."),
						"delivered_uri":      c("Where the copy was written in your bucket, once delivered."),
					},
				},
			},
		},
	}
}

func (d *kdbSnapshotsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, res *datasource.ConfigureResponse) {
	client, ok := clientFrom(req.ProviderData, &res.Diagnostics)
	if ok {
		d.client = client
	}
}

func (d *kdbSnapshotsDataSource) Read(ctx context.Context, req datasource.ReadRequest, res *datasource.ReadResponse) {
	var cfg kdbSnapshotsModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() {
		return
	}
	all, err := d.client.ListKdbSnapshots(ctx, cfg.InstanceID.ValueString())
	if err != nil {
		res.Diagnostics.AddError("Could not list KDB snapshots", explainBackup(err))
		return
	}
	cfg.Snapshots = make([]kdbSnapshotItemModel, 0, len(all))
	for _, s := range all {
		if v := cfg.Status.ValueString(); v != "" && s.Status != v {
			continue
		}
		cfg.Snapshots = append(cfg.Snapshots, kdbSnapshotItemModel{
			ID:               types.StringValue(s.ID),
			InstanceID:       stringOrNull(s.InstanceID),
			InstanceName:     stringOrNull(s.InstanceName),
			Label:            stringOrNull(s.Label),
			Status:           types.StringValue(s.Status),
			SizeBytes:        int64OrNull(s.SizeBytes),
			TriggeredBy:      types.StringValue(s.TriggeredBy),
			Cloud:            stringOrNull(s.Cloud),
			Region:           stringOrNull(s.Region),
			CreatedAt:        types.StringValue(s.CreatedAt),
			CompletedAt:      stringOrNull(s.CompletedAt),
			ExpiresAt:        stringOrNull(s.TTD),
			CloudAccountID:   stringOrNull(s.CloudAccountID),
			CloudAccountName: stringOrNull(s.CloudAccountName),
			DeliveryStatus:   stringOrNull(s.DeliveryStatus),
			DeliveredURI:     stringOrNull(s.DeliveredURI),
		})
	}
	res.Diagnostics.Append(res.State.Set(ctx, &cfg)...)
}
