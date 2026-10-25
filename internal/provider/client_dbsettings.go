package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Database-level settings: ACL users, the maintenance window, scheduled disk
// snapshots, encrypted replication and client certificates, and the TLS CA.
//
// The instance's own fields that these settings add are kept here and embedded
// in Instance, CreateInstanceRequest and BinaryRequest, so the instance types
// stay where they are and each feature's fields stay with the feature.

// instanceSettingsFields is what GET /instances/:id returns about the settings
// in this file. The route answers `SELECT i.*`, so every column is present on a
// current server; on an older one a missing field decodes to its zero value,
// which the readers below treat as the API's default.
type instanceSettingsFields struct {
	ShardCount int64 `json:"shard_count"`

	ReplicationTLS bool    `json:"replication_tls"`
	ClientCertMode string  `json:"client_cert_mode"`
	ClientCaPem    *string `json:"client_ca_pem"`

	MaintenanceDay      *int64  `json:"maintenance_day"`
	MaintenanceHour     *int64  `json:"maintenance_hour"`
	MaintenanceTimezone *string `json:"maintenance_timezone"`

	DiskSnapshotEnabled       bool    `json:"disk_snapshot_enabled"`
	DiskSnapshotCadence       *string `json:"disk_snapshot_cadence"`
	DiskSnapshotRetentionDays *int64  `json:"disk_snapshot_retention_days"`
	DiskSnapshotRetentionMode *string `json:"disk_snapshot_retention_mode"`
}

// shards is the shard count with an older server's missing field read as one:
// a database that predates sharding is a single shard.
func (f instanceSettingsFields) shards() int64 {
	if f.ShardCount < 1 {
		return 1
	}
	return f.ShardCount
}

// clientCertMode reads an absent mode as "off", which is the column's default.
func (f instanceSettingsFields) clientCertMode() string {
	if f.ClientCertMode == "" {
		return "off"
	}
	return f.ClientCertMode
}

// createSettingsFields are the create-time fields added with these settings.
// Both are sent only when they say something other than the default, so a
// create that uses neither sends exactly the body it always did.
type createSettingsFields struct {
	FreeTier   *bool  `json:"free_tier,omitempty"`
	ShardCount *int64 `json:"shard_count,omitempty"`
}

// binarySettingsFields travel with the engine binary because the same restart
// applies them: the node's start command carries the TLS flags for replication
// and for client certificates.
type binarySettingsFields struct {
	ReplicationTLS *bool   `json:"replication_tls,omitempty"`
	ClientCertMode *string `json:"client_cert_mode,omitempty"`
	ClientCaPem    *string `json:"client_ca_pem,omitempty"`
}

// --- ACL users ----------------------------------------------------------------

// DatabaseUser is an ACL user on one database. The list endpoint never carries
// the password; it is read separately, by name.
type DatabaseUser struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Profile   string `json:"profile"`
	CreatedAt string `json:"created_at"`
}

type CreateDatabaseUserRequest struct {
	Username string  `json:"username"`
	Profile  string  `json:"profile"`
	Password *string `json:"password,omitempty"`
}

type createDatabaseUserResponse struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Profile  string `json:"profile"`
	Password string `json:"password"`
	JobID    string `json:"job_id"`
}

// UpdateDatabaseUserRequest changes the profile, the password, or both. A
// rotation asks the API to generate a new password; it is ignored when a
// password is given.
type UpdateDatabaseUserRequest struct {
	Profile        *string `json:"profile,omitempty"`
	Password       *string `json:"password,omitempty"`
	RotatePassword *bool   `json:"rotate_password,omitempty"`
}

type updateDatabaseUserResponse struct {
	Username string  `json:"username"`
	Profile  string  `json:"profile"`
	Password *string `json:"password"`
}

func aclUserPath(instanceID, username string) string {
	return "/instances/" + instanceID + "/acl-users/" + url.PathEscape(username)
}

