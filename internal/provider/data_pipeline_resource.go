package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*dataPipelineResource)(nil)
	_ resource.ResourceWithConfigure      = (*dataPipelineResource)(nil)
	_ resource.ResourceWithImportState    = (*dataPipelineResource)(nil)
	_ resource.ResourceWithValidateConfig = (*dataPipelineResource)(nil)
)

func NewDataPipelineResource() resource.Resource { return &dataPipelineResource{} }

// dataPipelineResource is a data integration pipeline: change data capture from
// a PostgreSQL or MySQL database into a KiviDB database
// (apps/api/src/routes/data-integration.ts).
//
// The pipeline is named, not numbered: the name is the API's key, so it is the
// Terraform id and the import id too.
type dataPipelineResource struct{ client *Client }

type dataPipelineModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	InstanceID        types.String `tfsdk:"instance_id"`
	SourceType        types.String `tfsdk:"source_type"`
	SourceHost        types.String `tfsdk:"source_host"`
	SourcePort        types.Int64  `tfsdk:"source_port"`
	SourceDatabase    types.String `tfsdk:"source_database"`
	SourceUser        types.String `tfsdk:"source_user"`
	SourcePassword    types.String `tfsdk:"source_password"`
	SourceTLS         types.String `tfsdk:"source_tls"`
	Tables            types.List   `tfsdk:"tables"`
	Enabled           types.Bool   `tfsdk:"enabled"`
	ForceDelete       types.Bool   `tfsdk:"force_delete"`
	ResnapshotTrigger types.String `tfsdk:"resnapshot_trigger"`

	InstanceName        types.String `tfsdk:"instance_name"`
	Status              types.String `tfsdk:"status"`
	SlotName            types.String `tfsdk:"slot_name"`
	SnapshotDone        types.Bool   `tfsdk:"snapshot_done"`
	RowsSnapshotted     types.Int64  `tfsdk:"rows_snapshotted"`
	ChangesApplied      types.Int64  `tfsdk:"changes_applied"`
	LastChangeAt        types.String `tfsdk:"last_change_at"`
	LagMS               types.Int64  `tfsdk:"lag_ms"`
	LastError           types.String `tfsdk:"last_error"`
	LastErrorAt         types.String `tfsdk:"last_error_at"`
	ConsecutiveFailures types.Int64  `tfsdk:"consecutive_failures"`
	CreatedAt           types.String `tfsdk:"created_at"`
}

type pipelineTableModel struct {
	Table      types.String `tfsdk:"table"`
	Key        types.String `tfsdk:"key"`
	Type       types.String `tfsdk:"type"`
	Columns    types.List   `tfsdk:"columns"`
	Rename     types.Map    `tfsdk:"rename"`
	TTLSeconds types.Int64  `tfsdk:"ttl_seconds"`
}

var pipelineTableAttrTypes = map[string]attr.Type{
	"table":       types.StringType,
	"key":         types.StringType,
	"type":        types.StringType,
	"columns":     types.ListType{ElemType: types.StringType},
	"rename":      types.MapType{ElemType: types.StringType},
	"ttl_seconds": types.Int64Type,
}

