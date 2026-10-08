package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource                     = (*cloudAccountDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*cloudAccountDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*cloudAccountDataSource)(nil)
)

func NewCloudAccountDataSource() datasource.DataSource { return &cloudAccountDataSource{} }

// cloudAccountDataSource looks up one cloud account connected in the console.
//
// There is deliberately no resource for this. Connecting an account grants
// KiviDB a role in it, and that is done signed in, by a person, in the
// console -- an API key cannot do it. What Terraform needs is the id to place
// a database in, found by a name a person can read.
type cloudAccountDataSource struct{ client *Client }

type cloudAccountModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Cloud        types.String `tfsdk:"cloud"`
	AWSAccountID types.String `tfsdk:"aws_account_id"`
	GCPProjectID types.String `tfsdk:"gcp_project_id"`
	Regions      types.List   `tfsdk:"regions"`
	Status       types.String `tfsdk:"status"`

	AzureTenantID          types.String `tfsdk:"azure_tenant_id"`
	AzureSubscriptionID    types.String `tfsdk:"azure_subscription_id"`
	AzureResourceGroup     types.String `tfsdk:"azure_resource_group"`
	AzureSnapshotAccount   types.String `tfsdk:"azure_snapshot_account"`
	AzureSnapshotContainer types.String `tfsdk:"azure_snapshot_container"`
}

func (d *cloudAccountDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, res *datasource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_cloud_account"
}

func (d *cloudAccountDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, res *datasource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "A cloud account of your own, connected in the KiviDB console, that databases " +
			"can run in. Look it up by `id` or by `name` (exactly one) and pass its `id` to " +
			"`kividb_instance.cloud_account_id`. Accounts are connected in the console, not from Terraform. " +
			"AWS accounts, Google Cloud projects and Azure subscriptions are supported.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The account's id. Set this or `name`.",
			},
			"name": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The name the account was given in the console. Set this or `id`; it must match exactly one account.",
			},
			"cloud": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The account's cloud: `aws`, `azure` or `gcp`.",
			},
			"aws_account_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The 12-digit AWS account id, for an AWS account.",
			},
			"gcp_project_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The project ID, for a Google Cloud project.",
			},
			"azure_tenant_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The Microsoft Entra tenant ID, for an Azure subscription.",
			},
			"azure_subscription_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The subscription ID, for an Azure subscription.",
			},
			"azure_resource_group": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The resource group databases are created in, for an Azure subscription.",
			},
			"azure_snapshot_account": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The storage account snapshots are kept in, for an Azure subscription.",
			},
			"azure_snapshot_container": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The blob container in `azure_snapshot_account` snapshots are kept in, for an Azure subscription.",
			},
			"regions": schema.ListAttribute{
				ElementType:         types.StringType,
				Computed:            true,
				MarkdownDescription: "Regions enabled for this account. A database in it must use one of them.",
			},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`verified` once KiviDB has confirmed it can act in the account; " +
					"`pending` or `broken` otherwise. Only a verified account can take new databases.",
			},
		},
	}
}

func (d *cloudAccountDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("name")),
	}
}

func (d *cloudAccountDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, res *datasource.ConfigureResponse) {
	client, ok := clientFrom(req.ProviderData, &res.Diagnostics)
	if ok {
		d.client = client
	}
}

