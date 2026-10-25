package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*clientCertificatesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*clientCertificatesDataSource)(nil)
)

func NewClientCertificatesDataSource() datasource.DataSource { return &clientCertificatesDataSource{} }

type clientCertificatesDataSource struct{ client *Client }

type clientCertificatesModel struct {
	ID             types.String `tfsdk:"id"`
	InstanceID     types.String `tfsdk:"instance_id"`
	Mode           types.String `tfsdk:"mode"`
	CAPem          types.String `tfsdk:"ca_pem"`
	CACertificates types.List   `tfsdk:"ca_certificates"`
	TLSEnabled     types.Bool   `tfsdk:"tls_enabled"`
	TLSOnly        types.Bool   `tfsdk:"tls_only"`
	Available      types.Bool   `tfsdk:"available"`
}

var clientCACertificateType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"subject":            types.StringType,
	"not_after":          types.StringType,
	"fingerprint_sha256": types.StringType,
}}

func (d *clientCertificatesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, res *datasource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_client_certificates"
}

func (d *clientCertificatesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, res *datasource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "How a database checks client certificates (mutual TLS), and what is in the CA " +
			"bundle it checks them against -- including when each CA expires, which is when every client " +
			"certificate it signed stops being accepted.\n\n" +
			"Set the mode and the CA with `client_cert_mode` and `client_ca_pem` on `kividb_instance`; " +
			"this reads them for a database that configuration does not manage, or to watch expiry.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The database's id."},
			"instance_id": schema.StringAttribute{Required: true, MarkdownDescription: "A database in this organization."},
			"mode": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`off`, `optional` or `required`.",
			},
			"ca_pem": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The CA bundle the database checks client certificates against, in PEM. Null when the mode is `off`.",
			},
			"ca_certificates": schema.ListAttribute{
				ElementType: clientCACertificateType,
				Computed:    true,
				MarkdownDescription: "Each certificate in `ca_pem`: its `subject`, when it expires (`not_after`, " +
					"RFC 3339) and its SHA-256 fingerprint (`fingerprint_sha256`). Empty when there is none.",
			},
			"tls_enabled": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the database serves TLS, which client certificates need."},
			"tls_only":    schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether plaintext connections are refused, which `required` needs."},
			"available": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether this database can use client certificates at all. False for sharded databases.",
			},
		},
	}
}

func (d *clientCertificatesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, res *datasource.ConfigureResponse) {
	if c := configureDataSourceClient(req, res); c != nil {
		d.client = c
	}
}

func (d *clientCertificatesDataSource) Read(ctx context.Context, req datasource.ReadRequest, res *datasource.ReadResponse) {
	var m clientCertificatesModel
	res.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if res.Diagnostics.HasError() {
		return
	}
	cc, err := d.client.GetClientCertificates(ctx, m.InstanceID.ValueString())
	if err != nil {
		res.Diagnostics.AddError("Could not read the client certificate settings", explainDataSourceRead(err, m.InstanceID.ValueString()))
		return
	}
	m.ID = m.InstanceID
	mode := cc.Mode
	if mode == "" {
		mode = "off"
	}
	m.Mode = types.StringValue(mode)
	if cc.CAPem == nil || *cc.CAPem == "" {
		m.CAPem = types.StringNull()
	} else {
		m.CAPem = types.StringValue(*cc.CAPem)
	}
	elems := make([]attr.Value, 0, len(cc.CACertificates))
	for _, c := range cc.CACertificates {
		elems = append(elems, types.ObjectValueMust(clientCACertificateType.AttrTypes, map[string]attr.Value{
			"subject":            types.StringValue(c.Subject),
			"not_after":          types.StringValue(c.NotAfter),
			"fingerprint_sha256": types.StringValue(c.Fingerprint256),
		}))
	}
	m.CACertificates = types.ListValueMust(clientCACertificateType, elems)
	m.TLSEnabled = types.BoolValue(cc.TLSEnabled)
	m.TLSOnly = types.BoolValue(cc.TLSOnly)
	m.Available = types.BoolValue(cc.Available)
	res.Diagnostics.Append(res.State.Set(ctx, &m)...)
}