// The API's own rules (packages/shared/src/data-integration.ts), repeated so a
// mistake is found at plan rather than after a round trip to the source.
var (
	pipelineNamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	pipelineHostPattern   = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
	pipelineTablePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}(\.[A-Za-z_][A-Za-z0-9_$]{0,62})?$`)
	pipelineColumnPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)
	pipelineKeyPattern    = regexp.MustCompile(`^[^\s{}]*(\{[A-Za-z_][A-Za-z0-9_$]{0,62}\}[^\s{}]*)+$`)
	pipelinePlaceholder   = regexp.MustCompile(`\{([^}]+)\}`)
	// The API trims these before storing them; a value with spaces at either
	// end would be stored without them and read back as a change.
	trimmedPattern = regexp.MustCompile(`^\S(.*\S)?$`)
)

func (r *dataPipelineResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_data_pipeline"
}

func (r *dataPipelineResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: "A data integration pipeline: it copies tables from your PostgreSQL or MySQL " +
			"database into a KiviDB database, first as a snapshot and then change by change, so the " +
			"KiviDB copy stays seconds behind the source.\n\n" +
			"**A pipeline is billed.** It costs $0.04 for every hour, or part of an hour, in which it " +
			"runs, plus $0.10 per GB it writes into the database beyond 10 GB a month. Starting one " +
			"needs enough credit in the organization to cover about a month of it on top of what is " +
			"already running; when there is not enough, the create (or `enabled = true`) is refused " +
			"with the shortfall.\n\n" +
			"A new pipeline starts at once. The API checks the source before saving anything: it " +
			"connects, and reads one row of each table.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The pipeline's name, which is how the API identifies it.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The pipeline's name: 1-63 lowercase letters, digits, `-` or `_`, starting " +
					"with a letter or digit. Unique in the organization. Changing it replaces the pipeline.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators: []validator.String{
					stringvalidator.RegexMatches(pipelineNamePattern,
						"must be 1-63 lowercase letters, digits, - or _, starting with a letter or digit"),
				},
			},
			"instance_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The KiviDB database rows are written into. It cannot change: pointing " +
					"this at another database replaces the pipeline, which drops its replication slot " +
					"and copies every table again into the new one.",
				PlanModifiers: []planmodifier.String{replaceUnlessSameID()},
				Validators: []validator.String{
					stringvalidator.RegexMatches(uuidPattern, "must be a database id (a UUID)"),
				},
			},
			"source_type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "`postgres` (PostgreSQL 10+, logical replication) or `mysql` (MySQL " +
					"5.7, 8.0, 8.4 or MariaDB, the binary log). It cannot change; changing it replaces " +
					"the pipeline. " +
					"PostgreSQL needs `wal_level = logical`, a user with `REPLICATION` (on Amazon RDS, the " +
					"`rds_replication` role) that owns the tables or may create a publication for them, " +
					"and a primary key on every table. MySQL needs `log_bin` on, `binlog_format = ROW`, " +
					"`binlog_row_image = FULL`, and a user with `REPLICATION SLAVE`, `REPLICATION CLIENT` " +
					"and `SELECT` on the tables.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.OneOf("postgres", "mysql")},
			},
			"source_host": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The source database's hostname or IPv4 address. It must resolve to a " +
					"public address: KiviDB connects from its own network, and refuses private, " +
					"loopback and other reserved addresses.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 253),
					stringvalidator.RegexMatches(pipelineHostPattern, "must be a hostname or IPv4 address"),
				},
			},
			"source_port": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "The source database's port, such as 5432 or 3306.",
				Validators:          []validator.Int64{int64validator.Between(1, 65535)},
			},
			"source_database": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database in the source server the tables are in.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 128),
					stringvalidator.RegexMatches(trimmedPattern, "must not start or end with a space"),
				},
			},
			"source_user": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The user KiviDB connects to the source as. See `source_type` for what it needs.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 128),
					stringvalidator.RegexMatches(trimmedPattern, "must not start or end with a space"),
				},
			},
			"source_password": schema.StringAttribute{
				Required:  true,
				Sensitive: true,
				MarkdownDescription: "The source user's password. The API stores it encrypted and never " +
					"returns it, so Terraform keeps the value you configured in state (mark the state " +
					"as sensitive accordingly) and cannot see a password changed in the console. After " +
					"an import the first apply sends it.",
				Validators: []validator.String{stringvalidator.LengthBetween(1, 1024)},
			},
			"source_tls": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("require"),
				MarkdownDescription: "How to connect to the source: `require` (encrypted, certificate not " +
					"checked; the default), `verify` (encrypted and the certificate checked) or `off` " +
					"(plain text).",
				Validators: []validator.String{stringvalidator.OneOf("require", "verify", "off")},
			},
			"tables": schema.ListNestedAttribute{
				Required: true,
				MarkdownDescription: "The tables to copy, 1 to 50, and where each row goes. Adding, removing " +
					"or changing a table copies every table again from the start, then carries on " +
					"streaming.",
				Validators: []validator.List{listvalidator.SizeBetween(1, 50)},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"table": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "`schema.table` for PostgreSQL (`public` when the schema is " +
								"left out); just the table for MySQL, whose database is `source_database`. " +
								"The table needs a primary key.",
							Validators: []validator.String{
								stringvalidator.RegexMatches(pipelineTablePattern, "must be schema.table or table"),
							},
						},
						"key": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "The key each row is written to: literal text with at least one " +
								"`{column}` placeholder and no spaces, such as `customer:{id}`. Use the " +
								"primary key's columns: a delete carries only those (unless a PostgreSQL " +
								"table has `REPLICA IDENTITY FULL`).",
							Validators: []validator.String{
								stringvalidator.LengthAtMost(200),
								stringvalidator.RegexMatches(pipelineKeyPattern,
									"must be literal text with at least one {column}, and no spaces"),
							},
						},
						"type": schema.StringAttribute{
							Optional:            true,
							Computed:            true,
							Default:             stringdefault.StaticString("hash"),
							MarkdownDescription: "`hash` (one field per column; the default) or `json` (one JSON document per row).",
							Validators:          []validator.String{stringvalidator.OneOf("hash", "json")},
						},
						"columns": schema.ListAttribute{
							ElementType: types.StringType,
							Optional:    true,
							MarkdownDescription: "Copy only these columns, 1 to 200. Every column is copied when " +
								"this is left out. Every placeholder in `key` must be one of them.",
							Validators: []validator.List{
								listvalidator.SizeBetween(1, 200),
								listvalidator.ValueStringsAre(
									stringvalidator.RegexMatches(pipelineColumnPattern, "must be a column name")),
							},
						},
						"rename": schema.MapAttribute{
							ElementType:         types.StringType,
							Optional:            true,
							MarkdownDescription: "Store a column under another field name: `{ column = \"field\" }`.",
							Validators: []validator.Map{
								mapvalidator.KeysAre(
									stringvalidator.RegexMatches(pipelineColumnPattern, "must be a column name")),
								mapvalidator.ValueStringsAre(stringvalidator.LengthBetween(1, 100)),
							},
						},
						"ttl_seconds": schema.Int64Attribute{
							Optional:            true,
							MarkdownDescription: "Expire each key this many seconds after it is written, 60 to 31536000 (a year). Keys do not expire when this is left out.",
							Validators:          []validator.Int64{int64validator.Between(60, 31_536_000)},
						},
					},
				},
			},
			"enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether the pipeline runs. Defaults to true. Setting it to true starts " +
					"the pipeline (and retries one that has failed), which needs enough organization " +
					"credit for about a month of it; false stops it, and the hourly charge stops with it. " +
					"A stopped PostgreSQL pipeline keeps its replication slot in the source, so the source " +
					"keeps its WAL for us until the pipeline is started again or deleted: a pipeline left " +
					"stopped for long fills the source's disk. " +
					"A pipeline created with `enabled = false` is created running and stopped straight " +
					"after, because that is how the API creates one: it is charged for that first hour, " +
					"and may have started copying.",
			},
			"force_delete": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "What a destroy does when KiviDB cannot drop the pipeline's replication " +
					"slot in a PostgreSQL source (the source is unreachable, say). False, the default: " +
					"the destroy fails with the API's explanation and the pipeline stays, stopped. True: " +
					"the pipeline is deleted anyway and KiviDB keeps trying to drop the slot for about " +
					"four days, then tells the organization which slot to drop by hand. Until it is " +
					"dropped, the source keeps its WAL. Set it and apply before destroying: a destroy " +
					"uses the value in state.",
			},
			"resnapshot_trigger": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Any value; changing it copies every table again from the start and " +
					"then carries on streaming, without changing anything else. Nothing happens when it " +
					"is first set on a new pipeline, which copies everything anyway.",
			},

			"instance_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The target database's name.",
			},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "What the pipeline is doing: `pending`, `snapshotting`, `streaming`, " +
					"`stopped` or `error`.",
			},
			"slot_name": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The replication slot the pipeline made in a PostgreSQL source, once it " +
					"has made one. Null for MySQL.",
			},
			"snapshot_done": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether the first copy of every table has finished.",
			},
			"rows_snapshotted": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Rows copied by the snapshot.",
			},
			"changes_applied": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Changes applied since the snapshot.",
			},
			"last_change_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the last change was applied.",
			},
			"lag_ms": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "How far behind the source the KiviDB copy was at the last change, in milliseconds.",
			},
			"last_error": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The last failure, in the words of the source or of KiviDB. A failed pipeline is retried on its own.",
			},
			"last_error_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the last failure happened.",
			},
			"consecutive_failures": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Failures in a row since the pipeline last ran cleanly.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the pipeline was created.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *dataPipelineResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
	client, ok := clientFrom(req.ProviderData, &res.Diagnostics)
	if ok {
		r.client = client
	}
}

