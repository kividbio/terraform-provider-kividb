package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	usersPath    = "/instances/i-1/acl-users"
	userPath     = "/instances/i-1/acl-users/reporting"
	userListJSON = `{"users":[{"id":"u-1","username":"reporting","profile":"read_only","created_at":"2026-10-25T10:00:00.000Z"}]}`
	generatedPwd = "0123456789abcdef0123456789abcdef0123456789abcdef"
)

func testUserResource(f *fakeAPI) *databaseUserResource {
	return &databaseUserResource{client: f.client(), notRunningTimeout: time.Second, notRunningInterval: time.Millisecond}
}

func userState(t *testing.T, s tfsdk.State) databaseUserModel {
	t.Helper()
	var m databaseUserModel
	failOn(t, s.Get(context.Background(), &m))
	return m
}

func TestDatabaseUserCreateWithAGeneratedPassword(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("POST "+usersPath, 201, `{"id":"u-1","username":"reporting","profile":"read_only","password":"`+generatedPwd+`","job_id":"j-1"}`)
	f.answer("GET "+usersPath, 200, userListJSON)

	res := dsDoCreate(t, testUserResource(f), map[string]tftypes.Value{
		"instance_id": dsStr("i-1"), "username": dsStr("reporting"), "profile": dsStr("read_only"),
	})
	failOn(t, res.Diagnostics)

	sent := f.sent("POST " + usersPath)
	if len(sent) != 1 || sent[0].Body["username"] != "reporting" || sent[0].Body["profile"] != "read_only" {
		t.Fatalf("create body wrong: %+v", sent)
	}
	if _, has := sent[0].Body["password"]; has {
		t.Errorf("no password was configured, so none may be sent: %v", sent[0].Body)
	}
	got := userState(t, res.State)
	if got.ID.ValueString() != "i-1/reporting" {
		t.Errorf("id = %v", got.ID)
	}
	if got.Password.ValueString() != generatedPwd {
		t.Errorf("the generated password must be kept in state: %v", got.Password)
	}
	if got.CreatedAt.ValueString() != "2026-10-25T10:00:00.000Z" {
		t.Errorf("created_at = %v", got.CreatedAt)
	}
}

func TestDatabaseUserCreateWithAChosenPassword(t *testing.T) {
	f := newFakeAPI(t)
	f.on("POST "+usersPath, func(c apiCall) apiReply {
		return apiReply{201, `{"id":"u-1","username":"reporting","profile":"read_write","password":"` + c.Body["password"].(string) + `"}`}
	})
	f.answer("GET "+usersPath, 200, userListJSON)

	res := dsDoCreate(t, testUserResource(f), map[string]tftypes.Value{
		"instance_id": dsStr("i-1"), "username": dsStr("reporting"), "profile": dsStr("read_write"),
		"password": dsStr("a-chosen-password-1"),
	})
	failOn(t, res.Diagnostics)
	if sent := f.sent("POST " + usersPath); sent[0].Body["password"] != "a-chosen-password-1" {
		t.Errorf("the chosen password was not sent: %v", sent[0].Body)
	}
	if got := userState(t, res.State); got.Password.ValueString() != "a-chosen-password-1" {
		t.Errorf("password = %v", got.Password)
	}
}

// A database created in the same apply is not running for a moment after its
// own resource returns. The write waits for it rather than failing.
func TestDatabaseUserCreateWaitsForTheDatabaseToRun(t *testing.T) {
	f := newFakeAPI(t)
	attempts := 0
	f.on("POST "+usersPath, func(apiCall) apiReply {
		attempts++
		if attempts < 3 {
			return apiReply{409, `{"error":{"code":"instance_not_running"}}`}
		}
		return apiReply{201, `{"id":"u-1","username":"reporting","profile":"read_only","password":"` + generatedPwd + `"}`}
	})
	f.answer("GET "+usersPath, 200, userListJSON)

	res := dsDoCreate(t, testUserResource(f), map[string]tftypes.Value{
		"instance_id": dsStr("i-1"), "username": dsStr("reporting"), "profile": dsStr("read_only"),
	})
	failOn(t, res.Diagnostics)
	if attempts != 3 {
		t.Errorf("expected two refusals then a create, got %d attempts", attempts)
	}
}

// The wait is bounded: a database that never runs fails the apply, saying why.
func TestDatabaseUserWaitIsBounded(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("POST "+usersPath, 409, `{"error":{"code":"instance_not_running"}}`)
	r := testUserResource(f)
	r.notRunningTimeout = 20 * time.Millisecond

	res := dsDoCreate(t, r, map[string]tftypes.Value{
		"instance_id": dsStr("i-1"), "username": dsStr("reporting"), "profile": dsStr("read_only"),
	})
	wantError(t, res.Diagnostics, "not running", "after 20ms")
}

