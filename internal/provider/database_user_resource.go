package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*databaseUserResource)(nil)
	_ resource.ResourceWithConfigure   = (*databaseUserResource)(nil)
	_ resource.ResourceWithImportState = (*databaseUserResource)(nil)
)

func NewDatabaseUserResource() resource.Resource {
	return &databaseUserResource{notRunningTimeout: 15 * time.Minute, notRunningInterval: 10 * time.Second}
}

type databaseUserResource struct {
	client *Client

	// How long a write waits for a database that is not running yet. Fields so
	// a test can shorten them.
	notRunningTimeout  time.Duration
	notRunningInterval time.Duration
}

type databaseUserModel struct {
	ID              types.String `tfsdk:"id"`
	InstanceID      types.String `tfsdk:"instance_id"`
	Username        types.String `tfsdk:"username"`
	Profile         types.String `tfsdk:"profile"`
	Password        types.String `tfsdk:"password"`
	RotationTrigger types.String `tfsdk:"rotation_trigger"`
	CreatedAt       types.String `tfsdk:"created_at"`
}

// The API's username rule, and the names it keeps for itself.
var aclUsernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var reservedACLUsernames = []string{"kividbapp", "kividb_system", "kividb_internal"}

func (r *databaseUserResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_database_user"
}

func (r *databaseUserResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: `A user on one database, with its own password and a permission profile.

Every database comes with a default user (` + "`kividbapp`" + `), whose password is in the
console. Users made here are in addition to it, so an application can be given
only what it needs -- read-only for a reporting job, publish/subscribe for a
notifier -- and its password changed or revoked without touching anyone else's.

### Passwords

Set ` + "`password`" + ` to choose one, or leave it out and the API generates one (48
hexadecimal characters). Either way it is stored in Terraform state, so treat
the state as a secret. To change a generated password, change
` + "`rotation_trigger`" + ` to any new value; to change a chosen one, change ` + "`password`" + `.
A password changed in the console shows up as drift on the next plan: a chosen
one is set back, a generated one is read into state.

### When it takes effect

The API stores the user at once and applies it to the database's nodes in the
background, normally within a few seconds of the apply finishing. Users can
only be added, changed or removed while the database is running; an apply that
reaches a database still being created or changed waits for it, for up to 15
minutes.

Deleting this resource deletes the user, and connections using it are refused
from then on.`,
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<instance_id>/<username>`, which is also the import id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database the user belongs to. **Changing this creates the user on the other database and deletes it here.**",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"username": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "2-32 characters: lower-case letters, digits and underscores, starting with a " +
					"letter. `kividbapp`, `kividb_system` and `kividb_internal` are KiviDB's own. A user cannot be " +
					"renamed, so **changing this replaces the user**, and its password with it.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(2, 32),
					stringvalidator.RegexMatches(aclUsernamePattern, "must be lower-case letters, digits and underscores, starting with a letter"),
					stringvalidator.NoneOf(reservedACLUsernames...),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"profile": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("read_write"),
				MarkdownDescription: "What the user may do: `read_write` (every data command), `read_only` (reads " +
					"only) or `pubsub` (publish and subscribe only). Changed in place.",
				Validators: []validator.String{stringvalidator.OneOf("read_only", "read_write", "pubsub")},
			},
			"password": schema.StringAttribute{
				Optional:  true,
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The user's password, 12-128 characters. Leave it out to have the API generate " +
					"one, which is then readable here. Changed in place.",
				Validators:    []validator.String{stringvalidator.LengthBetween(12, 128)},
				PlanModifiers: []planmodifier.String{generatedPassword()},
			},
			"rotation_trigger": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Any value; changing it has the API generate a new password. Only used while " +
					"`password` is not set -- a chosen password changes when `password` does. A date, or a " +
					"`time_rotating` resource's id, makes a regular rotation.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the user was created.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// generatedPassword plans a password left out of the configuration: the one in
// state, unless there is none yet or a rotation was asked for, in which case
// the API will generate one and it is unknown until then.
type generatedPasswordModifier struct{}

func generatedPassword() planmodifier.String { return generatedPasswordModifier{} }

func (generatedPasswordModifier) Description(_ context.Context) string {
	return "Kept from state while not configured, unless rotation_trigger changes."
}

func (m generatedPasswordModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (generatedPasswordModifier) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, res *planmodifier.StringResponse) {
	if !req.ConfigValue.IsNull() || req.Plan.Raw.IsNull() || req.State.Raw.IsNull() || req.StateValue.IsNull() {
		return // configured, destroyed, or created: the framework's own planning is right
	}
	var planned, prior types.String
	res.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("rotation_trigger"), &planned)...)
	res.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("rotation_trigger"), &prior)...)
	if !planned.Equal(prior) {
		res.PlanValue = types.StringUnknown()
		return
	}
	res.PlanValue = req.StateValue
}

func (r *databaseUserResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		res.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	r.client = client
}

func (r *databaseUserResource) whenRunning(ctx context.Context, op func() error) error {
	return retryWhileNotRunning(ctx, r.notRunningTimeout, r.notRunningInterval, op)
}

func (r *databaseUserResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan databaseUserModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	instanceID := plan.InstanceID.ValueString()

	body := CreateDatabaseUserRequest{
		Username: plan.Username.ValueString(),
		Profile:  plan.Profile.ValueString(),
	}
	if !plan.Password.IsNull() && !plan.Password.IsUnknown() {
		v := plan.Password.ValueString()
		body.Password = &v
	}

	var user *DatabaseUser
	var password string
	err := r.whenRunning(ctx, func() error {
		var err error
		user, password, err = r.client.CreateDatabaseUser(ctx, instanceID, body)
		return err
	})
	if err != nil {
		res.Diagnostics.AddError("Could not create the database user", explainDatabaseUser(err))
		return
	}

	plan.ID = types.StringValue(instanceID + "/" + user.Username)
	plan.Profile = types.StringValue(user.Profile)
	plan.Password = types.StringValue(password)
	plan.CreatedAt = types.StringNull()
	// The create answers without a timestamp; the list has it. Not reading it
	// is not a reason to lose a user that exists, so a failure leaves it null
	// for the next refresh.
	if listed, err := r.client.GetDatabaseUser(ctx, instanceID, user.Username); err == nil {
		plan.CreatedAt = types.StringValue(listed.CreatedAt)
	}
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *databaseUserResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state databaseUserModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	instanceID, username := state.InstanceID.ValueString(), state.Username.ValueString()
	if instanceID == "" || username == "" {
		res.State.RemoveResource(ctx)
		return
	}

	gone, diags := r.refresh(ctx, &state)
	res.Diagnostics.Append(diags...)
	if gone {
		// Deleted in the console, or the database is gone and took its users
		// with it. Either way the next plan creates it again.
		res.State.RemoveResource(ctx)
		return
	}
	if res.Diagnostics.HasError() {
		return
	}
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// refresh reads the user and its password into m. It reports gone when the
// user or its database no longer exists.
func (r *databaseUserResource) refresh(ctx context.Context, m *databaseUserModel) (gone bool, diags diag.Diagnostics) {
	instanceID, username := m.InstanceID.ValueString(), m.Username.ValueString()
	user, err := r.client.GetDatabaseUser(ctx, instanceID, username)
	if err != nil {
		if isNotFound(err) {
			return true, nil
		}
		diags.AddError("Could not read the database user", explainDatabaseUser(err))
		return false, diags
	}
	m.ID = types.StringValue(instanceID + "/" + user.Username)
	m.Profile = types.StringValue(user.Profile)
	m.CreatedAt = types.StringValue(user.CreatedAt)

	// Read so a password changed in the console is seen. A key whose role may
	// see users but not their passwords keeps the one in state rather than
	// failing every plan.
	password, err := r.client.DatabaseUserPassword(ctx, instanceID, username)
	switch {
	case err == nil:
		m.Password = types.StringValue(password)
	case isNotFound(err):
		return true, nil
	case isForbidden(err):
	default:
		diags.AddError("Could not read the database user's password", explainDatabaseUser(err))
	}
	return false, diags
}

func (r *databaseUserResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan, state databaseUserModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	instanceID, username := state.InstanceID.ValueString(), state.Username.ValueString()

	var body UpdateDatabaseUserRequest
	if !plan.Profile.Equal(state.Profile) && !plan.Profile.IsUnknown() {
		v := plan.Profile.ValueString()
		body.Profile = &v
	}
	switch {
	case plan.Password.IsUnknown():
		// Left to the API, and planned unknown: the rotation trigger changed.
		rotate := true
		body.RotatePassword = &rotate
	case !plan.Password.Equal(state.Password):
		v := plan.Password.ValueString()
		body.Password = &v
	}

	password := state.Password.ValueString()
	if body.Profile != nil || body.Password != nil || body.RotatePassword != nil {
		err := r.whenRunning(ctx, func() error {
			p, err := r.client.UpdateDatabaseUser(ctx, instanceID, username, body)
			if p != "" {
				password = p
			}
			return err
		})
		if err != nil {
			res.Diagnostics.AddError("Could not change the database user", explainDatabaseUser(err))
			return
		}
	}

	plan.ID = types.StringValue(instanceID + "/" + username)
	plan.Password = types.StringValue(password)
	plan.CreatedAt = state.CreatedAt
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *databaseUserResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state databaseUserModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	err := r.whenRunning(ctx, func() error {
		return r.client.DeleteDatabaseUser(ctx, state.InstanceID.ValueString(), state.Username.ValueString())
	})
	if err != nil && !isNotFound(err) {
		res.Diagnostics.AddError("Could not delete the database user", explainDatabaseUser(err))
	}
}

func (r *databaseUserResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	instanceID, username, ok := strings.Cut(req.ID, "/")
	if !ok || instanceID == "" || username == "" || strings.Contains(username, "/") {
		res.Diagnostics.AddError("Not a database user id",
			fmt.Sprintf("Import a database user as `<instance_id>/<username>`, for example "+
				"`790df3ae-c064-45f5-b924-9ed89471fd72/reporting`. Got %q.", req.ID))
		return
	}
	state := databaseUserModel{
		InstanceID:      types.StringValue(instanceID),
		Username:        types.StringValue(username),
		Password:        types.StringNull(),
		RotationTrigger: types.StringNull(),
	}
	gone, diags := r.refresh(ctx, &state)
	res.Diagnostics.Append(diags...)
	if gone {
		res.Diagnostics.AddError("No such database user",
			fmt.Sprintf("Database %s has no user %q, or the database does not exist in this organization.", instanceID, username))
		return
	}
	if res.Diagnostics.HasError() {
		return
	}
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// explainDatabaseUser adds what the ACL routes' bare codes mean. They answer
// with a code and no message, so without this a practitioner would read
// "username_taken (HTTP 409)".
func explainDatabaseUser(err error) string {
	// Asserted, not unwrapped: a refusal wrapped by the wait for a running
	// database already says what happened, and how long it waited.
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Message != "" {
		return explain(err)
	}
	switch apiErr.Code {
	case "username_taken":
		return "This database already has a user with that name. Import it with " +
			"`terraform import <address> <instance_id>/<username>`, or choose another name."
	case "reserved_username":
		return "That username is one of KiviDB's own (kividbapp, kividb_system, kividb_internal). Choose another."
	case "instance_not_running", "instance_not_ready":
		return "The database is not running, and users can only be changed while it is. " + apiErr.Error()
	default:
		return explain(err)
	}
}
