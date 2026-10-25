package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Backups: export destinations (a bucket in the customer's own AWS account that
// KDB exports are delivered to), a database's export configuration, KDB
// snapshots, and the listing of disk snapshots.

// ExportDestinationSetup is what the customer creates on their side: a role
// with this trust policy and this permission policy. The API builds both from
// the destination itself, and verification expects exactly them.
type ExportDestinationSetup struct {
	RoleName         string          `json:"role_name"`
	ExternalID       string          `json:"external_id"`
	TrustPolicy      json.RawMessage `json:"trust_policy"`
	PermissionPolicy json.RawMessage `json:"permission_policy"`
}

// ExportDestination is a bucket KDB exports are delivered into.
type ExportDestination struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Cloud        string                 `json:"cloud"`
	AWSAccountID string                 `json:"aws_account_id"`
	Bucket       string                 `json:"bucket"`
	BucketRegion *string                `json:"bucket_region"`
	Prefix       string                 `json:"prefix"`
	Status       string                 `json:"status"`
	VerifiedAt   *string                `json:"verified_at"`
	LastError    *string                `json:"last_error"`
	CreatedAt    string                 `json:"created_at"`
	Setup        ExportDestinationSetup `json:"setup"`
}

// RoleArn is the role KiviDB assumes, in the destination's account. The API
// stores it but returns only the name; the ARN is the two put together, the same
// way the API built it (exportRoleArn in the shared package).
func (d ExportDestination) RoleArn() string {
	if d.Setup.RoleName == "" {
		return ""
	}
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", d.AWSAccountID, d.Setup.RoleName)
}

type CreateExportDestinationRequest struct {
	Name         string  `json:"name"`
	AWSAccountID string  `json:"aws_account_id"`
	Bucket       string  `json:"bucket"`
	Prefix       *string `json:"prefix,omitempty"`
	RoleName     *string `json:"role_name,omitempty"`
}

func (c *Client) CreateExportDestination(ctx context.Context, req CreateExportDestinationRequest) (*ExportDestination, error) {
	var out struct {
		Destination *ExportDestination `json:"destination"`
	}
	if err := c.do(ctx, http.MethodPost, "/export-destinations", req, &out); err != nil {
		return nil, err
	}
	if out.Destination == nil || out.Destination.ID == "" {
		return nil, errors.New("the API accepted the export destination but returned none")
	}
	return out.Destination, nil
}

func (c *Client) ListExportDestinations(ctx context.Context) ([]ExportDestination, error) {
	var out struct {
		Destinations []ExportDestination `json:"destinations"`
	}
	if err := c.do(ctx, http.MethodGet, "/export-destinations", nil, &out); err != nil {
		return nil, err
	}
	return out.Destinations, nil
}

