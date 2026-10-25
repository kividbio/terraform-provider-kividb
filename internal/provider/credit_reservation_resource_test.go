package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// rvFake is apps/api/src/routes/credit-reservations.ts with its arithmetic:
// the balance, reservations drawn down by consumed_cents, a running
// database's day as ceil(monthly / 30), the two refusals of checkRoom, and the
// preview's release_reservation_id and add_to_reservation_id.
type rvFake struct {
	t         *testing.T
	mu        sync.Mutex
	balance   int64
	instances map[string]*rvFakeInstance
	order     []string // instance ids, for stable output
	rows      []*rvFakeRow
	next      int
	calls     []string
	previews  []map[string]any
	refuse    map[string]rvRefusal
}

type rvRefusal struct {
	status int
	body   string
}

type rvFakeInstance struct {
	name     string
	monthly  int64
	running  bool
	freeTier bool
	deleted  bool
}

type rvFakeRow struct {
	id, instanceID, name string
	amount, consumed     int64
	status, reason       string
}

func rvNewFake(t *testing.T, balance int64) (*rvFake, *httptest.Server) {
	f := &rvFake{t: t, balance: balance, instances: map[string]*rvFakeInstance{}, refuse: map[string]rvRefusal{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func rvID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func (f *rvFake) addInstance(id, name string, monthly int64) {
	f.instances[id] = &rvFakeInstance{name: name, monthly: monthly, running: true}
	f.order = append(f.order, id)
}

// reserve puts a reservation straight in, as the console would have.
func (f *rvFake) reserve(instanceID string, amount int64) string {
	f.next++
	id := rvID(900 + f.next)
	f.rows = append(f.rows, &rvFakeRow{id: id, instanceID: instanceID, name: f.instances[instanceID].name, amount: amount, status: "active"})
	return id
}

func rvUsdJS(c int64) string { return fmt.Sprintf("$%.2f", float64(c)/100) }

func (f *rvFake) reservedByInstance() map[string]int64 {
	out := map[string]int64{}
	for _, r := range f.rows {
		if r.status == "active" && r.instanceID != "" && r.consumed < r.amount {
			out[r.instanceID] = r.amount - r.consumed
		}
	}
	return out
}

func rvSum(m map[string]int64) (s int64) {
	for _, v := range m {
		s += v
	}
	return s
}

func (f *rvFake) daily(id string) int64 {
	return int64(math.Ceil(float64(f.instances[id].monthly) / 30))
}

func (f *rvFake) checkRoom(forInstance string, add int64) string {
	reserved := f.reservedByInstance()
	unreserved := f.balance - rvSum(reserved)
	if add > unreserved {
		return fmt.Sprintf("Only %s of your balance is unreserved; add credits to reserve %s.", rvUsdJS(max(0, unreserved)), rvUsdJS(add))
	}
	var day int64
	var n int
	for _, id := range f.order {
		in := f.instances[id]
		if id == forInstance || !in.running || in.freeTier || in.deleted {
			continue
		}
		if _, ok := reserved[id]; ok {
			continue
		}
		day += f.daily(id)
		n++
	}
	if day > 0 && unreserved-add < day {
		return fmt.Sprintf("That would leave %s for your %d running database(s) without a reservation, less than a day of their usage (%s), and they would be stopped. Add credits first, or reserve less.",
			rvUsdJS(unreserved-add), n, rvUsdJS(day))
	}
	return ""
}

func (f *rvFake) amountFor(instanceID string, b map[string]any) int64 {
	if v, ok := b["amount_cents"].(float64); ok {
		return int64(v)
	}
	m, _ := b["months"].(float64)
	return f.instances[instanceID].monthly * int64(m)
}

func (f *rvFake) find(id string) *rvFakeRow {
	for _, r := range f.rows {
		if strings.EqualFold(r.id, id) {
			return r
		}
	}
	return nil
}

func (f *rvFake) view(r *rvFakeRow) map[string]any {
	v := map[string]any{
		"id": r.id, "amount_cents": r.amount, "consumed_cents": r.consumed, "remaining_cents": r.amount - r.consumed,
		"status": r.status, "instance_name": r.name, "created_at": "2026-10-25T10:00:00Z",
		"instance_id": nil, "instance_status": nil, "deactivated_reason": nil, "deactivated_at": nil,
	}
	if r.instanceID != "" {
		v["instance_id"] = r.instanceID
		v["instance_status"] = "running"
	}
	if r.status == "inactive" {
		v["deactivated_reason"] = r.reason
		v["deactivated_at"] = "2026-10-25T11:00:00Z"
	}
	return v
}

func (f *rvFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, call)
	w.Header().Set("Content-Type", "application/json")
	send := func(status int, v any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	refuse := func(status int, code, msg string) {
		send(status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
	}
	if ref, ok := f.refuse[call]; ok {
		w.WriteHeader(ref.status)
		_, _ = w.Write([]byte(ref.body))
		return
	}
	if r.URL.Path == "/instances" { // the provider's key check at configure
		send(200, map[string]any{"instances": []any{}})
		return
	}
	body := map[string]any{}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	rest := strings.TrimPrefix(r.URL.Path, "/credits/reservations")
	parts := strings.Split(strings.Trim(rest, "/"), "/")

	switch {
	case r.Method == http.MethodGet && rest == "":
		rows := []map[string]any{}
		sorted := append([]*rvFakeRow(nil), f.rows...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].status < sorted[j].status })
		var reserved int64
		for _, row := range sorted {
			rows = append(rows, f.view(row))
			if row.status == "active" {
				reserved += row.amount - row.consumed
			}
		}
		send(200, map[string]any{"balance_cents": f.balance, "reserved_cents": reserved, "unreserved_cents": f.balance - reserved, "reservations": rows})

	case r.Method == http.MethodPost && rest == "/preview":
		f.previews = append(f.previews, body)
		instID := strings.ToLower(body["instance_id"].(string))
		inst := f.instances[instID]
		if inst == nil || inst.deleted {
			refuse(404, "instance_not_found", "No such database in this organization.")
			return
		}
		addToID, _ := body["add_to_reservation_id"].(string)
		releaseID, _ := body["release_reservation_id"].(string)
		addTo, release := f.find(addToID), f.find(releaseID)
		if addToID != "" && (addTo == nil || addTo.status != "active" || addTo.instanceID != instID) {
			refuse(409, "not_active", "That is not an active reservation for this database.")
			return
		}
		if releaseID != "" && release == nil {
			refuse(404, "not_found", "No such reservation.")
			return
		}
		reservedNow := f.reservedByInstance()
		reservedAfter := map[string]int64{}
		for k, v := range reservedNow {
			reservedAfter[k] = v
		}
		if release != nil && release.status == "active" && release.instanceID != "" {
			delete(reservedAfter, release.instanceID)
		}
		amount := f.amountFor(instID, body)
		unreservedBefore := f.balance - rvSum(reservedNow)
		roomForIt := f.balance - rvSum(reservedAfter)
		unreservedAfter := roomForIt - amount
		var dayBefore, dayAfter int64
		var exposedAfter []string
		for _, id := range f.order {
			in := f.instances[id]
			if !in.running || in.freeTier || in.deleted {
				continue
			}
			if _, ok := reservedNow[id]; !ok {
				dayBefore += f.daily(id)
			}
			if _, ok := reservedAfter[id]; !ok && id != instID {
				dayAfter += f.daily(id)
				exposedAfter = append(exposedAfter, id)
			}
		}
		otherActive := false
		for _, row := range f.rows {
			if row.instanceID == instID && row.status == "active" && (release == nil || row.id != release.id) {
				otherActive = true
			}
		}
		var refusal map[string]string
		switch {
		case amount <= 0:
			refusal = map[string]string{"code": "nothing_to_reserve", "message": "This database has no price to reserve against; give amount_cents."}
		case addTo == nil && otherActive:
			refusal = map[string]string{"code": "already_reserved", "message": "This database already has an active reservation; add to it instead."}
		case amount > roomForIt:
			refusal = map[string]string{"code": "insufficient_unreserved_credits", "message": fmt.Sprintf(
				"Only %s of your balance is unreserved; add credits to reserve %s.", rvUsdJS(max(0, roomForIt)), rvUsdJS(amount))}
		case dayAfter > 0 && unreservedAfter < dayAfter:
			refusal = map[string]string{"code": "insufficient_unreserved_credits", "message": fmt.Sprintf(
				"That would leave %s for your %d running database(s) without a reservation, less than a day of their usage (%s), and they would be stopped. Add credits first, or reserve less.",
				rvUsdJS(unreservedAfter), len(exposedAfter), rvUsdJS(dayAfter))}
		}
		days := func(cents, perDay int64) any {
			if perDay <= 0 {
				return nil
			}
			return math.Max(0, math.Floor(float64(cents)/float64(perDay)*10)/10)
		}
		atRisk := []map[string]any{}
		for _, id := range exposedAfter {
			atRisk = append(atRisk, map[string]any{"instance_id": id, "name": f.instances[id].name, "daily_cents": f.daily(id)})
		}
		var ref any
		if refusal != nil {
			ref = refusal
		}
		send(200, map[string]any{
			"allowed": refusal == nil, "refusal": ref, "amount_cents": amount, "balance_cents": f.balance,
			"unreserved_before_cents": unreservedBefore, "unreserved_after_cents": unreservedAfter,
			"unreserved_daily_cents_before": dayBefore, "unreserved_daily_cents_after": dayAfter,
			"unreserved_runway_days_before": days(unreservedBefore, dayBefore),
			"unreserved_runway_days_after":  days(unreservedAfter, dayAfter),
			"at_risk":                       atRisk,
		})

	case r.Method == http.MethodPost && rest == "":
		instID := strings.ToLower(body["instance_id"].(string))
		if f.instances[instID] == nil {
			refuse(404, "instance_not_found", "No such database in this organization.")
			return
		}
		for _, row := range f.rows {
			if row.instanceID == instID && row.status == "active" {
				refuse(409, "already_reserved", "This database already has an active reservation; add to it instead.")
				return
			}
		}
		amount := f.amountFor(instID, body)
		if why := f.checkRoom(instID, amount); why != "" {
			refuse(402, "insufficient_unreserved_credits", why)
			return
		}
		f.next++
		row := &rvFakeRow{id: rvID(f.next), instanceID: instID, name: f.instances[instID].name, amount: amount, status: "active"}
		f.rows = append(f.rows, row)
		send(201, map[string]any{"reservation": f.view(row)})

	case r.Method == http.MethodPost && len(parts) == 2:
		row := f.find(parts[0])
		if row == nil {
			refuse(404, "not_found", "No such reservation.")
			return
		}
		switch parts[1] {
		case "add":
			if row.status != "active" {
				refuse(409, "not_active", "Activate the reservation before adding to it.")
				return
			}
			add := f.amountFor(row.instanceID, body)
			if why := f.checkRoom(row.instanceID, add); why != "" {
				refuse(402, "insufficient_unreserved_credits", why)
				return
			}
			row.amount += add
		case "deactivate":
			if row.status != "active" {
				refuse(404, "not_found", "No active reservation with that id.")
				return
			}
			row.status, row.reason = "inactive", "user"
		case "activate":
			if row.status != "active" {
				if row.instanceID == "" {
					refuse(409, "instance_deleted", "Its database was deleted; delete the reservation instead.")
					return
				}
				if why := f.checkRoom(row.instanceID, row.amount-row.consumed); why != "" {
					refuse(402, "insufficient_unreserved_credits", why)
					return
				}
				row.status, row.reason = "active", ""
			}
		}
		send(200, map[string]any{"reservation": f.view(row)})

	case r.Method == http.MethodDelete && len(parts) == 1:
		row := f.find(parts[0])
		if row == nil {
			refuse(404, "not_found", "No such reservation.")
			return
		}
		if row.status == "active" {
			refuse(409, "still_active", "Deactivate the reservation before deleting it.")
			return
		}
		for i := range f.rows {
			if f.rows[i] == row {
				f.rows = append(f.rows[:i], f.rows[i+1:]...)
				break
			}
		}
		w.WriteHeader(204)
	default:
		f.t.Errorf("unexpected %s", call)
		w.WriteHeader(500)
	}
}

func (f *rvFake) took(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (f *rvFake) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.previews = nil, nil
}

// --- framework plumbing ------------------------------------------------------

func rvSchema(t *testing.T) rschema.Schema {
	t.Helper()
	var res resource.SchemaResponse
	NewCreditReservationResource().Schema(context.Background(), resource.SchemaRequest{}, &res)
	failOn(t, res.Diagnostics)
	failOn(t, res.Schema.ValidateImplementation(context.Background()))
	return res.Schema
}

// rvPlanned is a plan for a new reservation: computed attributes unknown.
func rvPlanned(instanceID string, months, cents int64, active bool) rvModel {
	m := rvModel{
		ID: types.StringUnknown(), InstanceID: types.StringValue(instanceID),
		Months: types.Int64Null(), AmountCents: types.Int64Null(), Active: types.BoolValue(active),
		ReservedCents: types.Int64Unknown(), ConsumedCents: types.Int64Unknown(), RemainingCents: types.Int64Unknown(),
		Status: types.StringUnknown(), InstanceName: types.StringUnknown(), DeactivatedReason: types.StringUnknown(),
		DeactivatedAt: types.StringUnknown(), CreatedAt: types.StringUnknown(),
	}
	if months > 0 {
		m.Months = types.Int64Value(months)
	} else {
		m.AmountCents = types.Int64Value(cents)
	}
	return m
}

func rvPlanOf(t *testing.T, sch rschema.Schema, m rvModel) tfsdk.Plan {
	t.Helper()
	p := tfsdk.Plan{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(context.Background()), nil)}
	failOn(t, p.Set(context.Background(), &m))
	return p
}

func rvStateOf(t *testing.T, sch rschema.Schema, m rvModel) tfsdk.State {
	t.Helper()
	s := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(context.Background()), nil)}
	failOn(t, s.Set(context.Background(), &m))
	return s
}

