package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Alerts (routes/alerts.ts in the API). Channels say where a notice goes,
// rules say when one is sent. Neither lives under /instances, so an API key
// limited to some databases cannot reach any of it: a rule can name any
// database and send to any channel of the organization.

// AlertChannel is a channel as the API lists it.
//
// The API never returns a channel's URL or PagerDuty key whole: whoever holds a
// Slack incoming-webhook URL can post to that Slack channel. It returns a hint
// instead -- the URL's origin and the start of its path, or the key's last four
// characters -- which is all the provider has to recognise one after an import.
type AlertChannel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Roles     []string `json:"roles"`
	URLHint   *string  `json:"url_hint"`
	Region    *string  `json:"region"`
	KeyHint   *string  `json:"key_hint"`
	CreatedAt string   `json:"created_at"`
}

// CreateAlertChannelRequest is the union the API accepts, discriminated by
// kind. Fields that do not belong to the kind are left out rather than sent
// empty: the server's schema for each kind is strict about what it reads.
type CreateAlertChannelRequest struct {
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	Roles      []string `json:"roles,omitempty"`
	URL        string   `json:"url,omitempty"`
	RoutingKey string   `json:"routing_key,omitempty"`
	Region     string   `json:"region,omitempty"`
}

// CreatedAlertChannel is the create's answer. A webhook's signing secret is in
// it and in no other response: the server keeps only a sealed copy.
type CreatedAlertChannel struct {
	Channel         AlertChannel `json:"channel"`
	SigningSecret   *string      `json:"signing_secret"`
	SignatureHeader *string      `json:"signature_header"`
}

func (c *Client) CreateAlertChannel(ctx context.Context, req CreateAlertChannelRequest) (*CreatedAlertChannel, error) {
	var out CreatedAlertChannel
	if err := c.do(ctx, http.MethodPost, "/alerts/channels", req, &out); err != nil {
		return nil, err
	}
	if out.Channel.ID == "" {
		return nil, fmt.Errorf("the server accepted the channel but named no id")
	}
	return &out, nil
}

// ListAlertChannels returns every channel of the organization, oldest first.
func (c *Client) ListAlertChannels(ctx context.Context) ([]AlertChannel, error) {
	var out struct {
		Channels []AlertChannel `json:"channels"`
	}
	if err := c.do(ctx, http.MethodGet, "/alerts/channels", nil, &out); err != nil {
		return nil, err
	}
	return out.Channels, nil
}

// GetAlertChannel finds one channel in the list. There is no read-one
// endpoint, so a channel that is not listed answers as a 404 would, and the
// resource drops it from state the same way.
func (c *Client) GetAlertChannel(ctx context.Context, id string) (*AlertChannel, error) {
	all, err := c.ListAlertChannels(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if strings.EqualFold(all[i].ID, id) {
			return &all[i], nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Code: "not_found", Message: "No alert channel " + id + " in this organization."}
}

// DeleteAlertChannel is refused with 409 channel_in_use while any rule still
// sends to the channel, and the message names those rules.
func (c *Client) DeleteAlertChannel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/alerts/channels/"+id, nil, nil)
}

// AlertRule is a rule as the API lists it, and as the create and replace
// answer it. The list also carries where the rule stands (status, last value);
// the provider does not surface those, because they change with the database
// and are not configuration.
type AlertRule struct {
	ID              string    `json:"id"`
	InstanceID      *string   `json:"instance_id"`
	Name            string    `json:"name"`
	Metric          string    `json:"metric"`
	Op              string    `json:"op"`
	Threshold       flexFloat `json:"threshold"`
	ForMinutes      int64     `json:"for_minutes"`
	CooldownMinutes int64     `json:"cooldown_minutes"`
	ChannelIDs      []string  `json:"channel_ids"`
	Enabled         bool      `json:"enabled"`
}

// AlertRuleRequest is the body of both the create and the replace. The server
// fills in op, threshold and the failover overrides from the metric, so those
// are sent only when set.
type AlertRuleRequest struct {
	Name            string   `json:"name"`
	Metric          string   `json:"metric"`
	InstanceID      *string  `json:"instance_id,omitempty"`
	Op              *string  `json:"op,omitempty"`
	Threshold       *float64 `json:"threshold,omitempty"`
	ForMinutes      int64    `json:"for_minutes"`
	CooldownMinutes int64    `json:"cooldown_minutes"`
	ChannelIDs      []string `json:"channel_ids"`
	Enabled         bool     `json:"enabled"`
}

func (c *Client) CreateAlertRule(ctx context.Context, req AlertRuleRequest) (*AlertRule, error) {
	var out struct {
		Rule AlertRule `json:"rule"`
	}
	if err := c.do(ctx, http.MethodPost, "/alerts/rules", req, &out); err != nil {
		return nil, err
	}
	if out.Rule.ID == "" {
		return nil, fmt.Errorf("the server accepted the alert but named no id")
	}
	return &out.Rule, nil
}

// ReplaceAlertRule is a PUT: the whole rule, every time. The server also forgets
// where the rule stood, so a changed rule starts evaluating from scratch.
func (c *Client) ReplaceAlertRule(ctx context.Context, id string, req AlertRuleRequest) (*AlertRule, error) {
	var out struct {
		Rule AlertRule `json:"rule"`
	}
	if err := c.do(ctx, http.MethodPut, "/alerts/rules/"+id, req, &out); err != nil {
		return nil, err
	}
	if out.Rule.ID == "" {
		out.Rule.ID = id
	}
	return &out.Rule, nil
}

func (c *Client) ListAlertRules(ctx context.Context) ([]AlertRule, error) {
	var out struct {
		Rules []AlertRule `json:"rules"`
	}
	if err := c.do(ctx, http.MethodGet, "/alerts/rules", nil, &out); err != nil {
		return nil, err
	}
	return out.Rules, nil
}

// GetAlertRule finds one rule in the list; see GetAlertChannel.
func (c *Client) GetAlertRule(ctx context.Context, id string) (*AlertRule, error) {
	all, err := c.ListAlertRules(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if strings.EqualFold(all[i].ID, id) {
			return &all[i], nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Code: "not_found", Message: "No alert " + id + " in this organization."}
}

func (c *Client) DeleteAlertRule(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/alerts/rules/"+id, nil, nil)
}

// AuditLogSettings is whether the organization keeps an audit log
// (routes/audit-log.ts). Read by owner and admin keys; changed only by an
// owner signed in to the console, never by a key.
type AuditLogSettings struct {
	Enabled       bool    `json:"enabled"`
	EnabledAt     *string `json:"enabled_at"`
	EnabledBy     *string `json:"enabled_by"`
	HotDays       int64   `json:"hot_days"`
	RetentionDays int64   `json:"retention_days"`
	CanChange     bool    `json:"can_change"`
}

func (c *Client) GetAuditLogSettings(ctx context.Context) (*AuditLogSettings, error) {
	var out AuditLogSettings
	if err := c.do(ctx, http.MethodGet, "/audit-log/settings", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// maskURL is the API's own url_hint, reproduced: the URL's origin and the first
// twelve characters of its path, with an ellipsis when the path was longer.
// Used only to recognise an imported channel's URL in configuration; when it
// disagrees with the server's hint the channel is replaced, which is the safe
// direction to be wrong in.
func maskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" && !(u.Scheme == "https" && p == "443") && !(u.Scheme == "http" && p == "80") {
		host += ":" + p
	}
	origin := strings.ToLower(u.Scheme) + "://" + host
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if r := []rune(path); len(r) > 12 {
		path = string(r[:12]) + "…"
	}
	return origin + path
}
