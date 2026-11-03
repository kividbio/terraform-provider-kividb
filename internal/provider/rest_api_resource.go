package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*restAPIResource)(nil)
	_ resource.ResourceWithConfigure   = (*restAPIResource)(nil)
	_ resource.ResourceWithImportState = (*restAPIResource)(nil)
)

func NewRestAPIResource() resource.Resource { return &restAPIResource{} }

type restAPIResource struct {
	client *Client
	// How often to look while waiting for the endpoint; a test shortens it.
	poll time.Duration
}

type restAPIModel struct {
	ID             types.String `tfsdk:"id"`
	InstanceID     types.String `tfsdk:"instance_id"`
	WaitUntilReady types.Bool   `tfsdk:"wait_until_ready"`
	URL            types.String `tfsdk:"url"`
	Status         types.String `tfsdk:"status"`
}

// restReadyTimeout bounds the wait for a certificate and the push to the
// nodes, which takes one to two minutes on every cloud (2026-11-03).
const restReadyTimeout = 15 * time.Minute

func (r *restAPIResource) Metadata(_ context.Context, req resource.MetadataRequest, res *resource.MetadataResponse) {
	res.TypeName = req.ProviderTypeName + "_rest_api"
}

func (r *restAPIResource) Schema(_ context.Context, _ resource.SchemaRequest, res *resource.SchemaResponse) {
	res.Schema = schema.Schema{
		MarkdownDescription: `A database's REST API: an HTTPS endpoint at ` + "`https://<database hostname>`" + `,
compatible with Upstash's REST API. An app on the ` + "`@upstash/redis`" + ` SDK switches by
setting ` + "`UPSTASH_REDIS_REST_URL`" + ` to ` + "`url`" + ` and ` + "`UPSTASH_REDIS_REST_TOKEN`" + ` to a token.

A token is ` + "`kvr_`" + ` followed by the base64url (no padding) of ` + "`username:password`" + ` for a
database user (` + "`kividb_database_user`" + `), so the user's profile decides what it may
run: a ` + "`read_only`" + ` user gives a read-only token. Changing the user's password
revokes its token within 30 seconds.

Creating this resource turns the endpoint on: KiviDB issues a public
certificate for the database's hostname and opens port 443 to the database's
allow-list. Deleting it turns the endpoint off and closes the port. Available
on dedicated databases that are not sharded.`,
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The database's id: a database has one REST API.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The database. **Changing this turns the old database's endpoint off and the new one's on.**",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"wait_until_ready": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Wait for the certificate and the nodes, so `url` is set when the apply finishes (usually one to two minutes; at most 15). Defaults to `true`.",
			},
			"url": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The endpoint, `https://<database hostname>`, once it serves. Empty while it is being set up.",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`issuing` while the certificate is issued and pushed, `ready` once the database serves it, `unavailable` if that failed.",
			},
		},
	}
}

func (r *restAPIResource) Configure(_ context.Context, req resource.ConfigureRequest, res *resource.ConfigureResponse) {
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

func (m *restAPIModel) apply(instanceID string, v *RestAPI) {
	m.ID = types.StringValue(instanceID)
	m.InstanceID = types.StringValue(instanceID)
	m.Status = types.StringValue(v.Status)
	if v.URL != nil {
		m.URL = types.StringValue(*v.URL)
	} else {
		m.URL = types.StringValue("")
	}
}

// waitReady polls until the endpoint serves, fails, or the timeout passes.
func (r *restAPIResource) waitReady(ctx context.Context, id string) (*RestAPI, error) {
	poll := r.poll
	if poll == 0 {
		poll = 10 * time.Second
	}
	deadline := time.Now().Add(restReadyTimeout)
	for {
		v, err := r.client.GetRestAPI(ctx, id)
		if err != nil {
			return nil, err
		}
		switch v.Status {
		case "ready":
			return v, nil
		case "unavailable":
			reason := "no reason given"
			if v.Error != nil {
				reason = *v.Error
			}
			return nil, fmt.Errorf("the REST API is on but not serving: %s", reason)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the REST API was not ready after %s (status %q)", restReadyTimeout, v.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (r *restAPIResource) Create(ctx context.Context, req resource.CreateRequest, res *resource.CreateResponse) {
	var plan restAPIModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := plan.InstanceID.ValueString()
	v, err := r.client.SetRestAPI(ctx, id, true)
	if err != nil {
		res.Diagnostics.AddError("Could not turn the REST API on", explain(err))
		return
	}
	// The default (true) when unset; state never holds an unknown.
	wait := plan.WaitUntilReady.IsNull() || plan.WaitUntilReady.IsUnknown() || plan.WaitUntilReady.ValueBool()
	plan.WaitUntilReady = types.BoolValue(wait)
	if wait && v.Status != "ready" {
		if v, err = r.waitReady(ctx, id); err != nil {
			// Saved anyway: the endpoint is on, and a later apply or a
			// destroy must be able to see and turn it off.
			plan.apply(id, &RestAPI{Enabled: true, Status: "unavailable"})
			res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
			res.Diagnostics.AddError("The REST API did not become ready", explain(err))
			return
		}
	}
	plan.apply(id, v)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *restAPIResource) Read(ctx context.Context, req resource.ReadRequest, res *resource.ReadResponse) {
	var state restAPIModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	id := state.InstanceID.ValueString()
	v, err := r.client.GetRestAPI(ctx, id)
	if err != nil {
		if isNotFound(err) {
			res.State.RemoveResource(ctx)
			return
		}
		res.Diagnostics.AddError("Could not read the REST API", explain(err))
		return
	}
	if !v.Enabled {
		// Turned off elsewhere (console, API): the next plan turns it back on.
		res.State.RemoveResource(ctx)
		return
	}
	state.apply(id, v)
	res.Diagnostics.Append(res.State.Set(ctx, &state)...)
}

// Update only changes wait_until_ready, which does nothing to the database.
func (r *restAPIResource) Update(ctx context.Context, req resource.UpdateRequest, res *resource.UpdateResponse) {
	var plan restAPIModel
	res.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if res.Diagnostics.HasError() {
		return
	}
	v, err := r.client.GetRestAPI(ctx, plan.InstanceID.ValueString())
	if err != nil {
		res.Diagnostics.AddError("Could not read the REST API", explain(err))
		return
	}
	plan.apply(plan.InstanceID.ValueString(), v)
	res.Diagnostics.Append(res.State.Set(ctx, &plan)...)
}

func (r *restAPIResource) Delete(ctx context.Context, req resource.DeleteRequest, res *resource.DeleteResponse) {
	var state restAPIModel
	res.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if res.Diagnostics.HasError() {
		return
	}
	if _, err := r.client.SetRestAPI(ctx, state.InstanceID.ValueString(), false); err != nil && !isNotFound(err) {
		res.Diagnostics.AddError("Could not turn the REST API off", explain(err))
	}
}

func (r *restAPIResource) ImportState(ctx context.Context, req resource.ImportStateRequest, res *resource.ImportStateResponse) {
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("instance_id"), req.ID)...)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	res.Diagnostics.Append(res.State.SetAttribute(ctx, path.Root("wait_until_ready"), true)...)
}