func rvNull(sch rschema.Schema) tftypes.Value {
	return tftypes.NewValue(sch.Type().TerraformType(context.Background()), nil)
}

func rvModelOf(t *testing.T, s tfsdk.State) rvModel {
	t.Helper()
	var m rvModel
	failOn(t, s.Get(context.Background(), &m))
	return m
}

// rvModifyCreate runs the plan of a new reservation (releaseID set for the
// create half of a replacement).
func rvModifyCreate(t *testing.T, r *creditReservationResource, sch rschema.Schema, m rvModel, releaseID string) resource.ModifyPlanResponse {
	t.Helper()
	plan := rvPlanOf(t, sch, m)
	res := resource.ModifyPlanResponse{Plan: plan}
	if releaseID == "" {
		r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
			Plan: plan, Config: tfsdk.Config{Schema: sch, Raw: plan.Raw}, State: tfsdk.State{Schema: sch, Raw: rvNull(sch)},
		}, &res)
		return res
	}
	r.modifyCreatePlan(context.Background(), &res, m, releaseID)
	return res
}

func rvModifyUpdate(t *testing.T, r *creditReservationResource, sch rschema.Schema, state tfsdk.State, plan rvModel) resource.ModifyPlanResponse {
	t.Helper()
	p := rvPlanOf(t, sch, plan)
	res := resource.ModifyPlanResponse{Plan: p}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Plan: p, Config: tfsdk.Config{Schema: sch, Raw: p.Raw}, State: state,
	}, &res)
	return res
}