// ListDatabaseUsers lists a database's ACL users. A missing database is a 404,
// which the callers read as "gone".
func (c *Client) ListDatabaseUsers(ctx context.Context, instanceID string) ([]DatabaseUser, error) {
	var out struct {
		Users []DatabaseUser `json:"users"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances/"+instanceID+"/acl-users", nil, &out); err != nil {
		return nil, err
	}
	return out.Users, nil
}

// GetDatabaseUser finds one user by name. Not listed is reported as a 404, so a
// user deleted in the console reads the same as a database that is gone.
func (c *Client) GetDatabaseUser(ctx context.Context, instanceID, username string) (*DatabaseUser, error) {
	users, err := c.ListDatabaseUsers(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.Username == username {
			u := u
			return &u, nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Code: "not_found",
		Message: fmt.Sprintf("database %s has no user %q", instanceID, username)}
}

// DatabaseUserPassword reads the password the API holds for a user.
func (c *Client) DatabaseUserPassword(ctx context.Context, instanceID, username string) (string, error) {
	var out struct {
		Password string `json:"password"`
	}
	if err := c.do(ctx, http.MethodGet, aclUserPath(instanceID, username)+"/password", nil, &out); err != nil {
		return "", err
	}
	return out.Password, nil
}

// CreateDatabaseUser returns the stored user and the password it was given,
// which is generated by the API when the request named none.
func (c *Client) CreateDatabaseUser(ctx context.Context, instanceID string, req CreateDatabaseUserRequest) (*DatabaseUser, string, error) {
	var out createDatabaseUserResponse
	if err := c.do(ctx, http.MethodPost, "/instances/"+instanceID+"/acl-users", req, &out); err != nil {
		return nil, "", err
	}
	return &DatabaseUser{ID: out.ID, Username: out.Username, Profile: out.Profile}, out.Password, nil
}

// UpdateDatabaseUser returns the new password when the change set one, and ""
// when it changed only the profile.
func (c *Client) UpdateDatabaseUser(ctx context.Context, instanceID, username string, req UpdateDatabaseUserRequest) (string, error) {
	var out updateDatabaseUserResponse
	if err := c.do(ctx, http.MethodPatch, aclUserPath(instanceID, username), req, &out); err != nil {
		return "", err
	}
	if out.Password == nil {
		return "", nil
	}
	return *out.Password, nil
}

func (c *Client) DeleteDatabaseUser(ctx context.Context, instanceID, username string) error {
	return c.do(ctx, http.MethodDelete, aclUserPath(instanceID, username), map[string]any{}, nil)
}

// --- Maintenance window -------------------------------------------------------

type MaintenanceWindow struct {
	Day         int64  `json:"day"`
	Hour        int64  `json:"hour"`
	Timezone    string `json:"timezone"`
	LengthHours int64  `json:"length_hours,omitempty"`
	Description string `json:"description,omitempty"`
}

// PlannedMaintenance is work KiviDB has scheduled inside the window.
type PlannedMaintenance struct {
	Kind          string `json:"kind"`
	StartsAt      string `json:"starts_at"`
	RestartsNodes bool   `json:"restarts_nodes"`
}

// Maintenance is GET /instances/:id/maintenance, which PATCH
// /instances/:id/maintenance-window also answers with.
type Maintenance struct {
	Window          MaintenanceWindow    `json:"window"`
	NoticeHours     int64                `json:"notice_hours"`
	NextWindowStart string               `json:"next_window_start"`
	Planned         []PlannedMaintenance `json:"planned"`
}

func (c *Client) GetMaintenance(ctx context.Context, instanceID string) (*Maintenance, error) {
	var out Maintenance
	if err := c.do(ctx, http.MethodGet, "/instances/"+instanceID+"/maintenance", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetMaintenanceWindow moves the window. The API clears maintenance already
// planned and plans it again in the new window, with fresh notice.
func (c *Client) SetMaintenanceWindow(ctx context.Context, instanceID string, day, hour int64, timezone string) (*Maintenance, error) {
	body := map[string]any{"day": day, "hour": hour, "timezone": timezone}
	var out Maintenance
	if err := c.do(ctx, http.MethodPatch, "/instances/"+instanceID+"/maintenance-window", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Scheduled disk snapshots -----------------------------------------------

// DiskSnapshotConfig is PATCH /instances/:id/disk-snapshot-config. Every field
// is optional on the API, and a field left out keeps its value.
type DiskSnapshotConfig struct {
	Enabled       *bool   `json:"disk_snapshot_enabled,omitempty"`
	Cadence       *string `json:"disk_snapshot_cadence,omitempty"`
	RetentionDays *int64  `json:"disk_snapshot_retention_days,omitempty"`
	RetentionMode *string `json:"disk_snapshot_retention_mode,omitempty"`
}

func (c *Client) SetDiskSnapshotConfig(ctx context.Context, instanceID string, cfg DiskSnapshotConfig) error {
	return c.do(ctx, http.MethodPatch, "/instances/"+instanceID+"/disk-snapshot-config", cfg, nil)
}

// --- TLS ----------------------------------------------------------------------

// TLSCACertificate reads the CA that signs KiviDB's server certificates.
//
// The one endpoint in the provider that answers PEM text rather than JSON, so
// it does not go through `do`, which would try to decode it. Errors still
// arrive in the JSON envelope and are decoded the same way.
func (c *Client) TLSCACertificate(ctx context.Context, instanceID string) (string, error) {
	path := "/instances/" + instanceID + "/tls/ca-cert"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/x-pem-file, application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling GET %s: %w", path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	if res.StatusCode >= 400 {
		return "", decodeAPIError(res.StatusCode, raw)
	}
	pem := string(raw)
	if !strings.Contains(pem, "-----BEGIN CERTIFICATE-----") {
		return "", errors.New("the API answered without a PEM certificate")
	}
	return pem, nil
}

type ClientCertificateSummary struct {
	Subject        string `json:"subject"`
	NotAfter       string `json:"notAfter"`
	Fingerprint256 string `json:"fingerprint256"`
}

// ClientCertificates is GET /instances/:id/client-certificates.
type ClientCertificates struct {
	Mode           string                     `json:"mode"`
	CACertificates []ClientCertificateSummary `json:"ca_certificates"`
	CAPem          *string                    `json:"ca_pem"`
	TLSEnabled     bool                       `json:"tls_enabled"`
	TLSOnly        bool                       `json:"tls_only"`
	Available      bool                       `json:"available"`
}

func (c *Client) GetClientCertificates(ctx context.Context, instanceID string) (*ClientCertificates, error) {
	var out ClientCertificates
	if err := c.do(ctx, http.MethodGet, "/instances/"+instanceID+"/client-certificates", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- helpers --------------------------------------------------------------------

// retryWhileNotRunning repeats a write the API refuses because the database is
// not running yet -- just created, or mid-change after another resource's
// apply. Anything else is returned at once, and the wait is bounded.
//
// The ACL routes refuse every write unless the database is running, and in a
// configuration that creates a database and its users together, the users are
// planned the moment the database exists. Without this the first apply fails
// for the users of a database that was created a moment ago.
func retryWhileNotRunning(ctx context.Context, timeout, interval time.Duration, op func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := op()
		var apiErr *APIError
		if err == nil || !errors.As(err, &apiErr) || !isNotRunningRefusal(apiErr) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the database was still not running after %s, and changes to it can only be "+
				"made while it runs; apply again once it is running (the API answered: %w)", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func isNotRunningRefusal(e *APIError) bool {
	return e.Status == http.StatusConflict &&
		(e.Code == "instance_not_running" || e.Code == "instance_not_ready")
}

// pemBlocks reduces PEM text to its certificate blocks, with line endings and
// surrounding whitespace normalised, for comparing what was configured against
// what the API stored. The API keeps only the blocks, joined by newlines.
func pemBlocks(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	const begin, end = "-----BEGIN CERTIFICATE-----", "-----END CERTIFICATE-----"
	var out []string
	for {
		i := strings.Index(s, begin)
		if i < 0 {
			return out
		}
		j := strings.Index(s[i:], end)
		if j < 0 {
			return out
		}
		block := s[i : i+j+len(end)]
		lines := strings.Split(block, "\n")
		for k := range lines {
			lines[k] = strings.TrimSpace(lines[k])
		}
		out = append(out, strings.Join(lines, "\n"))
		s = s[i+j+len(end):]
	}
}

func samePEM(a, b string) bool {
	x, y := pemBlocks(a), pemBlocks(b)
	if len(x) != len(y) || len(x) == 0 {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func isNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsNotFound()
}

func isForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden
}

// zodIssues is the `issues` a route that validates with safeParse sends in
// place of a message: zod's flatten(), field by field. Without reading it, a
// refused maintenance window reported "bad_request (HTTP 400)" and nothing
// about which field.
type zodIssues struct {
	FormErrors  []string            `json:"formErrors"`
	FieldErrors map[string][]string `json:"fieldErrors"`
}

func (z *zodIssues) String() string {
	var parts []string
	parts = append(parts, z.FormErrors...)
	fields := make([]string, 0, len(z.FieldErrors))
	for f := range z.FieldErrors {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	for _, f := range fields {
		for _, m := range z.FieldErrors[f] {
			parts = append(parts, f+": "+m)
		}
	}
	return strings.Join(parts, "; ")
}