// ValidateConfig repeats the API's checks across fields: a table listed twice,
// a MySQL table named with a database, a key placeholder that is not one of
// the chosen columns.
func (r *dataPipelineResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, res *resource.ValidateConfigResponse) {
	var cfg dataPipelineModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() || cfg.Tables.IsNull() || cfg.Tables.IsUnknown() {
		return
	}
	var tables []pipelineTableModel
	res.Diagnostics.Append(cfg.Tables.ElementsAs(ctx, &tables, false)...)
	if res.Diagnostics.HasError() {
		return
	}
	mysql := !cfg.SourceType.IsUnknown() && cfg.SourceType.ValueString() == "mysql"
	seen := map[string]bool{}
	for i, t := range tables {
		at := path.Root("tables").AtListIndex(i)
		if t.Table.IsUnknown() || t.Table.IsNull() {
			continue
		}
		name := t.Table.ValueString()
		if seen[name] {
			res.Diagnostics.AddAttributeError(at.AtName("table"), "Table listed twice",
				fmt.Sprintf("%s is listed more than once. List each table once.", name))
		}
		seen[name] = true
		if mysql && strings.Contains(name, ".") {
			res.Diagnostics.AddAttributeError(at.AtName("table"), "MySQL table named with a database",
				fmt.Sprintf("For MySQL, name the table without a database (%q is in `source_database`).", name))
		}
		if t.Key.IsUnknown() || t.Key.IsNull() || t.Columns.IsNull() || t.Columns.IsUnknown() {
			continue
		}
		var cols []string
		if t.Columns.ElementsAs(ctx, &cols, false).HasError() {
			continue
		}
		have := map[string]bool{}
		for _, c := range cols {
			have[c] = true
		}
		for _, m := range pipelinePlaceholder.FindAllStringSubmatch(t.Key.ValueString(), -1) {
			if !have[m[1]] {
				res.Diagnostics.AddAttributeError(at.AtName("key"), "Key uses a column that is not copied",
					fmt.Sprintf("{%s} is not one of the columns. Add it to `columns`, or leave `columns` out to copy every column.", m[1]))
			}
		}
	}
}