func rvCreate(t *testing.T, r *creditReservationResource, sch rschema.Schema, m rvModel) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	res := resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: rvNull(sch)}}
	r.Create(context.Background(), resource.CreateRequest{Plan: rvPlanOf(t, sch, m)}, &res)
	return res.State, res.Diagnostics
}

func rvUpdate(t *testing.T, r *creditReservationResource, sch rschema.Schema, state tfsdk.State, plan rvModel) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	res := resource.UpdateResponse{State: tfsdk.State{Schema: sch, Raw: rvNull(sch)}}
	r.Update(context.Background(), resource.UpdateRequest{Plan: rvPlanOf(t, sch, plan), State: state}, &res)
	return res.State, res.Diagnostics
}

func rvRead(t *testing.T, r *creditReservationResource, state tfsdk.State) resource.ReadResponse {
	t.Helper()
	res := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &res)
	return res
}

// rvNext is the plan for an existing reservation: the state, with what moves
// on its own unknown.
func rvNext(t *testing.T, s tfsdk.State) rvModel {
	m := rvModelOf(t, s)
	m.ConsumedCents, m.RemainingCents = types.Int64Unknown(), types.Int64Unknown()
	return m
}

func rvText(d diag.Diagnostics) string {
	var b strings.Builder
	for _, e := range d {
		b.WriteString(e.Summary() + ": " + e.Detail() + "\n")
	}
	return b.String()
}