func TestDatabaseUserRefusalsAreExplained(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       []string
	}{
		// The ACL routes answer bare codes, which are given words.
		{"taken", `{"error":{"code":"username_taken"}}`, 409, []string{"already has a user with that name", "terraform import"}},
		// A refusal the server explained is passed on in its own words.
		{"api key", `{"error":{"code":"unauthorized","message":"This endpoint does not accept API keys yet. Use a session token."}}`, 401,
			[]string{"This endpoint does not accept API keys yet. Use a session token."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.answer("POST "+usersPath, tc.status, tc.body)
			res := dsDoCreate(t, testUserResource(f), map[string]tftypes.Value{
				"instance_id": dsStr("i-1"), "username": dsStr("reporting"), "profile": dsStr("read_only"),
			})
			wantError(t, res.Diagnostics, tc.want...)
		})
	}
}

func userStateVals() map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"id": dsStr("i-1/reporting"), "instance_id": dsStr("i-1"), "username": dsStr("reporting"),
		"profile": dsStr("read_only"), "password": dsStr(generatedPwd), "created_at": dsStr("2026-10-25T10:00:00.000Z"),
	}
}

// A password changed in the console is read back, so the next plan sees it.
func TestDatabaseUserReadSeesTheCurrentPassword(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET "+usersPath, 200, strings.Replace(userListJSON, "read_only", "pubsub", 1))
	f.answer("GET "+userPath+"/password", 200, `{"username":"reporting","password":"changed-in-the-console"}`)

	res := dsDoRead(t, testUserResource(f), userStateVals())
	failOn(t, res.Diagnostics)
	got := userState(t, res.State)
	if got.Password.ValueString() != "changed-in-the-console" || got.Profile.ValueString() != "pubsub" {
		t.Errorf("read did not refresh: password=%v profile=%v", got.Password, got.Profile)
	}
}

