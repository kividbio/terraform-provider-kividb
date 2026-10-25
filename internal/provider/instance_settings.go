package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The parts of kividb_instance added with the database settings: the free
// tier, shards, encrypted replication and client certificates.

// ValidateConfig refuses, at plan, combinations the API would refuse at apply
// or -- worse -- accept and quietly build differently.
//
// The free tier is the second kind. The API forces its size, tier, replicas
// and flags whatever is sent, so a configuration asking for 8 GB of free
// database would be created as 1 GB and then fail the apply as an inconsistent
// result. Saying so here names the attribute to change.
func (r *instanceResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, res *resource.ValidateConfigResponse) {
	var cfg instanceModel
	res.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if res.Diagnostics.HasError() {
		return
	}

	isTrue := func(b types.Bool) bool { return !b.IsUnknown() && !b.IsNull() && b.ValueBool() }
	// Unset means the schema default, which is false for every flag here.
	isFalse := func(b types.Bool) bool { return !b.IsUnknown() && (b.IsNull() || !b.ValueBool()) }
	sharded := !cfg.ShardCount.IsUnknown() && !cfg.ShardCount.IsNull() && cfg.ShardCount.ValueInt64() > 1

	free := isTrue(cfg.FreeTier)
	paid := isFalse(cfg.FreeTier)

	if !cfg.AofEnabled.IsUnknown() && !cfg.AofEnabled.IsNull() {
		switch {
		case free && cfg.AofEnabled.ValueBool():
			res.Diagnostics.AddAttributeError(path.Root("aof_enabled"), "The free tier has no append-only persistence",
				"A free-tier database keeps no append-only file. Remove `aof_enabled`.")
		case paid && !cfg.AofEnabled.ValueBool():
			res.Diagnostics.AddAttributeError(path.Root("aof_enabled"), "Append-only persistence is always on",
				"KiviDB keeps append-only persistence on for every paid database, because without it a lost "+
					"machine loses the data. The API ignores `false`, so the database would not match this "+
					"configuration. Remove `aof_enabled`.")
		}
	}

	if free {
		refuse := func(attr, why string) {
			res.Diagnostics.AddAttributeError(path.Root(attr), "Not available on the free tier",
				why+" Remove it, or set `free_tier = false` for a paid database.")
		}
		if !cfg.Tier.IsUnknown() && cfg.Tier.ValueString() != "essentials" {
			refuse("tier", "A free-tier database is an Essentials database: set `tier = \"essentials\"`.")
		}
		if !cfg.DataSizeGB.IsUnknown() && cfg.DataSizeGB.ValueInt64() != 1 {
			refuse("data_size_gb", "A free-tier database is 1 GB: set `data_size_gb = 1`.")
		}
		if !cfg.Cloud.IsUnknown() && cfg.Cloud.ValueString() != "aws" {
			refuse("cloud", "The free tier runs on AWS only.")
		}
		if !cfg.ReplicaCount.IsUnknown() && !cfg.ReplicaCount.IsNull() && cfg.ReplicaCount.ValueInt64() != 0 {
			refuse("replica_count", "A free-tier database has no replicas.")
		}
		for attr, v := range map[string]types.Bool{
			"lua_enabled": cfg.LuaEnabled, "tls_enabled": cfg.TLSEnabled, "tls_only": cfg.TLSOnly,
			"replication_tls": cfg.ReplicationTLS,
		} {
			if isTrue(v) {
				refuse(attr, fmt.Sprintf("A free-tier database runs without `%s`.", attr))
			}
		}
		if !cfg.ClientCertMode.IsUnknown() && !cfg.ClientCertMode.IsNull() && cfg.ClientCertMode.ValueString() != "off" {
			refuse("client_cert_mode", "A free-tier database has no TLS, so no client certificates.")
		}
		if sharded {
			refuse("shard_count", "A free-tier database is a single shard.")
		}
		if !cfg.CloudAccountID.IsUnknown() && !cfg.CloudAccountID.IsNull() {
			refuse("cloud_account_id", "The free tier runs on KiviDB's shared hosts, not in your own cloud account.")
		}
	}

	// Encrypted replication uses the database's TLS certificate.
	if isTrue(cfg.ReplicationTLS) {
		if isFalse(cfg.TLSEnabled) {
			res.Diagnostics.AddAttributeError(path.Root("replication_tls"), "Encrypted replication needs TLS",
				"Encrypted replication uses the database's TLS certificate. Set `tls_enabled = true` as well.")
		}
		if sharded {
			res.Diagnostics.AddAttributeError(path.Root("replication_tls"), "Not available on sharded databases",
				"A sharded database with TLS already encrypts traffic between its nodes, replication included. "+
					"Remove `replication_tls`.")
		}
	}

	mode := cfg.ClientCertMode
	if !mode.IsUnknown() && !mode.IsNull() && mode.ValueString() != "off" {
		if isFalse(cfg.TLSEnabled) {
			res.Diagnostics.AddAttributeError(path.Root("client_cert_mode"), "Client certificates need TLS",
				"Client certificates are checked on the TLS port. Set `tls_enabled = true` as well.")
		}
		if mode.ValueString() == "required" && isFalse(cfg.TLSOnly) {
			res.Diagnostics.AddAttributeError(path.Root("client_cert_mode"), "Requiring client certificates needs tls_only",
				"With the plaintext port open, a client could connect there without a certificate. "+
					"Set `tls_only = true`, or use `client_cert_mode = \"optional\"`.")
		}
		if cfg.ClientCaPem.IsNull() {
			res.Diagnostics.AddAttributeError(path.Root("client_ca_pem"), "No client CA",
				"Set `client_ca_pem` to the CA certificate that signs your client certificates.")
		}
		if sharded {
			res.Diagnostics.AddAttributeError(path.Root("client_cert_mode"), "Not available on sharded databases",
				"Client certificates are not available for sharded databases yet.")
		}
	}
	if (mode.IsNull() || (!mode.IsUnknown() && mode.ValueString() == "off")) &&
		!cfg.ClientCaPem.IsNull() && !cfg.ClientCaPem.IsUnknown() {
		res.Diagnostics.AddAttributeError(path.Root("client_ca_pem"), "A client CA without client certificates",
			"The API discards the CA when `client_cert_mode` is `off`, so it would never match this "+
				"configuration. Set `client_cert_mode` to `optional` or `required`, or remove `client_ca_pem`.")
	}
}