// The production shape: two 1 GB Essentials databases at $14.52 a month and a
// $49.92 balance.
func rvTwoDatabases(t *testing.T) (*rvFake, *creditReservationResource, rschema.Schema) {
	f, srv := rvNewFake(t, 4992)
	f.addInstance(rvID(1), "alpha", 1452)
	f.addInstance(rvID(2), "beta", 1452)
	return f, &creditReservationResource{client: NewClient(srv.URL, "kvdb_admin")}, rvSchema(t)
}

// --- the user's scenario -----------------------------------------------------

// Three databases at $200 a month, $500 of credit, two of them reserved for a
// month each: a third reservation is refused, and the plan says so.
func TestRvThirdReservationIsRefusedAtPlan(t *testing.T) {
	f, srv := rvNewFake(t, 50000)
	for i, n := range []string{"orders", "search", "sessions"} {
		f.addInstance(rvID(i+1), n, 20000)
	}
	f.reserve(rvID(1), 20000)
	f.reserve(rvID(2), 20000)
	r := &creditReservationResource{client: NewClient(srv.URL, "kvdb_admin")}
	sch := rvSchema(t)

	res := rvModifyCreate(t, r, sch, rvPlanned(rvID(3), 1, 0, true), "")
	want := "Only $100.00 of your balance is unreserved; add credits to reserve $200.00."
	if !res.Diagnostics.HasError() || !strings.Contains(rvText(res.Diagnostics), want) {
		t.Fatalf("the plan must fail with the API's refusal %q, got:\n%s", want, rvText(res.Diagnostics))
	}
	for _, c := range f.calls {
		if c == "POST /credits/reservations" {
			t.Errorf("planning must not create anything")
		}
	}

	// And the apply, should it get there, meets the same words.
	_, d := rvCreate(t, r, sch, rvPlanned(rvID(3), 1, 0, true))
	if !d.HasError() || !strings.Contains(rvText(d), want) {
		t.Errorf("apply refusal lost: %s", rvText(d))
	}
}