// request builds the PUT body from the plan.
func (m dataPipelineModel) request(ctx context.Context) (PutDataPipelineRequest, diag.Diagnostics) {
	var diags diag.Diagnostics
	var tables []pipelineTableModel
	diags.Append(m.Tables.ElementsAs(ctx, &tables, false)...)
	out := PutDataPipelineRequest{
		InstanceID:     strings.ToLower(m.InstanceID.ValueString()),
		SourceType:     m.SourceType.ValueString(),
		SourceHost:     m.SourceHost.ValueString(),
		SourcePort:     m.SourcePort.ValueInt64(),
		SourceDatabase: m.SourceDatabase.ValueString(),
		SourceUser:     m.SourceUser.ValueString(),
		SourceTLS:      m.SourceTLS.ValueString(),
		Tables:         make([]DataPipelineTable, 0, len(tables)),
	}
	if !m.SourcePassword.IsNull() && !m.SourcePassword.IsUnknown() {
		pw := m.SourcePassword.ValueString()
		out.SourcePassword = &pw
	}
	if out.SourceTLS == "" {
		out.SourceTLS = "require"
	}
	for _, t := range tables {
		dt := DataPipelineTable{Table: t.Table.ValueString(), Key: t.Key.ValueString(), Type: t.Type.ValueString()}
		if dt.Type == "" {
			dt.Type = "hash"
		}
		if !t.Columns.IsNull() && !t.Columns.IsUnknown() {
			diags.Append(t.Columns.ElementsAs(ctx, &dt.Columns, false)...)
		}
		if !t.Rename.IsNull() && !t.Rename.IsUnknown() {
			rename := map[string]string{}
			diags.Append(t.Rename.ElementsAs(ctx, &rename, false)...)
			dt.Rename = &rename
		}
		if !t.TTLSeconds.IsNull() && !t.TTLSeconds.IsUnknown() {
			v := t.TTLSeconds.ValueInt64()
			dt.TTLSeconds = &v
		}
		out.Tables = append(out.Tables, dt)
	}
	return out, diags
}

