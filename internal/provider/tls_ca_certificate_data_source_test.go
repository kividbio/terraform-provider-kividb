package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestTLSCACertificateReadsThePEM(t *testing.T) {
	f := newFakeAPI(t)
	f.answer("GET /instances/i-1/tls/ca-cert", 200, testCA)
	res := readDataSource(t, &tlsCACertificateDataSource{client: f.client()}, map[string]tftypes.Value{"instance_id": str("i-1")})
	failOn(t, res.Diagnostics)
	var got tlsCACertificateModel
	failOn(t, res.State.Get(context.Background(), &got))
	if got.CertificatePEM.ValueString() != testCA || got.ID.ValueString() != "i-1" {
		t.Errorf("state = %+v", got)
	}
}

func TestTLSCACertificateErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"no such database", `{"error":{"code":"not_found"}}`, 404, "No database with id i-1"},
		{"refused", `{"error":{"code":"internal_error","message":"The CA certificate is not configured in this environment."}}`, 500,
			"The CA certificate is not configured in this environment."},
		{"not a certificate", `<html>gateway</html>`, 200, "without a PEM certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.answer("GET /instances/i-1/tls/ca-cert", tc.status, tc.body)
			res := readDataSource(t, &tlsCACertificateDataSource{client: f.client()}, map[string]tftypes.Value{"instance_id": str("i-1")})
			wantError(t, res.Diagnostics, tc.want)
		})
	}
}