func TestRvPlanWarnsWhichDatabasesAreLeftAtRisk(t *testing.T) {
	_, r, sch := rvTwoDatabases(t)
	res := rvModifyCreate(t, r, sch, rvPlanned(rvID(1), 1, 0, true), "")
	failOn(t, res.Diagnostics)
	text := rvText(res.Diagnostics)
	for _, want := range []string{
		"sets aside $14.52",
		"After this, $35.40 stays unreserved for 1 database without a reservation (beta): about 72.2 days of running before they are stopped.",
		"Before this change: $49.92 unreserved, about 50.9 days.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("warning lacks %q:\n%s", want, text)
		}
	}
	if res.Diagnostics.WarningsCount() != 1 {
		t.Errorf("want exactly one warning, got %d", res.Diagnostics.WarningsCount())
	}
}

func TestRvPlanRefusesStrandingTheOtherDatabase(t *testing.T) {
	_, r, sch := rvTwoDatabases(t)
	res := rvModifyCreate(t, r, sch, rvPlanned(rvID(1), 0, 4950, true), "")
	want := "That would leave $0.42 for your 1 running database(s) without a reservation, less than a day of their usage ($0.49), and they would be stopped. Add credits first, or reserve less."
	if !strings.Contains(rvText(res.Diagnostics), want) || !res.Diagnostics.HasError() {
		t.Fatalf("strand refusal not shown verbatim:\n%s", rvText(res.Diagnostics))
	}

	res = rvModifyCreate(t, r, sch, rvPlanned(rvID(1), 0, 5000, true), "")
	want = "Only $49.92 of your balance is unreserved; add credits to reserve $50.00."
	if !strings.Contains(rvText(res.Diagnostics), want) {
		t.Fatalf("over-unreserved refusal not shown verbatim:\n%s", rvText(res.Diagnostics))
	}
}

func TestRvUnknownAtPlanAsksNothing(t *testing.T) {
	f, r, sch := rvTwoDatabases(t)
	m := rvPlanned(rvID(1), 1, 0, true)
	m.InstanceID = types.StringUnknown()
	res := rvModifyCreate(t, r, sch, m, "")
	failOn(t, res.Diagnostics)
	if len(res.Diagnostics) != 0 || len(f.calls) != 0 {
		t.Errorf("an unknown database must be skipped quietly: %v %v", res.Diagnostics, f.calls)
	}
}

func TestRvMemberKeyRefusalIsShown(t *testing.T) {
	f, r, sch := rvTwoDatabases(t)
	msg := "Only the organization's owners and admins can manage credit reservations."
	f.refuse["POST /credits/reservations/preview"] = rvRefusal{403, `{"error":{"code":"forbidden","message":"` + msg + `"}}`}
	res := rvModifyCreate(t, r, sch, rvPlanned(rvID(1), 1, 0, true), "")
	if !res.Diagnostics.HasError() || !strings.Contains(rvText(res.Diagnostics), msg) {
		t.Errorf("403 lost: %s", rvText(res.Diagnostics))
	}
}

// --- lifecycle ---------------------------------------------------------------