// apply copies the server's view over the model. The password, force_delete
// and resnapshot_trigger are not the server's to report and are left as they
// are.
func (m *dataPipelineModel) apply(ctx context.Context, p *DataPipeline) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(p.Name)
	m.Name = types.StringValue(p.Name)
	// Keep the configured spelling of the same id; see uuidPattern.
	if m.InstanceID.IsNull() || m.InstanceID.IsUnknown() || !strings.EqualFold(m.InstanceID.ValueString(), p.Instance.ID) {
		m.InstanceID = types.StringValue(p.Instance.ID)
	}
	m.SourceType = types.StringValue(p.SourceType)
	m.SourceHost = types.StringValue(p.SourceHost)
	m.SourcePort = types.Int64Value(p.SourcePort)
	m.SourceDatabase = types.StringValue(p.SourceDatabase)
	m.SourceUser = types.StringValue(p.SourceUser)
	m.SourceTLS = types.StringValue(p.SourceTLS)
	m.Enabled = types.BoolValue(p.DesiredState == "running")

	tables := make([]pipelineTableModel, 0, len(p.Tables))
	for _, t := range p.Tables {
		tm := pipelineTableModel{
			Table:      types.StringValue(t.Table),
			Key:        types.StringValue(t.Key),
			Type:       types.StringValue(t.Type),
			Columns:    types.ListNull(types.StringType),
			Rename:     types.MapNull(types.StringType),
			TTLSeconds: types.Int64Null(),
		}
		if tm.Type.ValueString() == "" {
			tm.Type = types.StringValue("hash")
		}
		if t.Columns != nil {
			tm.Columns = stringList(t.Columns)
		}
		if t.Rename != nil {
			v, d := types.MapValueFrom(ctx, types.StringType, *t.Rename)
			diags.Append(d...)
			tm.Rename = v
		}
		if t.TTLSeconds != nil {
			tm.TTLSeconds = types.Int64Value(*t.TTLSeconds)
		}
		tables = append(tables, tm)
	}
	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: pipelineTableAttrTypes}, tables)
	diags.Append(d...)
	m.Tables = list

	m.InstanceName = stringOrNull(p.Instance.Name)
	m.Status = types.StringValue(p.Status)
	m.SlotName = stringOrNull(p.SlotName)
	m.SnapshotDone = types.BoolValue(p.SnapshotDone)
	m.RowsSnapshotted = types.Int64Value(p.RowsSnapshotted)
	m.ChangesApplied = types.Int64Value(p.ChangesApplied)
	m.LastChangeAt = stringOrNull(p.LastChangeAt)
	if p.LagMS != nil {
		m.LagMS = types.Int64Value(*p.LagMS)
	} else {
		m.LagMS = types.Int64Null()
	}
	m.LastError = stringOrNull(p.LastError)
	m.LastErrorAt = stringOrNull(p.LastErrorAt)
	m.ConsecutiveFailures = types.Int64Value(p.ConsecutiveFailures)
	m.CreatedAt = types.StringValue(p.CreatedAt)

	if m.ForceDelete.IsNull() || m.ForceDelete.IsUnknown() {
		m.ForceDelete = types.BoolValue(false)
	}
	if m.ResnapshotTrigger.IsUnknown() {
		m.ResnapshotTrigger = types.StringNull()
	}
	return diags
}

