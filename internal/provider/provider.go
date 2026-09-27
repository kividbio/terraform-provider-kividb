package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const defaultEndpoint = "https://api.kividb.io"

var _ provider.Provider = (*kividbProvider)(nil)

type kividbProvider struct {
	version string
}

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &kividbProvider{version: version} }
}

func (p *kividbProvider) Metadata(_ context.Context, _ provider.MetadataRequest, res *provider.MetadataResponse) {
	res.TypeName = "kividb"
	res.Version = p.version
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	APIKey   types.String `tfsdk:"api_key"`
}

func (p *kividbProvider) Schema(_ context.Context, _ provider.SchemaRequest, res *provider.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "Manage KiviDB Cloud databases.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "KiviDB Cloud API base URL. Defaults to `" + defaultEndpoint +
					"`, or `KIVIDB_API_ENDPOINT` if set.",
			},
			"api_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "An API key from the KiviDB console (Dashboard → API keys). " +
					"Prefer `KIVIDB_API_KEY` in the environment: a key written into a `.tf` file " +
					"ends up in version control, and a key in a variable ends up in state.",
			},
		},
	}
}

func (p *kividbProvider) Configure(ctx context.Context, req provider.ConfigureRequest, res *provider.ConfigureResponse) {
	var cfg providerModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() {
		return
	}

	// The environment wins only where the configuration is silent. A provider
	// block that names a key means that key, or the two disagree invisibly
	// depending on who is running it.
	endpoint := os.Getenv("KIVIDB_API_ENDPOINT")
	if !cfg.Endpoint.IsNull() && cfg.Endpoint.ValueString() != "" {
		endpoint = cfg.Endpoint.ValueString()
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	apiKey := os.Getenv("KIVIDB_API_KEY")
	if !cfg.APIKey.IsNull() && cfg.APIKey.ValueString() != "" {
		apiKey = cfg.APIKey.ValueString()
	}
	if apiKey == "" {
		res.Diagnostics.AddError(
			"No KiviDB API key",
			"Set KIVIDB_API_KEY in the environment, or `api_key` in the provider block. "+
				"Create one in the KiviDB console under Dashboard → API keys.",
		)
		return
	}

	client := NewClient(endpoint, apiKey)

	// Fail here rather than at the first resource. A bad key discovered during
	// apply can leave a half-built plan; discovered at configure time it is just
	// a message.
	if err := client.Whoami(ctx); err != nil {
		if apiErr, ok := err.(*APIError); ok && apiErr.Status == 401 {
			res.Diagnostics.AddError(
				"KiviDB rejected that API key",
				"The key was not accepted. It may have been revoked, expired, or copied incompletely -- "+
					"a key is one line beginning `kvdb_`. Check it under Dashboard → API keys.",
			)
			return
		}
		res.Diagnostics.AddError(
			"Could not reach the KiviDB API",
			"Tried "+endpoint+": "+err.Error(),
		)
		return
	}

	res.DataSourceData = client
	res.ResourceData = client
}

func (p *kividbProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewInstanceResource,
	}
}

func (p *kividbProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewInstanceDataSource,
	}
}
