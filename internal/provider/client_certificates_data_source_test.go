package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestClientCertificatesRead(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET /instances/i-1/client-certificates", 200, `{"mode":"required",
		"ca_certificates":[{"subject":"CN=Example Client CA","notAfter":"2027-10-25T00:00:00.000Z","fingerprint256":"AB:CD"}],
		"ca_pem":`+jsonString(testCA)+`,"tls_enabled":true,"tls_only":true,"available":true}`)
	res := dsReadDataSource(t, &clientCertificatesDataSource{client: f.client()}, map[string]tftypes.Value{"instance_id": dsStr("i-1")})
	failOn(t, res.Diagnostics)
	var got clientCertificatesModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.Mode.ValueString() != "required" || got.CAPem.ValueString() != testCA || !got.TLSOnly.ValueBool() || !got.Available.ValueBool() {
		t.Errorf("state = %+v", got)
	}
	var certs []struct {
		Subject     types.String `tfsdk:"subject"`
		NotAfter    types.String `tfsdk:"not_after"`
		Fingerprint types.String `tfsdk:"fingerprint_sha256"`
	}
	failOn(t, got.CACertificates.ElementsAs(context.Background(), &certs, false))
	if len(certs) != 1 || certs[0].NotAfter.ValueString() != "2027-10-25T00:00:00.000Z" || certs[0].Fingerprint.ValueString() != "AB:CD" {
		t.Errorf("ca_certificates = %+v", certs)
	}
}

func TestClientCertificatesOffHasNoCA(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET /instances/i-1/client-certificates", 200,
		`{"mode":"off","ca_certificates":[],"ca_pem":null,"tls_enabled":false,"tls_only":false,"available":false}`)
	res := dsReadDataSource(t, &clientCertificatesDataSource{client: f.client()}, map[string]tftypes.Value{"instance_id": dsStr("i-1")})
	failOn(t, res.Diagnostics)
	var got clientCertificatesModel
	failOn(t, res.State.Get(context.Background(), &got))
	if !got.CAPem.IsNull() || len(got.CACertificates.Elements()) != 0 || got.CACertificates.IsNull() {
		t.Errorf("off: ca_pem=%v ca_certificates=%v", got.CAPem, got.CACertificates)
	}
}

func TestClientCertificatesErrors(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET /instances/i-1/client-certificates", 404, `{"error":{"code":"not_found"}}`)
	res := dsReadDataSource(t, &clientCertificatesDataSource{client: f.client()}, map[string]tftypes.Value{"instance_id": dsStr("i-1")})
	wantError(t, res.Diagnostics, "No database with id i-1")

	g := newFakeAPI(t)
	g.answer("GET /instances/i-1/client-certificates", 403, `{"error":{"code":"forbidden","message":"Your role cannot read this."}}`)
	res = dsReadDataSource(t, &clientCertificatesDataSource{client: g.client()}, map[string]tftypes.Value{"instance_id": dsStr("i-1")})
	wantError(t, res.Diagnostics, "Your role cannot read this.")
}

func jsonString(s string) string {
	out := `"`
	for _, r := range s {
		switch r {
		case '\n':
			out += `\n`
		case '"':
			out += `\"`
		default:
			out += string(r)
		}
	}
	return out + `"`
}