// aofFollowsTier plans aof_enabled when the configuration leaves it unset:
// on for a paid database, off for the free tier, which is what the API does.
type aofFollowsTierModifier struct{}

func aofFollowsTier() planmodifier.Bool { return aofFollowsTierModifier{} }

func (aofFollowsTierModifier) Description(_ context.Context) string {
	return "On for a paid database and off for the free tier, as the API decides."
}

func (m aofFollowsTierModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (aofFollowsTierModifier) PlanModifyBool(ctx context.Context, req planmodifier.BoolRequest, res *planmodifier.BoolResponse) {
	if !req.ConfigValue.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var free types.Bool
	res.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("free_tier"), &free)...)
	if free.IsUnknown() {
		res.PlanValue = types.BoolUnknown()
		return
	}
	res.PlanValue = types.BoolValue(!free.ValueBool())
}

// caFollowsMode plans client_ca_pem when the configuration leaves it unset:
// null when client certificates are off, because the API discards the CA then,
// and otherwise whatever the database already holds.
type caFollowsModeModifier struct{}

func caFollowsMode() planmodifier.String { return caFollowsModeModifier{} }

func (caFollowsModeModifier) Description(_ context.Context) string {
	return "Null while client certificates are off; otherwise the CA already stored."
}

func (m caFollowsModeModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (caFollowsModeModifier) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, res *planmodifier.StringResponse) {
	if !req.ConfigValue.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var mode types.String
	res.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("client_cert_mode"), &mode)...)
	switch {
	case mode.IsUnknown():
		res.PlanValue = types.StringUnknown()
	case mode.IsNull() || mode.ValueString() == "off":
		res.PlanValue = types.StringNull()
	default:
		res.PlanValue = req.StateValue
	}
}

// nodeSecurityChanged reports whether encrypted replication or client
// certificates differ between plan and state.
func nodeSecurityChanged(plan, state *instanceModel) bool {
	if !plan.ReplicationTLS.Equal(state.ReplicationTLS) || !plan.ClientCertMode.Equal(state.ClientCertMode) {
		return true
	}
	return caNeedsSending(plan, state.ClientCertMode, state.ClientCaPem)
}

// caNeedsSending: the CA is sent when the mode asks for one and the planned CA
// is not the one already stored. The API keeps the stored CA when none is sent,
// and drops it itself when the mode goes off.
func caNeedsSending(plan *instanceModel, haveMode, haveCA types.String) bool {
	if plan.ClientCertMode.ValueString() == "off" || plan.ClientCaPem.IsNull() || plan.ClientCaPem.IsUnknown() {
		return false
	}
	if !plan.ClientCertMode.Equal(haveMode) || haveCA.IsNull() || haveCA.IsUnknown() {
		return true
	}
	return !samePEM(plan.ClientCaPem.ValueString(), haveCA.ValueString())
}

// nodeSecurityRequest is the part of a binary request these settings add. Only
// what changed is sent: an unchanged flag sent anyway would be applied again.
func nodeSecurityRequest(plan, state *instanceModel) binarySettingsFields {
	var out binarySettingsFields
	if !plan.ReplicationTLS.Equal(state.ReplicationTLS) {
		out.ReplicationTLS = boolPtr(plan.ReplicationTLS)
	}
	if !plan.ClientCertMode.Equal(state.ClientCertMode) && !plan.ClientCertMode.IsUnknown() {
		v := plan.ClientCertMode.ValueString()
		out.ClientCertMode = &v
	}
	if caNeedsSending(plan, state.ClientCertMode, state.ClientCaPem) {
		v := plan.ClientCaPem.ValueString()
		out.ClientCaPem = &v
	}
	return out
}