func TestRvLifecycle(t *testing.T) {
	f, r, sch := rvTwoDatabases(t)
	ctx := context.Background()

	state, d := rvCreate(t, r, sch, rvPlanned(rvID(1), 1, 0, true))
	failOn(t, d)
	got := rvModelOf(t, state)
	if got.ReservedCents.ValueInt64() != 1452 || got.Months.ValueInt64() != 1 || !got.AmountCents.IsNull() || got.Status.ValueString() != "active" {
		t.Fatalf("created state wrong: %+v", got)
	}

	// Metering draws it down: a refresh reads it, and it is not a change.
	f.rows[0].consumed = 200
	rd := rvRead(t, r, state)
	failOn(t, rd.Diagnostics)
	state = rd.State
	if rvModelOf(t, state).RemainingCents.ValueInt64() != 1252 {
		t.Errorf("remaining not refreshed")
	}

	// Two more months: added in place, previewed against the reservation.
	f.resetCalls()
	plan := rvNext(t, state)
	plan.Months = types.Int64Value(3)
	mp := rvModifyUpdate(t, r, sch, state, plan)
	failOn(t, mp.Diagnostics)
	if len(mp.RequiresReplace) != 0 {
		t.Errorf("an increase must not replace")
	}
	if len(f.previews) != 1 || f.previews[0]["add_to_reservation_id"] != rvID(1) || f.previews[0]["months"] != float64(2) {
		t.Errorf("increase preview wrong: %v", f.previews)
	}
	state, d = rvUpdate(t, r, sch, state, plan)
	failOn(t, d)
	if !f.took("POST /credits/reservations/" + rvID(1) + "/add") {
		t.Errorf("increase did not add: %v", f.calls)
	}
	if got := rvModelOf(t, state); got.ReservedCents.ValueInt64() != 1452*3 || got.Months.ValueInt64() != 3 {
		t.Errorf("after add: %+v", got)
	}

	// Deactivate: warned at plan, done in place.
	plan = rvNext(t, state)
	plan.Active = types.BoolValue(false)
	mp = rvModifyUpdate(t, r, sch, state, plan)
	failOn(t, mp.Diagnostics)
	if !strings.Contains(rvText(mp.Diagnostics), `"alpha" is no longer protected: the $41.56 left`) {
		t.Errorf("deactivation warning: %s", rvText(mp.Diagnostics))
	}
	state, d = rvUpdate(t, r, sch, state, plan)
	failOn(t, d)
	if got := rvModelOf(t, state); got.Active.ValueBool() || got.DeactivatedReason.ValueString() != "user" {
		t.Errorf("not deactivated: %+v", got)
	}

	// Activate again: previewed as setting aside what is left.
	f.resetCalls()
	plan = rvNext(t, state)
	plan.Active = types.BoolValue(true)
	mp = rvModifyUpdate(t, r, sch, state, plan)
	failOn(t, mp.Diagnostics)
	if len(f.previews) != 1 || f.previews[0]["amount_cents"] != float64(4156) {
		t.Errorf("activation preview wrong: %v", f.previews)
	}
	state, d = rvUpdate(t, r, sch, state, plan)
	failOn(t, d)
	if !rvModelOf(t, state).Active.ValueBool() {
		t.Errorf("not active again")
	}

	// Destroy: warned, never previewed, deactivate then delete.
	f.resetCalls()
	dres := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: rvNull(sch)}}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{State: state, Plan: tfsdk.Plan{Schema: sch, Raw: rvNull(sch)},
		Config: tfsdk.Config{Schema: sch, Raw: rvNull(sch)}}, &dres)
	failOn(t, dres.Diagnostics)
	if dres.Diagnostics.WarningsCount() != 1 || len(f.calls) != 0 {
		t.Errorf("destroy plan: %v calls %v", dres.Diagnostics, f.calls)
	}
	del := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &del)
	failOn(t, del.Diagnostics)
	if !f.took("POST /credits/reservations/"+rvID(1)+"/deactivate") || !f.took("DELETE /credits/reservations/"+rvID(1)) || len(f.rows) != 0 {
		t.Errorf("delete must deactivate then delete: %v", f.calls)
	}

	// Gone: a refresh drops it, and deleting again is not an error.
	rd = rvRead(t, r, state)
	failOn(t, rd.Diagnostics)
	if !rd.State.Raw.IsNull() {
		t.Errorf("a deleted reservation must leave state")
	}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &del)
	failOn(t, del.Diagnostics)
}

func TestRvAmountIncreaseAndTooSmallIncrease(t *testing.T) {
	f, r, sch := rvTwoDatabases(t)
	state, d := rvCreate(t, r, sch, rvPlanned(rvID(1), 0, 1000, true))
	failOn(t, d)

	plan := rvNext(t, state)
	plan.AmountCents = types.Int64Value(1050)
	mp := rvModifyUpdate(t, r, sch, state, plan)
	if !mp.Diagnostics.HasError() || !strings.Contains(rvText(mp.Diagnostics), "at least 1100") {
		t.Errorf("an increase under $1 must fail at plan: %s", rvText(mp.Diagnostics))
	}

	plan.AmountCents = types.Int64Value(1500)
	f.resetCalls()
	mp = rvModifyUpdate(t, r, sch, state, plan)
	failOn(t, mp.Diagnostics)
	if f.previews[0]["amount_cents"] != float64(500) {
		t.Errorf("preview must add the difference: %v", f.previews)
	}
	state, d = rvUpdate(t, r, sch, state, plan)
	failOn(t, d)
	if rvModelOf(t, state).AmountCents.ValueInt64() != 1500 {
		t.Errorf("amount not raised")
	}

	// Raised in the console: read back, so it shows as a change.
	f.rows[0].amount = 2000
	rd := rvRead(t, r, state)
	failOn(t, rd.Diagnostics)
	if rvModelOf(t, rd.State).AmountCents.ValueInt64() != 2000 {
		t.Errorf("console increase not read")
	}
}