func TestDatabaseUserGoneIsRemovedFromState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"user deleted", 200, `{"users":[]}`},
		{"database deleted", 404, `{"error":{"code":"not_found"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.answer("GET "+usersPath, tc.status, tc.body)
			res := dsDoRead(t, testUserResource(f), userStateVals())
			failOn(t, res.Diagnostics)
			if !res.State.Raw.IsNull() {
				t.Errorf("a user that is gone must be removed from state")
			}
		})
	}
}

// A key that may list users but not read their passwords keeps the password in
// state rather than failing every refresh.
func TestDatabaseUserReadKeepsThePasswordWhenItCannotBeRead(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET "+usersPath, 200, userListJSON)
	f.answer("GET "+userPath+"/password", 403, `{"error":{"code":"forbidden","message":"no"}}`)
	res := dsDoRead(t, testUserResource(f), userStateVals())
	failOn(t, res.Diagnostics)
	if got := userState(t, res.State); got.Password.ValueString() != generatedPwd {
		t.Errorf("password = %v", got.Password)
	}
}

func TestDatabaseUserUpdate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		plan     map[string]tftypes.Value
		wantBody map[string]any
		reply    string
		wantPwd  string
	}{
		{
			name:     "profile only",
			plan:     map[string]tftypes.Value{"profile": dsStr("read_write"), "password": dsStr(generatedPwd)},
			wantBody: map[string]any{"profile": "read_write"},
			reply:    `{"ok":true,"username":"reporting","profile":"read_write"}`,
			wantPwd:  generatedPwd,
		},
		{
			name:     "a chosen password",
			plan:     map[string]tftypes.Value{"profile": dsStr("read_only"), "password": dsStr("a-new-chosen-password")},
			wantBody: map[string]any{"password": "a-new-chosen-password"},
			reply:    `{"ok":true,"username":"reporting","profile":"read_only","password":"a-new-chosen-password"}`,
			wantPwd:  "a-new-chosen-password",
		},
		{
			// The rotation trigger changed, so the password was planned unknown.
			name: "rotation",
			plan: map[string]tftypes.Value{"profile": dsStr("read_only"), "rotation_trigger": dsStr("2026-11"),
				"password": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)},
			wantBody: map[string]any{"rotate_password": true},
			reply:    `{"ok":true,"username":"reporting","profile":"read_only","password":"fresh-from-the-api-000000"}`,
			wantPwd:  "fresh-from-the-api-000000",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.answer("PATCH "+userPath, 202, tc.reply)
			plan := userStateVals()
			delete(plan, "id")
			for k, v := range tc.plan {
				plan[k] = v
			}
			res := dsDoUpdate(t, testUserResource(f), userStateVals(), plan)
			failOn(t, res.Diagnostics)
			sent := f.sent("PATCH " + userPath)
			if len(sent) != 1 || len(sent[0].Body) != len(tc.wantBody) {
				t.Fatalf("PATCH body = %+v, want %v", sent, tc.wantBody)
			}
			for k, v := range tc.wantBody {
				if sent[0].Body[k] != v {
					t.Errorf("PATCH %s = %v, want %v", k, sent[0].Body[k], v)
				}
			}
			if got := userState(t, res.State); got.Password.ValueString() != tc.wantPwd || got.ID.ValueString() != "i-1/reporting" {
				t.Errorf("state after update: password=%v id=%v", got.Password, got.ID)
			}
		})
	}
}

func TestDatabaseUserUpdateRefusalIsSurfaced(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("PATCH "+userPath, 400, `{"error":{"code":"invalid_body","message":"password: String must contain at least 12 character(s)"}}`)
	plan := userStateVals()
	plan["password"] = dsStr("short-but-validated-elsewhere")
	res := dsDoUpdate(t, testUserResource(f), userStateVals(), plan)
	wantError(t, res.Diagnostics, "password: String must contain at least 12 character(s)")
}

func TestDatabaseUserDelete(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("DELETE "+userPath, 202, `{"ok":true,"job_id":"j-2"}`)
	failOn(t, dsDoDelete(t, testUserResource(f), userStateVals()).Diagnostics)
	if len(f.sent("DELETE "+userPath)) != 1 {
		t.Error("the user was not deleted")
	}

	// Already gone is not a failure.
	g := newFakeAPI(t)
	g.answer("DELETE "+userPath, 404, `{"error":{"code":"not_found"}}`)
	failOn(t, dsDoDelete(t, testUserResource(g), userStateVals()).Diagnostics)
}

func TestDatabaseUserImport(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET "+usersPath, 200, userListJSON)
	f.answer("GET "+userPath+"/password", 200, `{"username":"reporting","password":"`+generatedPwd+`"}`)

	res := dsDoImport(t, testUserResource(f), "i-1/reporting")
	failOn(t, res.Diagnostics)
	var got databaseUserModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.ID.ValueString() != "i-1/reporting" || got.Profile.ValueString() != "read_only" ||
		got.Password.ValueString() != generatedPwd || got.InstanceID.ValueString() != "i-1" {
		t.Errorf("import state wrong: %+v", got)
	}

	for _, bad := range []string{"i-1", "i-1/", "/reporting", "i-1/a/b"} {
		wantError(t, dsDoImport(t, testUserResource(f), bad).Diagnostics, "<instance_id>/<username>")
	}

	g := newFakeAPI(t)
	g.answer("GET "+usersPath, 200, `{"users":[]}`)
	wantError(t, dsDoImport(t, testUserResource(g), "i-1/reporting").Diagnostics, "has no user \"reporting\"")
}

func TestDatabaseUserSchemaMirrorsTheAPI(t *testing.T) {
	sch := dsResourceSchema(t, NewDatabaseUserResource())
	if !sch.Attributes["password"].IsSensitive() {
		t.Error("password must be sensitive")
	}
	user := sch.Attributes["username"].(rschema.StringAttribute).Validators
	for _, bad := range []string{"a", "Reporting", "1st", "kividbapp", "has-dash", strings.Repeat("a", 33)} {
		if d := runStringValidators(user, bad); !d.HasError() {
			t.Errorf("username %q must be refused", bad)
		}
	}
	for _, good := range []string{"ab", "reporting", "app_2"} {
		if d := runStringValidators(user, good); d.HasError() {
			t.Errorf("username %q must be accepted: %v", good, d)
		}
	}
}

// A generated password is kept until the rotation trigger changes.
func TestGeneratedPasswordPlanning(t *testing.T) {
	ctx := context.Background()
	sch := dsResourceSchema(t, NewDatabaseUserResource())
	prior := userStateVals()
	prior["rotation_trigger"] = dsStr("2026-10")
	state := tfsdk.State{Schema: sch, Raw: stored(t, sch, prior)}

	for _, tc := range []struct {
		trigger     string
		wantUnknown bool
	}{{"2026-10", false}, {"2026-11", true}} {
		vals := userStateVals()
		vals["rotation_trigger"] = dsStr(tc.trigger)
		vals["password"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
		plan := tfsdk.Plan{Schema: sch, Raw: planned(t, sch, vals)}
		req := planmodifier.StringRequest{
			ConfigValue: types.StringNull(), PlanValue: types.StringUnknown(),
			StateValue: types.StringValue(generatedPwd), Plan: plan, State: state,
		}
		res := planmodifier.StringResponse{PlanValue: req.PlanValue}
		generatedPassword().PlanModifyString(ctx, req, &res)
		failOn(t, res.Diagnostics)
		if res.PlanValue.IsUnknown() != tc.wantUnknown {
			t.Errorf("trigger %s: planned %v", tc.trigger, res.PlanValue)
		}
		if !tc.wantUnknown && res.PlanValue.ValueString() != generatedPwd {
			t.Errorf("an unchanged trigger must keep the password: %v", res.PlanValue)
		}
	}
}

var _ resource.ResourceWithImportState = (*databaseUserResource)(nil)