type settingExpectation = struct {
	field string
	want  string
	got   func(*Instance) string
}

// nodeSecurityExpectations are what an update that changed these settings has
// to see before it is finished, in the shape wantedBy checks.
func nodeSecurityExpectations(plan, state *instanceModel) []settingExpectation {
	var out []settingExpectation
	if !plan.ReplicationTLS.Equal(state.ReplicationTLS) && !plan.ReplicationTLS.IsUnknown() {
		out = append(out, settingExpectation{
			"replication_tls", fmt.Sprint(plan.ReplicationTLS.ValueBool()),
			func(i *Instance) string { return fmt.Sprint(i.ReplicationTLS) },
		})
	}
	if !plan.ClientCertMode.Equal(state.ClientCertMode) && !plan.ClientCertMode.IsUnknown() {
		out = append(out, settingExpectation{
			"client_cert_mode", plan.ClientCertMode.ValueString(),
			func(i *Instance) string { return i.clientCertMode() },
		})
	}
	return out
}

// applyInstanceSettings copies these settings from what the API returned.
func applyInstanceSettings(m *instanceModel, inst *Instance) {
	m.FreeTier = types.BoolValue(inst.FreeTier)
	m.ShardCount = types.Int64Value(inst.shards())
	m.ReplicationTLS = types.BoolValue(inst.ReplicationTLS)
	m.ClientCertMode = types.StringValue(inst.clientCertMode())

	// The API stores the CA re-joined from its certificate blocks, so the
	// configured text is kept when it holds the same certificates: a file with
	// Windows line endings or a trailing comment must not plan a change on
	// every refresh.
	switch {
	case inst.ClientCaPem == nil || *inst.ClientCaPem == "":
		m.ClientCaPem = types.StringNull()
	case !m.ClientCaPem.IsNull() && !m.ClientCaPem.IsUnknown() && samePEM(m.ClientCaPem.ValueString(), *inst.ClientCaPem):
		// keep the configured spelling
	default:
		m.ClientCaPem = types.StringValue(*inst.ClientCaPem)
	}
}

// applyNodeSecurityAfterCreate applies encrypted replication and client
// certificates to a database that was just created with them in its plan.
//
// The create endpoint takes neither: the API applies both to a running
// database, by the restart that installs the engine binary. So the database is
// waited for -- whatever wait_for_ready says, because the change cannot be made
// before it runs -- and the change is then made and waited for as an update
// would be. Waiting for it is not optional either: an apply that returned
// before the restart would record a value the database does not have yet, and
// Terraform would fail it as an inconsistent result.
func (r *instanceResource) applyNodeSecurityAfterCreate(ctx context.Context, plan *instanceModel, inst *Instance) (*Instance, error) {
	wantRepl := plan.ReplicationTLS.ValueBool()
	wantMode := plan.ClientCertMode.ValueString()
	if wantMode == "" {
		wantMode = "off"
	}
	if !wantRepl && wantMode == "off" {
		return inst, nil
	}

	inst, err := r.settle(ctx, inst.ID, true)
	if err != nil {
		return inst, err
	}
	if inst.ReplicationTLS == wantRepl && inst.clientCertMode() == wantMode {
		return inst, nil
	}

	version := ""
	if inst.KividbVersion != nil {
		version = *inst.KividbVersion
	}
	if version == "" {
		version = plan.KividbVersion.ValueString()
	}
	if version == "" {
		return inst, fmt.Errorf("the database reports no engine version, which the API needs to apply " +
			"these settings; set `kividb_version` and apply again")
	}

	current := instanceModel{
		ReplicationTLS: types.BoolValue(inst.ReplicationTLS),
		ClientCertMode: types.StringValue(inst.clientCertMode()),
		ClientCaPem:    stringOrNull(inst.ClientCaPem),
	}
	br := BinaryRequest{KividbVersion: version}
	br.binarySettingsFields = nodeSecurityRequest(plan, &current)
	if err := r.client.UpgradeBinary(ctx, inst.ID, br); err != nil {
		return inst, err
	}

	want := func(i *Instance) error {
		if i.ReplicationTLS != wantRepl {
			return fmt.Errorf("replication_tls is still %t, not %t", i.ReplicationTLS, wantRepl)
		}
		if i.clientCertMode() != wantMode {
			return fmt.Errorf("client_cert_mode is still %s, not %s", i.clientCertMode(), wantMode)
		}
		return nil
	}
	return r.settleUntil(ctx, inst.ID, true, want)
}
