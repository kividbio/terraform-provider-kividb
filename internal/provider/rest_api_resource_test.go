package provider

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const restPath = "/instances/i-1/rest"
const restInstancePath = "/instances/i-1"

func TestRestAPICreateWaitsUntilReady(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("POST "+restPath, 202, `{"rest_enabled":true,"rest_status":"issuing","rest_url":null,"rest_error":null}`)
	polls := 0
	f.on("GET "+restInstancePath, func(c apiCall) apiReply {
		polls++
		if polls < 3 {
			return apiReply{200, `{"instance":{"id":"i-1","rest_enabled":true,"rest_status":"issuing","rest_url":null}}`}
		}
		return apiReply{200, `{"instance":{"id":"i-1","rest_enabled":true,"rest_status":"ready","rest_url":"https://db.cloud.kividb.io"}}`}
	})
	r := &restAPIResource{client: f.client(), poll: time.Millisecond}
	res := dsDoCreate(t, r, map[string]tftypes.Value{"instance_id": dsStr("i-1")})
	failOn(t, res.Diagnostics)
	if sent := f.sent("POST " + restPath); len(sent) != 1 || sent[0].Body["enabled"] != true {
		t.Fatalf("POST body = %+v", sent)
	}
	var got restAPIModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.URL.ValueString() != "https://db.cloud.kividb.io" || got.Status.ValueString() != "ready" || polls != 3 {
		t.Errorf("state = %+v after %d polls", got, polls)
	}
}

func TestRestAPICreateReportsAFailureAndKeepsState(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("POST "+restPath, 202, `{"rest_enabled":true,"rest_status":"issuing","rest_url":null,"rest_error":null}`)
	f.answer("GET "+restInstancePath, 200, `{"instance":{"id":"i-1","rest_enabled":true,"rest_status":"unavailable","rest_url":null,"rest_error":"agent_update_needed"}}`)
	res := dsDoCreate(t, &restAPIResource{client: f.client(), poll: time.Millisecond}, map[string]tftypes.Value{"instance_id": dsStr("i-1")})
	wantError(t, res.Diagnostics, "agent_update_needed")
	var got restAPIModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.ID.ValueString() != "i-1" {
		t.Errorf("the endpoint is on, so it must be in state for a destroy to turn it off: %+v", got)
	}
}

func TestRestAPIReadDropsWhatWasTurnedOffElsewhere(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET "+restInstancePath, 200, `{"instance":{"id":"i-1","rest_enabled":false,"rest_status":"off","rest_url":null}}`)
	res := dsDoRead(t, &restAPIResource{client: f.client()}, map[string]tftypes.Value{
		"id": dsStr("i-1"), "instance_id": dsStr("i-1"), "wait_until_ready": dsBoolean(true), "url": dsStr("https://x"), "status": dsStr("ready"),
	})
	failOn(t, res.Diagnostics)
	if !res.State.Raw.IsNull() {
		t.Errorf("state kept for an endpoint that is off")
	}
}

func TestRestAPIDeleteTurnsItOff(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("POST "+restPath, 202, `{"rest_enabled":false,"rest_status":"off","rest_url":null,"rest_error":null}`)
	res := dsDoDelete(t, &restAPIResource{client: f.client()}, map[string]tftypes.Value{
		"id": dsStr("i-1"), "instance_id": dsStr("i-1"), "wait_until_ready": dsBoolean(true), "url": dsStr("https://x"), "status": dsStr("ready"),
	})
	failOn(t, res.Diagnostics)
	if sent := f.sent("POST " + restPath); len(sent) != 1 || sent[0].Body["enabled"] != false {
		t.Fatalf("POST body = %+v", sent)
	}
}