func (r *dataPipelineResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan dataPipelineModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()

	// PUT is an upsert on the name. Without this check a create that reused a
	// name would silently rewrite somebody else's pipeline -- its source, its
	// tables, its password -- and adopt it into this state.
	if _, err := r.client.GetDataPipeline(ctx, name); err == nil {
		res.Diagnostics.AddError("A pipeline with that name already exists",
			fmt.Sprintf("This organization already has a pipeline named %q. Pick another name, or bring it "+
				"under Terraform with `terraform import <address> %s`.", name, name))
		return
	} else if apiErr, ok := err.(*APIError); !ok || !apiErr.IsNotFound() {
		res.Diagnostics.AddError("Could not check whether the pipeline exists", explainPipeline(err))
		return
	}

	body, d := plan.request(ctx)
	res.Diagnostics.Append(d...)
	if res.Diagnostics.HasError() {
		return
	}
	p, err := r.client.PutDataPipeline(ctx, name, body)
	if err != nil {
		res.Diagnostics.AddError("Could not create the pipeline", explainPipeline(err))
		return
	}

	// The API creates every pipeline running.
	if !plan.Enabled.ValueBool() {
		stopped, serr := r.client.StopDataPipeline(ctx, name)
		if serr != nil {
			// The pipeline exists and is billed; its name goes into state either way.
			res.Diagnostics.Append(plan.apply(ctx, p)...)
			res.Diagnostics.AddError("The pipeline was created but could not be stopped",
				"It is running, and charged for while it runs. "+explainPipeline(serr))
			res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
			return
		}
		p = stopped
	}
	res.Diagnostics.Append(plan.apply(ctx, p)...)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *dataPipelineResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state dataPipelineModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if state.ID.ValueString() == "" {
		res.State.RemoveResource(ctx)
		return
	}
	p, err := r.client.GetDataPipeline(ctx, state.ID.ValueString())
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			// Deleted outside Terraform, or with its database: a pipeline goes
			// when the database it writes into is deleted.
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the pipeline", explainPipeline(err))
		return
	}
	res.Diagnostics.Append(state.apply(ctx, p)...)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

func (r *dataPipelineResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan, state dataPipelineModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	name := state.ID.ValueString()

	// Whatever happens below, state ends up holding what the server last said,
	// so a step that fails partway is retried by the next plan rather than
	// forgotten.
	var current *DataPipeline
	fail := func(summary string, err error) {
		res.Diagnostics.AddError(summary, explainPipeline(err))
		if current == nil {
			// Nothing changed on the server: keep what state already said.
			res.Diagnostics.Append(res.State.Set(ctx, &state)...)
			return
		}
		saved := plan
		res.Diagnostics.Append(saved.apply(ctx, current)...)
		res.Diagnostics.Append(res.State.Set(ctx, &saved)...)
	}

	settingsChanged := !plan.SourceHost.Equal(state.SourceHost) ||
		!plan.SourcePort.Equal(state.SourcePort) ||
		!plan.SourceDatabase.Equal(state.SourceDatabase) ||
		!plan.SourceUser.Equal(state.SourceUser) ||
		!plan.SourcePassword.Equal(state.SourcePassword) ||
		!plan.SourceTLS.Equal(state.SourceTLS) ||
		!plan.Tables.Equal(state.Tables)
	if settingsChanged {
		body, d := plan.request(ctx)
		res.Diagnostics.Append(d...)
		if res.Diagnostics.HasError() {
			return
		}
		p, err := r.client.PutDataPipeline(ctx, name, body)
		if err != nil {
			fail("Could not change the pipeline", err)
			return
		}
		current = p
	}

	// A change of tables already copies everything again; a second request
	// would only restart that copy.
	if !plan.ResnapshotTrigger.Equal(state.ResnapshotTrigger) && !plan.ResnapshotTrigger.IsNull() &&
		plan.Tables.Equal(state.Tables) {
		p, err := r.client.ResnapshotDataPipeline(ctx, name)
		if err != nil {
			fail("Could not start copying the tables again", err)
			return
		}
		current = p
	}

	if current == nil {
		p, err := r.client.GetDataPipeline(ctx, name)
		if err != nil {
			res.Diagnostics.AddError("Could not read the pipeline", explainPipeline(err))
			return
		}
		current = p
	}
	running := current.DesiredState == "running"
	switch want := plan.Enabled.ValueBool(); {
	case want && !running:
		p, err := r.client.StartDataPipeline(ctx, name)
		if err != nil {
			fail("Could not start the pipeline", err)
			return
		}
		current = p
	case !want && running:
		p, err := r.client.StopDataPipeline(ctx, name)
		if err != nil {
			fail("Could not stop the pipeline", err)
			return
		}
		current = p
	}

	res.Diagnostics.Append(plan.apply(ctx, current)...)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *dataPipelineResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state dataPipelineModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	name := state.ID.ValueString()
	force := state.ForceDelete.ValueBool()

	out, err := r.client.DeleteDataPipeline(ctx, name, force)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			if apiErr.IsNotFound() {
				return
			}
			res.Diagnostics.AddError("Could not delete the pipeline", explainPipeline(err))
			return
		}
		// No answer: the delete may have happened. Look before reporting a
		// failure that would leave a deleted pipeline in state.
		if _, gerr := r.client.GetDataPipeline(ctx, name); gerr != nil {
			if errors.As(gerr, &apiErr) && apiErr.IsNotFound() {
				return
			}
		}
		res.Diagnostics.AddError("Could not delete the pipeline", explainPipeline(err))
		return
	}
	if out != nil && out.SlotLeftInSource != "" {
		res.Diagnostics.AddWarning("Replication slot left in the source",
			fmt.Sprintf("The pipeline is deleted, but KiviDB could not drop the replication slot %s in the "+
				"source. It keeps trying for about four days and tells the organization if it cannot. Until "+
				"the slot is dropped the source keeps its WAL; you can drop it yourself with "+
				"SELECT pg_drop_replication_slot('%s'); and drop the publication of the same name.",
				out.SlotLeftInSource, out.SlotLeftInSource))
	}
}

