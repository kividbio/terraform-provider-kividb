package provider

import (
	"context"
	"net/http"
	"sync"
)

// A database's network settings: the allowlist, TLS-only, blocking public
// access, and private connectivity (PrivateLink, Private Link Service or
// Private Service Connect, depending on the cloud).
//
// PUT /instances/:id/network replaces the whole allowlist on every call, and
// also carries tls_only and public_access_blocked, which are kept when omitted.
// Three resources write through it -- kividb_instance_network (the allowlist),
// kividb_instance (tls_only) and kividb_private_link (public_access_blocked) --
// so each sends only its own field and, where the route needs the allowlist
// too, the one it has just read. networkLock serializes those writes within
// this provider process: without it, two resources applied in parallel could
// read the allowlist, and one write back a list the other had just replaced.

type NetworkRule struct {
	ID     string  `json:"id"`
	CIDR   string  `json:"cidr"`
	Port   int64   `json:"port"`
	Action string  `json:"action"`
	Label  *string `json:"label"`
}

// NetworkRuleInput is a rule as PUT takes it.
type NetworkRuleInput struct {
	CIDR   string  `json:"cidr"`
	Action string  `json:"action"`
	Label  *string `json:"label"`
}

type NetworkReconcile struct {
	Synced bool    `json:"synced"`
	Error  *string `json:"error"`
}

// Network is what GET and PUT /instances/:id/network answer.
type Network struct {
	Rules                 []NetworkRule    `json:"rules"`
	Port                  int64            `json:"port"`
	Ports                 []int64          `json:"ports"`
	TLSEnabled            bool             `json:"tls_enabled"`
	TLSOnly               bool             `json:"tls_only"`
	PrivateNetworkEnabled bool             `json:"private_network_enabled"`
	PublicAccessBlocked   bool             `json:"public_access_blocked"`
	Reconcile             NetworkReconcile `json:"reconcile"`
}

type networkUpdate struct {
	// Always sent, even empty: the route requires it.
	Rules               []NetworkRuleInput `json:"rules"`
	TLSOnly             *bool              `json:"tls_only,omitempty"`
	PublicAccessBlocked *bool              `json:"public_access_blocked,omitempty"`
}

var networkLocks sync.Map // instance id -> *sync.Mutex

func networkLock(instanceID string) *sync.Mutex {
	m, _ := networkLocks.LoadOrStore(instanceID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (c *Client) GetNetwork(ctx context.Context, instanceID string) (*Network, error) {
	var out Network
	if err := c.do(ctx, http.MethodGet, "/instances/"+instanceID+"/network", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) putNetwork(ctx context.Context, instanceID string, body networkUpdate) (*Network, error) {
	if body.Rules == nil {
		body.Rules = []NetworkRuleInput{}
	}
	var out Network
	if err := c.do(ctx, http.MethodPut, "/instances/"+instanceID+"/network", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReplaceAllowlist sets the allowlist and leaves tls_only and public access as
// they are.
func (c *Client) ReplaceAllowlist(ctx context.Context, instanceID string, rules []NetworkRuleInput) (*Network, error) {
	mu := networkLock(instanceID)
	mu.Lock()
	defer mu.Unlock()
	return c.putNetwork(ctx, instanceID, networkUpdate{Rules: rules})
}

// SetTLSOnly and SetPublicAccessBlocked change one flag and write back the
// allowlist exactly as the API holds it, because the route takes the
// allowlist with every write.
func (c *Client) SetTLSOnly(ctx context.Context, instanceID string, on bool) (*Network, error) {
	return c.setNetworkFlag(ctx, instanceID, networkUpdate{TLSOnly: &on})
}

func (c *Client) SetPublicAccessBlocked(ctx context.Context, instanceID string, blocked bool) (*Network, error) {
	return c.setNetworkFlag(ctx, instanceID, networkUpdate{PublicAccessBlocked: &blocked})
}

func (c *Client) setNetworkFlag(ctx context.Context, instanceID string, body networkUpdate) (*Network, error) {
	mu := networkLock(instanceID)
	mu.Lock()
	defer mu.Unlock()
	current, err := c.GetNetwork(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	body.Rules = make([]NetworkRuleInput, 0, len(current.Rules))
	for _, r := range current.Rules {
		action := r.Action
		if action == "" {
			action = "allow"
		}
		body.Rules = append(body.Rules, NetworkRuleInput{CIDR: r.CIDR, Action: action, Label: r.Label})
	}
	return c.putNetwork(ctx, instanceID, body)
}

// --- private connectivity ------------------------------------------------------

type PrivateLinkPort struct {
	Port      int64  `json:"port"`
	Role      string `json:"role"`
	TLS       bool   `json:"tls"`
	Label     string `json:"label"`
	SlotIndex *int64 `json:"slotIndex"`
}

// PrivateLink is GET /instances/:id/private-link. PUT answers a subset of it.
type PrivateLink struct {
	Enabled             bool              `json:"enabled"`
	Region              string            `json:"region"`
	TLSEnabled          bool              `json:"tls_enabled"`
	Ports               []PrivateLinkPort `json:"ports"`
	ServiceName         *string           `json:"service_name"`
	AllowedPrincipals   []string          `json:"allowed_principals"`
	PublicAccessBlocked bool              `json:"public_access_blocked"`
}

func (c *Client) GetPrivateLink(ctx context.Context, instanceID string) (*PrivateLink, error) {
	var out PrivateLink
	if err := c.do(ctx, http.MethodGet, "/instances/"+instanceID+"/private-link", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetPrivateLink turns private connectivity on or off and sets who may
// connect. A nil principals list keeps the current one; an empty one clears it.
// The endpoint service is built by a background job: the answer says what was
// asked for, and GetPrivateLink's service_name says when it exists.
func (c *Client) SetPrivateLink(ctx context.Context, instanceID string, enabled *bool, principals []string) error {
	body := map[string]any{}
	if enabled != nil {
		body["enabled"] = *enabled
	}
	if principals != nil {
		body["allowed_principals"] = principals
	}
	return c.do(ctx, http.MethodPut, "/instances/"+instanceID+"/private-link", body, nil)
}
