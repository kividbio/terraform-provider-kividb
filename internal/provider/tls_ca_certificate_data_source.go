package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*tlsCACertificateDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*tlsCACertificateDataSource)(nil)
)

func NewTLSCACertificateDataSource() datasource.DataSource { return &tlsCACertificateDataSource{} }

type tlsCACertificateDataSource struct{ client *Client }

type tlsCACertificateModel struct {
	ID             types.String `tfsdk:"id"`
	InstanceID     types.String `tfsdk:"instance_id"`
	CertificatePEM types.String `tfsdk:"certificate_pem"`
}

func (d *tlsCACertificateDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, res *datasource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_tls_ca_certificate"
}

func (d *tlsCACertificateDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, res *datasource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "The CA certificate that signs a database's TLS certificate, for a client to " +
			"trust when it connects with TLS (`tls_enabled` on `kividb_instance`).\n\n" +
			"It is the same certificate for every database KiviDB runs, so reading it for one database " +
			"gives the CA for all of them. It is public, not a secret: it is what clients check the " +
			"server against, and holding it lets nobody in.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The database's id.",
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "A database in this organization.",
			},
			"certificate_pem": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The CA certificate, in PEM.",
			},
		},
	}
}

func (d *tlsCACertificateDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, res *datasource.ConfigureResponse) {
	if c := configureDataSourceClient(req, res); c != nil {
		d.client = c
	}
}

func (d *tlsCACertificateDataSource) Read(ctx context.Context, req datasource.ReadRequest, res *datasource.ReadResponse) {
	var m tlsCACertificateModel
	res.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if res.Diagnostics.HasError() {
		return
	}
	pem, err := d.client.TLSCACertificate(ctx, m.InstanceID.ValueString())
	if err != nil {
		res.Diagnostics.AddError("Could not read the TLS CA certificate", explainDataSourceRead(err, m.InstanceID.ValueString()))
		return
	}
	m.ID = m.InstanceID
	m.CertificatePEM = types.StringValue(pem)
	res.Diagnostics.Append(res.State.Set(ctx, &m)...)
}

func configureDataSourceClient(req datasource.ConfigureRequest, res *datasource.ConfigureResponse) *Client {
	if req.ProviderData == nil {
		return nil
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		res.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return nil
	}
	return client
}

// explainDataSourceRead says which database was not found: a data source has
// no state to drop, so a missing database is an error worth naming.
func explainDataSourceRead(err error, instanceID string) string {
	if isNotFound(err) {
		return fmt.Sprintf("No database with id %s exists in this organization.", instanceID)
	}
	return explain(err)
}