func (d *cloudAccountDataSource) Read(ctx context.Context, req datasource.ReadRequest, res *datasource.ReadResponse) {
	var cfg cloudAccountModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() {
		return
	}

	accounts, err := d.client.ListCloudAccounts(ctx)
	if err != nil {
		res.Diagnostics.AddError("Could not list connected cloud accounts", explainCloudAccounts(err))
		return
	}

	account, err := findCloudAccount(accounts, cfg.ID.ValueString(), cfg.Name.ValueString())
	if err != nil {
		res.Diagnostics.AddError("Could not find that cloud account", err.Error())
		return
	}

	if account.Status != "verified" {
		res.Diagnostics.AddWarning(
			"Cloud account is not verified",
			fmt.Sprintf("%q has status %q. New databases can only be created in a verified account; "+
				"finish connecting it in the KiviDB console.", account.Label(), account.Status),
		)
	}

	state := cloudAccountState(*account)
	// Terraform requires a configured value back exactly as written, and the
	// id matched case-insensitively.
	if cfg.ID.ValueString() != "" {
		state.ID = cfg.ID
	}
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func cloudAccountState(a CloudAccount) cloudAccountModel {
	return cloudAccountModel{
		ID:           types.StringValue(a.ID),
		Name:         types.StringValue(a.Label()),
		Cloud:        types.StringValue(a.Cloud),
		AWSAccountID: stringOrNull(a.AWSAccountID),
		GCPProjectID: stringOrNull(a.GCPProjectID),
		Regions:      stringList(a.EnabledRegions),
		Status:       types.StringValue(a.Status),

		AzureTenantID:          stringOrNull(a.AzureTenantID),
		AzureSubscriptionID:    stringOrNull(a.AzureSubscriptionID),
		AzureResourceGroup:     stringOrNull(a.AzureResourceGroup),
		AzureSnapshotAccount:   stringOrNull(a.AzureSnapshotAccount),
		AzureSnapshotContainer: stringOrNull(a.AzureSnapshotContainer),
	}
}

// findCloudAccount picks the one account named by id or by name.
//
// Names are not unique in the console, so a name that matches two accounts is
// an error naming both ids rather than a guess: placing a database in the wrong
// account puts the data somewhere nobody intended.
func findCloudAccount(accounts []CloudAccount, id, name string) (*CloudAccount, error) {
	if id != "" {
		for i := range accounts {
			if strings.EqualFold(accounts[i].ID, id) {
				return &accounts[i], nil
			}
		}
		return nil, fmt.Errorf("no connected cloud account has id %q. %s", id, available(accounts))
	}
	if name == "" {
		return nil, fmt.Errorf("set `id` or `name` to say which cloud account to read")
	}

	var matches []*CloudAccount
	for i := range accounts {
		if accounts[i].Label() == name {
			matches = append(matches, &accounts[i])
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("no connected cloud account is named %q. %s", name, available(accounts))
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		return nil, fmt.Errorf("%d connected cloud accounts are named %q (%s). Look the account up by `id` "+
			"instead, or rename one of them in the KiviDB console.", len(matches), name, strings.Join(ids, ", "))
	}
}

func available(accounts []CloudAccount) string {
	if len(accounts) == 0 {
		return "This organization has no connected cloud accounts; connect one in the KiviDB console."
	}
	names := make([]string, 0, len(accounts))
	for _, a := range accounts {
		names = append(names, fmt.Sprintf("%q (%s)", a.Label(), a.ID))
	}
	sort.Strings(names)
	return "Connected accounts: " + strings.Join(names, ", ") + "."
}

// explainCloudAccounts adds what a refusal of the list means here. A 404 is the
// whole route being absent, not one account missing.
func explainCloudAccounts(err error) string {
	if apiErr, ok := err.(*APIError); ok && apiErr.IsNotFound() {
		return "Running databases in your own cloud account is not enabled for this organization. " +
			"Contact KiviDB support to turn it on."
	}
	return explain(err)
}

var (
	_ datasource.DataSource              = (*cloudAccountsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*cloudAccountsDataSource)(nil)
)

func NewCloudAccountsDataSource() datasource.DataSource { return &cloudAccountsDataSource{} }

// cloudAccountsDataSource lists every connected cloud account.
type cloudAccountsDataSource struct{ client *Client }

type cloudAccountsModel struct {
	CloudAccounts []cloudAccountModel `tfsdk:"cloud_accounts"`
}

func (d *cloudAccountsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, res *datasource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_cloud_accounts"
}

func (d *cloudAccountsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, res *datasource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "Every cloud account of your own connected in the KiviDB console. " +
			"To pick one account, use `kividb_cloud_account`.",
		Attributes: map[string]schema.Attribute{
			"cloud_accounts": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The connected accounts, oldest first.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "The account's id."},
						"name":           schema.StringAttribute{Computed: true, MarkdownDescription: "The name given in the console."},
						"cloud":          schema.StringAttribute{Computed: true, MarkdownDescription: "The account's cloud: `aws`, `azure` or `gcp`."},
						"aws_account_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The 12-digit AWS account id, for an AWS account."},
						"gcp_project_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The project ID, for a Google Cloud project."},
						"azure_tenant_id": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The Microsoft Entra tenant ID, for an Azure subscription.",
						},
						"azure_subscription_id": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The subscription ID, for an Azure subscription.",
						},
						"azure_resource_group": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The resource group databases are created in, for an Azure subscription.",
						},
						"azure_snapshot_account": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The storage account snapshots are kept in, for an Azure subscription.",
						},
						"azure_snapshot_container": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The blob container snapshots are kept in, for an Azure subscription.",
						},
						"regions": schema.ListAttribute{
							ElementType:         types.StringType,
							Computed:            true,
							MarkdownDescription: "Regions enabled for this account.",
						},
						"status": schema.StringAttribute{Computed: true, MarkdownDescription: "`verified`, `pending` or `broken`."},
					},
				},
			},
		},
	}
}

func (d *cloudAccountsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, res *datasource.ConfigureResponse) {
	client, ok := clientFrom(req.ProviderData, &res.Diagnostics)
	if ok {
		d.client = client
	}
}

func (d *cloudAccountsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, res *datasource.ReadResponse) {
	accounts, err := d.client.ListCloudAccounts(ctx)
	if err != nil {
		res.Diagnostics.AddError("Could not list connected cloud accounts", explainCloudAccounts(err))
		return
	}
	state := cloudAccountsModel{CloudAccounts: make([]cloudAccountModel, 0, len(accounts))}
	for _, a := range accounts {
		state.CloudAccounts = append(state.CloudAccounts, cloudAccountState(a))
	}
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// clientFrom unpacks the provider's client. Nil provider data is the framework
// validating before Configure has run, which is not an error.
func clientFrom(data any, diags *diag.Diagnostics) (*Client, bool) {
	if data == nil {
		return nil, false
	}
	client, ok := data.(*Client)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("got %T", data))
		return nil, false
	}
	return client, true
}