// Lowering replaces, says what that does to money, and the replacement's own
// plan previews with the old reservation released.
func TestRvLoweringReplacesAndPreviewsTheRelease(t *testing.T) {
	f, r, sch := rvTwoDatabases(t)
	state, d := rvCreate(t, r, sch, rvPlanned(rvID(1), 0, 4000, true))
	failOn(t, d)
	f.rows[0].consumed = 3500

	rd := rvRead(t, r, state)
	failOn(t, rd.Diagnostics)
	state = rd.State
	plan := rvNext(t, state)
	plan.AmountCents = types.Int64Value(3000)
	mp := rvModifyUpdate(t, r, sch, state, plan)
	failOn(t, mp.Diagnostics)
	if len(mp.RequiresReplace) != 1 || !mp.RequiresReplace[0].Equal(path.Root("amount_cents")) {
		t.Fatalf("lowering must replace: %v", mp.RequiresReplace)
	}
	text := rvText(mp.Diagnostics)
	for _, want := range []string{"returning the $5.00 it still holds", "sets aside $30.00 in full",
		"Only $5.00 is left on the old one, so this sets aside $25.00 more than is reserved now, not less."} {
		if !strings.Contains(text, want) {
			t.Errorf("replacement warning lacks %q:\n%s", want, text)
		}
	}

	// Without the release, alpha's live reservation makes the create look refused.
	f.resetCalls()
	res := rvModifyCreate(t, r, sch, rvPlanned(rvID(1), 0, 3000, true), rvModelOf(t, state).ID.ValueString())
	failOn(t, res.Diagnostics)
	if f.previews[0]["release_reservation_id"] != rvID(1) {
		t.Errorf("replacement preview must release the old reservation: %v", f.previews)
	}
	if !strings.Contains(rvText(res.Diagnostics), "The replacement reservation sets aside $30.00") {
		t.Errorf("replacement at-risk warning: %s", rvText(res.Diagnostics))
	}
	res = rvModifyCreate(t, r, sch, rvPlanned(rvID(1), 0, 3000, true), "")
	if !strings.Contains(rvText(res.Diagnostics), "already has an active reservation") {
		t.Errorf("sanity: without the release the preview refuses: %s", rvText(res.Diagnostics))
	}
}

func TestRvInstanceChangeReplaces(t *testing.T) {
	sch := rvSchema(t)
	a := sch.Attributes["instance_id"].(rschema.StringAttribute)
	var res planmodifier.StringResponse
	for _, m := range a.PlanModifiers {
		m.PlanModifyString(context.Background(), planmodifier.StringRequest{
			StateValue: types.StringValue(rvID(1)), PlanValue: types.StringValue(rvID(2)), ConfigValue: types.StringValue(rvID(2)),
			State: tfsdk.State{Schema: sch, Raw: rvPlanOf(t, sch, rvPlanned(rvID(1), 1, 0, true)).Raw},
			Plan:  rvPlanOf(t, sch, rvPlanned(rvID(2), 1, 0, true)),
		}, &res)
	}
	if !res.RequiresReplace {
		t.Errorf("another database must replace the reservation")
	}
}

func TestRvSystemDeactivationWarnsOnRefreshAndBlocksReactivation(t *testing.T) {
	f, r, sch := rvTwoDatabases(t)
	state, d := rvCreate(t, r, sch, rvPlanned(rvID(1), 1, 0, true))
	failOn(t, d)

	// alpha deleted: the API sets instance_id null and deactivates it.
	f.instances[rvID(1)].deleted = true
	f.rows[0].instanceID, f.rows[0].status, f.rows[0].reason = "", "inactive", "instance_deleted"
	rd := rvRead(t, r, state)
	failOn(t, rd.Diagnostics)
	if !strings.Contains(rvText(rd.Diagnostics), "KiviDB deactivated it because its database was deleted") {
		t.Errorf("no refresh warning: %s", rvText(rd.Diagnostics))
	}
	got := rvModelOf(t, rd.State)
	if got.InstanceID.ValueString() != rvID(1) {
		t.Errorf("instance_id must stay, or a replacement is planned: %v", got.InstanceID)
	}

	plan := rvNext(t, rd.State)
	plan.Active = types.BoolValue(true)
	mp := rvModifyUpdate(t, r, sch, rd.State, plan)
	if !mp.Diagnostics.HasError() || !strings.Contains(rvText(mp.Diagnostics), "cannot be activated again") {
		t.Errorf("re-activating a deleted database's reservation must fail at plan: %s", rvText(mp.Diagnostics))
	}
}

func TestRvRefusedApplyKeepsState(t *testing.T) {
	f, r, sch := rvTwoDatabases(t)
	state, d := rvCreate(t, r, sch, rvPlanned(rvID(1), 1, 0, true))
	failOn(t, d)
	plan := rvNext(t, state)
	plan.Months = types.Int64Value(12)
	after, d := rvUpdate(t, r, sch, state, plan)
	if !d.HasError() || !strings.Contains(rvText(d), "of your balance is unreserved") {
		t.Fatalf("apply refusal lost: %s", rvText(d))
	}
	if got := rvModelOf(t, after); got.Months.ValueInt64() != 1 || got.ReservedCents.ValueInt64() != 1452 {
		t.Errorf("state took the refused change: %+v", got)
	}
	_ = f
}

