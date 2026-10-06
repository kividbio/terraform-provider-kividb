package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client talks to the KiviDB Cloud API as an organization, not as a person.
//
// An API key carries the organization and the role, so there is no notion of
// "current organization" to select and no session to refresh. The key is sent as
// a bearer token exactly as a session token would be; the server tells them
// apart by the kvdb_ prefix.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// Generous, because a create is a synchronous request that provisions
		// nothing -- it queues -- but a resize on a busy control plane can take
		// a few seconds to be accepted. Waiting for the instance to become ready
		// is done by polling, not by holding one request open.
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// APIError is a refusal the server explained.
//
// The code and message are surfaced verbatim in Terraform's output because they
// are written for whoever is holding the key: "free tier instances are one per
// person and can only be created while signed in" tells somebody what to do,
// where "422" does not.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Code, e.Status)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// IsNotFound reports whether the resource is gone, which Terraform treats as
// "removed outside of Terraform" rather than as a failure.
func (e *APIError) IsNotFound() bool { return e.Status == http.StatusNotFound }

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s %s: %w", method, path, err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if res.StatusCode >= 400 {
		apiErr := &APIError{Status: res.StatusCode}
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			apiErr.Code = envelope.Error.Code
			apiErr.Message = envelope.Error.Message
		}
		return apiErr
	}

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decoding response from %s %s: %w", method, path, err)
		}
	}
	return nil
}

// Instance is the shape the API returns. Fields the provider does not surface
// are still decoded, so adding an attribute later needs no client change.
type Instance struct {
	ID                    string  `json:"id"`
	Name                  *string `json:"name"`
	Tier                  string  `json:"tier"`
	Cloud                 string  `json:"cloud"`
	Region                string  `json:"region"`
	DataSizeGB            int64   `json:"data_size_gb"`
	ThroughputMode        string  `json:"throughput_mode"`
	ReplicaCount          int64   `json:"replica_count"`
	Status                string  `json:"status"`
	Endpoint              *string `json:"endpoint"`
	PublicEndpoint        *string `json:"public_endpoint"`
	KividbVersion         *string `json:"kividb_version"`
	AofEnabled            bool    `json:"aof_enabled"`
	LuaEnabled            bool    `json:"lua_enabled"`
	TLSEnabled            bool    `json:"tls_enabled"`
	TLSOnly               bool    `json:"tls_only"`
	FreeTier              bool    `json:"free_tier"`
	PrivateNetworkEnabled bool    `json:"private_network_enabled"`
	OrgID                 *string `json:"org_id"`
	CreatedAt             string  `json:"created_at"`

	// Bring your own cloud. All null/empty on a database KiviDB hosts.
	CloudAccountID          *string  `json:"cloud_account_id"`
	PrivateEndpoint         *string  `json:"private_endpoint"`
	PrivateReadonlyEndpoint *string  `json:"private_readonly_endpoint"`
	PrivateReplicaEndpoints []string `json:"private_replica_endpoints"`
	BillingModel            *string  `json:"billing_model"`
}

type CreateInstanceRequest struct {
	Name           *string `json:"name,omitempty"`
	Tier           string  `json:"tier"`
	Cloud          string  `json:"cloud"`
	Region         string  `json:"region"`
	DataSizeGB     int64   `json:"data_size_gb"`
	ThroughputMode string  `json:"throughput_mode"`
	ReplicaCount   int64   `json:"replica_count"`
	KividbVersion  *string `json:"kividb_version,omitempty"`
	AofEnabled     *bool   `json:"aof_enabled,omitempty"`
	LuaEnabled     *bool   `json:"lua_enabled,omitempty"`
	TLSEnabled     *bool   `json:"tls_enabled,omitempty"`
	TLSOnly        *bool   `json:"tls_only,omitempty"`
	CloudAccountID *string `json:"cloud_account_id,omitempty"`
}

type createInstanceResponse struct {
	InstanceID string `json:"instance_id"`
	JobID      string `json:"job_id"`
	Status     string `json:"status"`
}

