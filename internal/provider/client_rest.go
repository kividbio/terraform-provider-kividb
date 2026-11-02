package provider

import (
	"context"
	"net/http"
)

// RestAPI is a database's REST endpoint as the API reports it: on the
// instance object (rest_enabled, rest_status, rest_url, rest_error) and in the
// reply to POST /instances/:id/rest.
type RestAPI struct {
	Enabled bool    `json:"rest_enabled"`
	Status  string  `json:"rest_status"`
	URL     *string `json:"rest_url"`
	Error   *string `json:"rest_error"`
}

// SetRestAPI turns the endpoint on or off (POST /instances/:id/rest). The
// API applies port 443 to the database's firewall before it saves the change,
// so an error here means nothing changed.
func (c *Client) SetRestAPI(ctx context.Context, instanceID string, enabled bool) (*RestAPI, error) {
	var out RestAPI
	if err := c.do(ctx, http.MethodPost, "/instances/"+instanceID+"/rest", map[string]any{"enabled": enabled}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRestAPI reads the endpoint's state from the instance.
func (c *Client) GetRestAPI(ctx context.Context, instanceID string) (*RestAPI, error) {
	var wrapper struct {
		Instance RestAPI `json:"instance"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances/"+instanceID, nil, &wrapper); err != nil {
		return nil, err
	}
	return &wrapper.Instance, nil
}