func TestRvImportByID(t *testing.T) {
	f, r, sch := rvTwoDatabases(t)
	id := f.reserve(rvID(2), 2904)
	f.rows[0].consumed = 100

	res := resource.ImportStateResponse{State: tfsdk.State{Schema: sch, Raw: rvNull(sch)}}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: strings.ToUpper(id)}, &res)
	failOn(t, res.Diagnostics)
	got := rvModelOf(t, res.State)
	if got.ID.ValueString() != id || got.InstanceID.ValueString() != rvID(2) || got.AmountCents.ValueInt64() != 2904 ||
		!got.Months.IsNull() || got.RemainingCents.ValueInt64() != 2804 || !got.Active.ValueBool() {
		t.Errorf("import wrong: %+v", got)
	}

	// Months written after an import are recorded, not re-priced or replaced.
	plan := rvNext(t, res.State)
	plan.AmountCents, plan.Months = types.Int64Null(), types.Int64Value(2)
	f.resetCalls()
	mp := rvModifyUpdate(t, r, sch, res.State, plan)
	failOn(t, mp.Diagnostics)
	if len(mp.RequiresReplace) != 0 || len(f.previews) != 0 {
		t.Errorf("adopting months must change nothing: %v %v", mp.RequiresReplace, f.previews)
	}

	res = resource.ImportStateResponse{State: tfsdk.State{Schema: sch, Raw: rvNull(sch)}}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: rvID(77)}, &res)
	if !res.Diagnostics.HasError() || !strings.Contains(rvText(res.Diagnostics), "No such reservation.") {
		t.Errorf("missing import: %s", rvText(res.Diagnostics))
	}
}

func TestRvExactlyOneAmount(t *testing.T) {
	r := &creditReservationResource{}
	sch := rvSchema(t)
	for _, tc := range []struct {
		months, cents types.Int64
		ok            bool
	}{
		{types.Int64Value(1), types.Int64Null(), true},
		{types.Int64Null(), types.Int64Value(500), true},
		{types.Int64Value(1), types.Int64Value(500), false},
		{types.Int64Null(), types.Int64Null(), false},
	} {
		m := rvPlanned(rvID(1), 1, 0, true)
		m.Months, m.AmountCents = tc.months, tc.cents
		cfg := tfsdk.Config{Schema: sch, Raw: rvPlanOf(t, sch, m).Raw}
		var diags diag.Diagnostics
		for _, v := range r.ConfigValidators(context.Background()) {
			var vr resource.ValidateConfigResponse
			v.ValidateResource(context.Background(), resource.ValidateConfigRequest{Config: cfg}, &vr)
			diags.Append(vr.Diagnostics...)
		}
		if diags.HasError() == tc.ok {
			t.Errorf("months=%v amount_cents=%v: ok=%v, got %v", tc.months, tc.cents, tc.ok, diags)
		}
	}
}

func TestRvDataSourceListsTheBalance(t *testing.T) {
	f, srv := rvNewFake(t, 4992)
	f.addInstance(rvID(1), "alpha", 1452)
	f.reserve(rvID(1), 1452)
	d := &creditReservationsDataSource{client: NewClient(srv.URL, "k")}
	var sres datasource.SchemaResponse
	d.Schema(context.Background(), datasource.SchemaRequest{}, &sres)
	failOn(t, sres.Diagnostics)
	res := datasource.ReadResponse{State: tfsdk.State{Schema: sres.Schema, Raw: tftypes.NewValue(sres.Schema.Type().TerraformType(context.Background()), nil)}}
	d.Read(context.Background(), datasource.ReadRequest{}, &res)
	failOn(t, res.Diagnostics)
	var got rvListModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.BalanceCents.ValueInt64() != 4992 || got.ReservedCents.ValueInt64() != 1452 || got.UnreservedCents.ValueInt64() != 3540 ||
		len(got.Reservations) != 1 || got.Reservations[0].InstanceName.ValueString() != "alpha" {
		t.Errorf("data source wrong: %+v", got)
	}
}

// Moving a reservation to another database names, at plan, the database it
// leaves unprotected: Terraform shows only the errors of a replacement's
// create half, so the warning comes from this first plan.
func TestRvMovingToAnotherDatabaseWarnsAboutTheOldOne(t *testing.T) {
	f, r, sch := rvTwoDatabases(t)
	state, d := rvCreate(t, r, sch, rvPlanned(rvID(1), 1, 0, true))
	failOn(t, d)
	f.resetCalls()
	plan := rvNext(t, state)
	plan.InstanceID = types.StringValue(rvID(2))
	mp := rvModifyUpdate(t, r, sch, state, plan)
	failOn(t, mp.Diagnostics)
	if len(f.previews) != 1 || f.previews[0]["release_reservation_id"] != rvID(1) || f.previews[0]["instance_id"] != rvID(2) {
		t.Errorf("move preview wrong: %v", f.previews)
	}
	if !strings.Contains(rvText(mp.Diagnostics), "for 1 database without a reservation (alpha)") {
		t.Errorf("the database left unprotected must be named: %s", rvText(mp.Diagnostics))
	}
}
