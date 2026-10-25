package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*diskSnapshotsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*diskSnapshotsDataSource)(nil)
)

func NewDiskSnapshotsDataSource() datasource.DataSource { return &diskSnapshotsDataSource{} }

// diskSnapshotsDataSource lists disk snapshots -- the organization's, or one
// database's -- including the scheduled ones no configuration created, so one
// can be named in kividb_instance.restore_from_snapshot_id.
type diskSnapshotsDataSource struct{ client *Client }

type diskSnapshotsModel struct {
	InstanceID types.String            `tfsdk:"instance_id"`
	Status     types.String            `tfsdk:"status"`
	Snapshots  []diskSnapshotItemModel `tfsdk:"snapshots"`
}

type diskSnapshotItemModel struct {
	ID               types.String  `tfsdk:"id"`
	InstanceID       types.String  `tfsdk:"instance_id"`
	InstanceName     types.String  `tfsdk:"instance_name"`
	Label            types.String  `tfsdk:"label"`
	Status           types.String  `tfsdk:"status"`
	SizeGB           types.Float64 `tfsdk:"size_gb"`
	TriggeredBy      types.String  `tfsdk:"triggered_by"`
	Cloud            types.String  `tfsdk:"cloud"`
	Region           types.String  `tfsdk:"region"`
	CreatedAt        types.String  `tfsdk:"created_at"`
	CompletedAt      types.String  `tfsdk:"completed_at"`
	ExpiresAt        types.String  `tfsdk:"expires_at"`
	CloudAccountID   types.String  `tfsdk:"cloud_account_id"`
	CloudAccountName types.String  `tfsdk:"cloud_account_name"`
}

func (d *diskSnapshotsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, res *datasource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_disk_snapshots"
}

func (d *diskSnapshotsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, res *datasource.SchemaResponse) {
	c := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	res.Schema = schema.Schema{
		MarkdownDescription: "Disk snapshots of your databases, newest first: those taken on demand " +
			"(`kividb_disk_snapshot`, the console, an assistant) and on a schedule. Use one's `id` as " +
			"`kividb_instance.restore_from_snapshot_id` to make a new database from it.",
		Attributes: map[string]schema.Attribute{
			"instance_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "List only this database's snapshots. Unset, every snapshot in the organization " +
					"is listed, including those of databases since deleted.",
			},
			"status": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "List only snapshots with this status, for example `ready`. Only a finished " +
					"snapshot can be restored from.",
			},
			"snapshots": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The snapshots, newest first.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":            c("Snapshot id."),
						"instance_id":   c("The database it was taken from. Null once that database is deleted."),
						"instance_name": c("Name of the database it was taken from."),
						"label":         c("The snapshot's name."),
						"status":        c("Snapshot state as the server reports it, such as `creating`, `ready` or `failed`."),
						"size_gb":       schema.Float64Attribute{Computed: true, MarkdownDescription: "Size of the captured volume, once known."},
						"triggered_by":  c("What took it, such as `manual` or `schedule`."),
						"cloud":         c("Cloud the snapshot is stored in."),
						"region":        c("Region the snapshot is stored in. A database made from it must be in this region."),
						"created_at":    c("When the snapshot was requested."),
						"completed_at":  c("When the copy finished."),
						"expires_at":    c("When KiviDB deletes it on its own under the retention policy. Null when it is kept until deleted."),
						"cloud_account_id": c("The cloud account it is stored in; null for KiviDB's cloud. A database " +
							"made from it is placed in this account."),
						"cloud_account_name": c("The name of that cloud account."),
					},
				},
			},
		},
	}
}

func (d *diskSnapshotsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, res *datasource.ConfigureResponse) {
	client, ok := clientFrom(req.ProviderData, &res.Diagnostics)
	if ok {
		d.client = client
	}
}

func (d *diskSnapshotsDataSource) Read(ctx context.Context, req datasource.ReadRequest, res *datasource.ReadResponse) {
	var cfg diskSnapshotsModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() {
		return
	}
	all, err := d.client.ListDiskSnapshots(ctx, cfg.InstanceID.ValueString())
	if err != nil {
		res.Diagnostics.AddError("Could not list disk snapshots", explainBackup(err))
		return
	}
	cfg.Snapshots = make([]diskSnapshotItemModel, 0, len(all))
	for _, s := range all {
		if v := cfg.Status.ValueString(); v != "" && s.Status != v {
			continue
		}
		name := s.ResolvedInstanceName
		if name == nil {
			name = s.InstanceName
		}
		item := diskSnapshotItemModel{
			ID:               types.StringValue(s.ID),
			InstanceID:       stringOrNull(s.InstanceID),
			InstanceName:     stringOrNull(name),
			Label:            stringOrNull(s.label()),
			Status:           types.StringValue(s.Status),
			SizeGB:           types.Float64Null(),
			TriggeredBy:      types.StringValue(s.TriggeredBy),
			Cloud:            types.StringValue(s.Cloud),
			Region:           types.StringValue(s.Region),
			CreatedAt:        types.StringValue(s.CreatedAt),
			CompletedAt:      stringOrNull(s.CompletedAt),
			ExpiresAt:        stringOrNull(s.TTD),
			CloudAccountID:   stringOrNull(s.CloudAccountID),
			CloudAccountName: stringOrNull(s.CloudAccountName),
		}
		if s.SizeGB.Value != nil {
			item.SizeGB = types.Float64Value(*s.SizeGB.Value)
		}
		cfg.Snapshots = append(cfg.Snapshots, item)
	}
	res.Diagnostics.Append(res.State.Set(ctx, &cfg)...)
}