// CreateInstance queues the provisioning and returns the new id.
//
// The idempotency key matters more here than anywhere else in the provider: a
// create whose response is lost to a timeout would otherwise be retried into a
// second instance, and the customer would be billed for one they never see in
// state. The server stores the response against the key and replays it.
func (c *Client) CreateInstance(ctx context.Context, req CreateInstanceRequest, idempotencyKey string) (string, error) {
	var out createInstanceResponse
	path := "/instances"
	if err := c.doWithIdempotency(ctx, http.MethodPost, path, req, &out, idempotencyKey); err != nil {
		return "", err
	}
	return out.InstanceID, nil
}

func (c *Client) doWithIdempotency(ctx context.Context, method, path string, body any, out any, key string) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if res.StatusCode >= 400 {
		apiErr := &APIError{Status: res.StatusCode}
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			apiErr.Code = envelope.Error.Code
			apiErr.Message = envelope.Error.Message
		}
		return apiErr
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// GetInstance reads one instance.
//
// The response is wrapped -- `{"instance": {...}}` -- where the list endpoint
// wraps in `{"instances": [...]}`. Decoding it flat silently produced an
// Instance with every field at its zero value, which Terraform then reported as
// the provider returning "aws" as "" after an apply. Found by running a real
// apply; a unit test now pins the shape.
func (c *Client) GetInstance(ctx context.Context, id string) (*Instance, error) {
	var wrapper struct {
		Instance Instance `json:"instance"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances/"+id, nil, &wrapper); err != nil {
		return nil, err
	}
	if wrapper.Instance.ID == "" {
		return nil, fmt.Errorf("the API returned no instance for %s", id)
	}
	return &wrapper.Instance, nil
}

// Rename is the only thing PATCH does. The narrowness is deliberate on the
// server and is what the provider relies on: a rename cannot accidentally carry
// a resize with it.
func (c *Client) Rename(ctx context.Context, id, name string) error {
	return c.do(ctx, http.MethodPatch, "/instances/"+id, map[string]string{"name": name}, nil)
}

// Resize changes the disk, the throughput mode, or both in one call.
type ResizeRequest struct {
	DataSizeGB     *int64  `json:"data_size_gb,omitempty"`
	ThroughputMode *string `json:"throughput_mode,omitempty"`
}

func (c *Client) Resize(ctx context.Context, id string, req ResizeRequest) error {
	return c.do(ctx, http.MethodPost, "/instances/"+id+"/resize", req, nil)
}

// ScaleReplicas takes 1..3. Zero is not a value this endpoint accepts -- going
// from replicated back to single-node is not a scale, it is a different shape of
// instance -- which the resource turns into a clear error rather than a 400.
func (c *Client) ScaleReplicas(ctx context.Context, id string, count int64) error {
	return c.do(ctx, http.MethodPost, "/instances/"+id+"/replicas",
		map[string]int64{"replica_count": count}, nil)
}

// UpgradeToPro moves an Essentials database to Pro.
//
// The endpoint does not take a tier. It is the tier change -- Pro is the only
// destination -- and what it needs is the replica count to come up with, because
// a Pro database is replicated and Essentials is not. Sending {"tier": "pro"}
// got a 500 with "replica_count: Required" buried in the server's logs.
func (c *Client) UpgradeToPro(ctx context.Context, id string, replicaCount int64) error {
	return c.do(ctx, http.MethodPost, "/instances/"+id+"/upgrade",
		map[string]int64{"replica_count": replicaCount}, nil)
}

// UpgradeBinary changes the engine version and the two flags that travel with
// it, because all three are applied by the same restart.
type BinaryRequest struct {
	KividbVersion string `json:"kividb_version"`
	LuaEnabled    *bool  `json:"lua_enabled,omitempty"`
	TLSEnabled    *bool  `json:"tls_enabled,omitempty"`
}

func (c *Client) UpgradeBinary(ctx context.Context, id string, req BinaryRequest) error {
	return c.do(ctx, http.MethodPost, "/instances/"+id+"/binary", req, nil)
}

func (c *Client) DeleteInstance(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/instances/"+id, map[string]any{}, nil)
}

// Whoami is used once at configure time so a bad key fails with "that key is not
// valid" at plan, rather than as a confusing 401 partway through an apply.
func (c *Client) Whoami(ctx context.Context) error {
	var out struct {
		Instances []Instance `json:"instances"`
	}
	return c.do(ctx, http.MethodGet, "/instances", nil, &out)
}

// flexFloat reads a number that may arrive as a JSON string.
//
// size_gb is NUMERIC in Postgres, and the driver renders NUMERIC as a string to
// avoid the precision loss float64 would introduce. So the same field arrives as
// 8 from one endpoint and "8.00" from another, and a plain *float64 fails the
// whole response with "cannot unmarshal string into float64" -- which surfaced
// as a snapshot that had been taken successfully and then could not be read
// back.
//
// Tolerant on the way in rather than strict: the provider does not get to
// choose how the server spells its numbers.
type flexFloat struct{ Value *float64 }

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		f.Value = nil
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("reading %q as a number: %w", s, err)
	}
	f.Value = &v
	return nil
}

// DiskSnapshot is a point-in-time copy of an instance's data volume.
type DiskSnapshot struct {
	ID           string    `json:"id"`
	InstanceID   string    `json:"instance_id"`
	InstanceName string    `json:"instance_name"`
	Label        string    `json:"label"`
	Cloud        string    `json:"cloud"`
	Region       string    `json:"region"`
	SizeGB       flexFloat `json:"size_gb"`
	Status       string    `json:"status"`
	TriggeredBy  string    `json:"triggered_by"`
	CreatedAt    string    `json:"created_at"`
	CompletedAt  *string   `json:"completed_at"`
	ErrorMessage *string   `json:"error_message"`
}

// CreateDiskSnapshot queues a snapshot and returns the row as it exists before
// the volume copy has run, so the id is usable immediately.
func (c *Client) CreateDiskSnapshot(ctx context.Context, instanceID, label string) (*DiskSnapshot, error) {
	body := map[string]string{}
	if label != "" {
		body["label"] = label
	}
	var out struct {
		Snapshot   *DiskSnapshot `json:"snapshot"`
		SnapshotID string        `json:"disk_snapshot_id"`
		ID         string        `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/instances/"+instanceID+"/disk-snapshots", body, &out); err != nil {
		return nil, err
	}
	if out.Snapshot != nil {
		return out.Snapshot, nil
	}
	// The create reports only an id; read the row back so every computed
	// attribute comes from the server rather than from what we hoped it wrote.
	id := out.SnapshotID
	if id == "" {
		id = out.ID
	}
	if id == "" {
		return nil, fmt.Errorf("the server accepted the snapshot but named no id")
	}
	return c.GetDiskSnapshot(ctx, id)
}