func (r *dataPipelineResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	p, err := r.client.GetDataPipeline(ctx, req.ID)
	if err != nil {
		res.Diagnostics.AddError("Could not import that pipeline", explainPipeline(err))
		return
	}
	var state dataPipelineModel
	// Not the server's to report: the password is never returned, and the
	// other two describe what this configuration does, not the pipeline.
	state.SourcePassword = types.StringNull()
	state.ForceDelete = types.BoolValue(false)
	state.ResnapshotTrigger = types.StringNull()
	res.Diagnostics.Append(state.apply(ctx, p)...)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// explainPipeline adds what a pipeline refusal means in a Terraform run to the
// API's own words, which are always kept whole.
func explainPipeline(err error) string {
	apiErr, ok := err.(*APIError)
	if !ok {
		return explain(err)
	}
	switch apiErr.Code {
	case "source_cleanup_failed":
		return apiErr.Error() + "\n\nTo delete it anyway from Terraform, set `force_delete = true`, " +
			"apply, and destroy again."
	case "source_check_failed":
		return apiErr.Error() + "\n\nNothing was saved. KiviDB connects to the source and reads one row " +
			"of each table before it saves a pipeline; see `source_type` in the documentation for what " +
			"the source and its user need."
	case "pipeline_not_found":
		return apiErr.Error() + "\n\nNo pipeline in this organization has that name."
	default:
		return explain(err)
	}
}

// replaceUnlessSameID replaces the resource when an id changes, but not when
// the same id is written in another case: the API answers in lower case.
func replaceUnlessSameID() planmodifier.String {
	return stringplanmodifier.RequiresReplaceIf(
		func(_ context.Context, req planmodifier.StringRequest, res *stringplanmodifier.RequiresReplaceIfFuncResponse) {
			if req.StateValue.IsNull() || req.PlanValue.IsUnknown() {
				res.RequiresReplace = !req.StateValue.IsNull()
				return
			}
			res.RequiresReplace = !strings.EqualFold(req.StateValue.ValueString(), req.PlanValue.ValueString())
		},
		"Replaces the resource when the id changes, ignoring case.",
		"Replaces the resource when the id changes, ignoring case.",
	)
}
