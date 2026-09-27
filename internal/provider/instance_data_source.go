package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*instanceDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*instanceDataSource)(nil)
)

func NewInstanceDataSource() datasource.DataSource { return &instanceDataSource{} }

type instanceDataSource struct{ client *Client }

// Read-only, so it needs none of the resource's plan modifiers and none of its
// guardrails -- reading a database cannot change one.
type instanceDataSourceModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Tier           types.String `tfsdk:"tier"`
	Cloud          types.String `tfsdk:"cloud"`
	Region         types.String `tfsdk:"region"`
	DataSizeGB     types.Int64  `tfsdk:"data_size_gb"`
	ThroughputMode types.String `tfsdk:"throughput_mode"`
	ReplicaCount   types.Int64  `tfsdk:"replica_count"`
	KividbVersion  types.String `tfsdk:"kividb_version"`
	Status         types.String `tfsdk:"status"`
	Endpoint       types.String `tfsdk:"endpoint"`
	PublicEndpoint types.String `tfsdk:"public_endpoint"`
	TLSEnabled     types.Bool   `tfsdk:"tls_enabled"`
	OrgID          types.String `tfsdk:"org_id"`
}

func (d *instanceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, res *datasource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_instance"
}

func (d *instanceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, res *datasource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "Details of an existing KiviDB database, for referring to one this " +
			"configuration does not manage.",
		Attributes: map[string]schema.Attribute{
			"id":              schema.StringAttribute{Required: true, MarkdownDescription: "The instance's id."},
			"name":            schema.StringAttribute{Computed: true},
			"tier":            schema.StringAttribute{Computed: true},
			"cloud":           schema.StringAttribute{Computed: true},
			"region":          schema.StringAttribute{Computed: true},
			"data_size_gb":    schema.Int64Attribute{Computed: true},
			"throughput_mode": schema.StringAttribute{Computed: true},
			"replica_count":   schema.Int64Attribute{Computed: true},
			"kividb_version":  schema.StringAttribute{Computed: true},
			"status":          schema.StringAttribute{Computed: true},
			"endpoint":        schema.StringAttribute{Computed: true},
			"public_endpoint": schema.StringAttribute{Computed: true},
			"tls_enabled":     schema.BoolAttribute{Computed: true},
			"org_id":          schema.StringAttribute{Computed: true},
		},
	}
}

func (d *instanceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, res *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		res.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	d.client = client
}

func (d *instanceDataSource) Read(ctx context.Context, req datasource.ReadRequest, res *datasource.ReadResponse) {
	var cfg instanceDataSourceModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() {
		return
	}
	inst, err := d.client.GetInstance(ctx, cfg.ID.ValueString())
	if err != nil {
		res.Diagnostics.AddError("Could not read that database", explain(err))
		return
	}
	cfg.Name = stringOrNull(inst.Name)
	cfg.Tier = types.StringValue(inst.Tier)
	cfg.Cloud = types.StringValue(inst.Cloud)
	cfg.Region = types.StringValue(inst.Region)
	cfg.DataSizeGB = types.Int64Value(inst.DataSizeGB)
	cfg.ThroughputMode = types.StringValue(inst.ThroughputMode)
	cfg.ReplicaCount = types.Int64Value(inst.ReplicaCount)
	cfg.KividbVersion = stringOrNull(inst.KividbVersion)
	cfg.Status = types.StringValue(inst.Status)
	cfg.Endpoint = stringOrNull(inst.Endpoint)
	cfg.PublicEndpoint = stringOrNull(inst.PublicEndpoint)
	cfg.TLSEnabled = types.BoolValue(inst.TLSEnabled)
	cfg.OrgID = stringOrNull(inst.OrgID)
	res.Diagnostics.Append(res.State.Set(ctx, &cfg)...)
}