// GetDiskSnapshot reads one snapshot. A snapshot that is gone answers 404, which
// APIError.IsNotFound reports, so the resource can drop from state instead of
// failing an apply.
func (c *Client) GetDiskSnapshot(ctx context.Context, id string) (*DiskSnapshot, error) {
	var out struct {
		Snapshot DiskSnapshot `json:"snapshot"`
	}
	if err := c.do(ctx, http.MethodGet, "/disk-snapshots/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out.Snapshot, nil
}

// RenameDiskSnapshot changes the label, which is the only mutable field.
func (c *Client) RenameDiskSnapshot(ctx context.Context, id, label string) error {
	return c.do(ctx, http.MethodPatch, "/disk-snapshots/"+id, map[string]string{"label": label}, nil)
}

func (c *Client) DeleteDiskSnapshot(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/disk-snapshots/"+id, nil, nil)
}

// CloudAccount is a cloud account the organization connected in the console,
// so databases can run inside it.
//
// The API calls the name `display_name`; `name` is accepted as well so the
// provider keeps reading if the field is ever spelled the short way.
type CloudAccount struct {
	ID             string   `json:"id"`
	DisplayName    string   `json:"display_name"`
	Name           string   `json:"name"`
	Cloud          string   `json:"cloud"`
	AWSAccountID   *string  `json:"aws_account_id"`
	GCPProjectID   *string  `json:"gcp_project_id"`
	EnabledRegions []string `json:"enabled_regions"`
	Status         string   `json:"status"`
}

// Label is the account's name as the console shows it.
func (a CloudAccount) Label() string {
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return a.Name
}

// ListCloudAccounts returns the organization's connected cloud accounts.
// Disconnected ones are not listed: nothing can be placed in them.
func (c *Client) ListCloudAccounts(ctx context.Context) ([]CloudAccount, error) {
	var out struct {
		CloudAccounts []CloudAccount `json:"cloud_accounts"`
	}
	if err := c.do(ctx, http.MethodGet, "/cloud-accounts", nil, &out); err != nil {
		return nil, err
	}
	return out.CloudAccounts, nil
}