// GetExportDestination reads one destination.
//
// The API has no read of one destination, only the organization's list, so
// this finds it there. A destination the list does not have answers a 404
// APIError, the same as every other read, so callers drop it from state the
// same way.
func (c *Client) GetExportDestination(ctx context.Context, id string) (*ExportDestination, error) {
	all, err := c.ListExportDestinations(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if strings.EqualFold(all[i].ID, id) {
			return &all[i], nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Code: "not_found"}
}

// VerificationError is a verification that ran and did not pass. The step
// names where it stopped (assume_role, session_account, bucket, write) and the
// message says what to fix, both as the API wrote them.
type VerificationError struct {
	Step        string
	Message     string
	Destination *ExportDestination
}

func (e *VerificationError) Error() string {
	if e.Step == "" {
		return e.Message
	}
	return e.Step + ": " + e.Message
}

// VerifyExportDestination asks KiviDB to assume the role, find the bucket and
// write a test file under the prefix.
//
// A failed verification is a 422 that still carries the destination (now
// `failed`, with last_error set), so it comes back as a *VerificationError
// rather than an APIError: the caller can tell "it ran and did not pass" from
// "it could not be run at all". The id goes in lower case, for the reason
// given on SetExportConfig.
func (c *Client) VerifyExportDestination(ctx context.Context, id string) (*ExportDestination, error) {
	status, raw, err := c.doRaw(ctx, http.MethodPost, "/export-destinations/"+strings.ToLower(id)+"/verify", map[string]any{})
	if err != nil {
		return nil, err
	}
	var body struct {
		Destination *ExportDestination `json:"destination"`
		Error       struct {
			Code    string `json:"code"`
			Step    string `json:"step"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	if status >= 400 {
		if body.Error.Code == "verification_failed" {
			return nil, &VerificationError{Step: body.Error.Step, Message: body.Error.Message, Destination: body.Destination}
		}
		return nil, decodeAPIError(status, raw)
	}
	if body.Destination == nil {
		return nil, errors.New("the API verified the export destination but returned none")
	}
	return body.Destination, nil
}

// doRaw is `do` for the one endpoint whose refusal carries a body worth more
// than its error envelope: it returns the status and the bytes, and leaves
// what a 4xx means to the caller.
func (c *Client) doRaw(ctx context.Context, method, path string, body any) (int, []byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("calling %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("reading response: %w", err)
	}
	return res.StatusCode, raw, nil
}

func (c *Client) DeleteExportDestination(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/export-destinations/"+strings.ToLower(id), nil, nil)
}

// ExportConfig is where a database's exports go and how often it exports on
// its own. A nil destination is none.
type ExportConfig struct {
	ExportDestinationID *string `json:"export_destination_id"`
	KdbExportCadence    string  `json:"kdb_export_cadence"`
}

// InstanceExport is the part of the instance read the export configuration
// lives in, plus what decides whether a schedule can run at all.
type InstanceExport struct {
	ID                  string  `json:"id"`
	ExportDestinationID *string `json:"export_destination_id"`
	KdbExportCadence    string  `json:"kdb_export_cadence"`
	FreeTier            bool    `json:"free_tier"`
	ShardCount          *int64  `json:"shard_count"`
}

func (c *Client) GetInstanceExport(ctx context.Context, instanceID string) (*InstanceExport, error) {
	var wrapper struct {
		Instance InstanceExport `json:"instance"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances/"+instanceID, nil, &wrapper); err != nil {
		return nil, err
	}
	if wrapper.Instance.ID == "" {
		return nil, fmt.Errorf("the API returned no instance for %s", instanceID)
	}
	if wrapper.Instance.KdbExportCadence == "" {
		// An API from before export destinations has no such column; it is
		// "off" in every sense that matters.
		wrapper.Instance.KdbExportCadence = "off"
	}
	return &wrapper.Instance, nil
}

// SetExportConfig sends both fields every time. The endpoint keeps whatever is
// omitted, so a destination Terraform wants removed has to be sent as an
// explicit null, never left out.
//
// Ids are sent in lower case. These routes compare `id::text = $1`, and Postgres
// writes a uuid in lower case, so an id configured in capitals -- which every
// other route accepts -- would be answered "not found" or "not verified".
func (c *Client) SetExportConfig(ctx context.Context, instanceID string, cfg ExportConfig) (*ExportConfig, error) {
	if cfg.ExportDestinationID != nil {
		lower := strings.ToLower(*cfg.ExportDestinationID)
		cfg.ExportDestinationID = &lower
	}
	var out ExportConfig
	if err := c.do(ctx, http.MethodPatch, "/instances/"+strings.ToLower(instanceID)+"/export-config", cfg, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// KdbSnapshot is a database's data exported as a portable .kdb file.
type KdbSnapshot struct {
	ID               string    `json:"id"`
	InstanceID       *string   `json:"instance_id"`
	InstanceName     *string   `json:"instance_name"`
	Label            *string   `json:"label"`
	SizeBytes        flexFloat `json:"size_bytes"`
	Status           string    `json:"status"`
	TriggeredBy      string    `json:"triggered_by"`
	Cloud            *string   `json:"cloud"`
	Region           *string   `json:"region"`
	TTD              *string   `json:"ttd"`
	CreatedAt        string    `json:"created_at"`
	CompletedAt      *string   `json:"completed_at"`
	ErrorMessage     *string   `json:"error_message"`
	CloudAccountID   *string   `json:"cloud_account_id"`
	CloudAccountName *string   `json:"cloud_account_name"`
	ShardCount       *int64    `json:"shard_count"`

	// Delivery into the organization's own bucket; all null when the export
	// had no destination.
	DestinationID  *string `json:"destination_id"`
	DeliveryStatus *string `json:"delivery_status"`
	DeliveredURI   *string `json:"delivered_uri"`
	DeliveredAt    *string `json:"delivered_at"`
	DeliveryError  *string `json:"delivery_error"`
}

type ExportKdbSnapshotRequest struct {
	Label   *string `json:"label,omitempty"`
	Deliver *bool   `json:"deliver,omitempty"`
}

// ExportKdbSnapshot queues an export and returns its id. The file is written
// by a job; the row reads `creating` until it is.
func (c *Client) ExportKdbSnapshot(ctx context.Context, instanceID string, req ExportKdbSnapshotRequest) (string, error) {
	var out struct {
		ID string `json:"kdb_snapshot_id"`
	}
	if err := c.do(ctx, http.MethodPost, "/instances/"+instanceID+"/kdb-snapshots/export", req, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("the API accepted the export but named no snapshot id")
	}
	return out.ID, nil
}

// ListKdbSnapshots lists the organization's KDB snapshots, or one database's
// when instanceID is set. Newest first; deleted ones are not listed.
func (c *Client) ListKdbSnapshots(ctx context.Context, instanceID string) ([]KdbSnapshot, error) {
	path := "/kdb-snapshots"
	if instanceID != "" {
		path = "/instances/" + instanceID + "/kdb-snapshots"
	}
	var out struct {
		Snapshots []KdbSnapshot `json:"snapshots"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Snapshots, nil
}

// GetKdbSnapshot reads one KDB snapshot.
//
// There is no read of one KDB snapshot, so this finds it in the organization's
// list -- not the database's, because a snapshot outlives its database (the
// row's instance_id is set to null when the database is deleted) and must not
// look deleted just because its database is.
func (c *Client) GetKdbSnapshot(ctx context.Context, id string) (*KdbSnapshot, error) {
	all, err := c.ListKdbSnapshots(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range all {
		if strings.EqualFold(all[i].ID, id) {
			return &all[i], nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Code: "not_found"}
}

func (c *Client) DeleteKdbSnapshot(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/kdb-snapshots/"+id, nil, nil)
}

// DiskSnapshotListItem is a disk snapshot as the listings return it.
//
// The organization's list calls the label `snapshot_label` and the database's
// list calls it `label`; both are read.
type DiskSnapshotListItem struct {
	ID                   string    `json:"id"`
	InstanceID           *string   `json:"instance_id"`
	InstanceName         *string   `json:"instance_name"`
	ResolvedInstanceName *string   `json:"resolved_instance_name"`
	Label                *string   `json:"label"`
	SnapshotLabel        *string   `json:"snapshot_label"`
	Cloud                string    `json:"cloud"`
	Region               string    `json:"region"`
	SizeGB               flexFloat `json:"size_gb"`
	Status               string    `json:"status"`
	TriggeredBy          string    `json:"triggered_by"`
	TTD                  *string   `json:"ttd"`
	CreatedAt            string    `json:"created_at"`
	CompletedAt          *string   `json:"completed_at"`
	ErrorMessage         *string   `json:"error_message"`
	CloudAccountID       *string   `json:"cloud_account_id"`
	CloudAccountName     *string   `json:"cloud_account_name"`
}

func (s DiskSnapshotListItem) label() *string {
	if s.Label != nil {
		return s.Label
	}
	return s.SnapshotLabel
}

func (c *Client) ListDiskSnapshots(ctx context.Context, instanceID string) ([]DiskSnapshotListItem, error) {
	path := "/disk-snapshots"
	if instanceID != "" {
		path = "/instances/" + instanceID + "/disk-snapshots"
	}
	var out struct {
		Snapshots []DiskSnapshotListItem `json:"snapshots"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Snapshots, nil
}

// compactJSON renders a policy document on one line, in the order the API
// wrote it, so the same document reads back as the same string.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
