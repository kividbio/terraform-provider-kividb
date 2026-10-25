package provider

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// DataPipeline is a data integration pipeline as GET/PUT /data-integration/:name
// return it (apps/api/src/routes/data-integration.ts, view()). The source
// password is never in it: the API stores it sealed and does not return it.
type DataPipeline struct {
	Name     string `json:"name"`
	Instance struct {
		ID     string  `json:"id"`
		Name   *string `json:"name"`
		Status *string `json:"status"`
	} `json:"instance"`
	SourceType     string              `json:"source_type"`
	SourceHost     string              `json:"source_host"`
	SourcePort     int64               `json:"source_port"`
	SourceDatabase string              `json:"source_database"`
	SourceUser     string              `json:"source_user"`
	SourceTLS      string              `json:"source_tls"`
	Tables         []DataPipelineTable `json:"tables"`

	DesiredState        string  `json:"desired_state"`
	Status              string  `json:"status"`
	SlotName            *string `json:"slot_name"`
	SnapshotDone        bool    `json:"snapshot_done"`
	RowsSnapshotted     int64   `json:"rows_snapshotted"`
	ChangesApplied      int64   `json:"changes_applied"`
	LastChangeAt        *string `json:"last_change_at"`
	LagMS               *int64  `json:"lag_ms"`
	LastError           *string `json:"last_error"`
	LastErrorAt         *string `json:"last_error_at"`
	ConsecutiveFailures int64   `json:"consecutive_failures"`
	RunningNow          bool    `json:"running_now"`
	CreatedAt           string  `json:"created_at"`
	UpdatedAt           string  `json:"updated_at"`
}

// DataPipelineTable is one table mapping (dataTableMappingSchema in
// packages/shared/src/data-integration.ts). The API stores the mapping as it
// was sent, after defaults, so what is left out here comes back left out.
type DataPipelineTable struct {
	Table      string             `json:"table"`
	Key        string             `json:"key"`
	Type       string             `json:"type"`
	Columns    []string           `json:"columns,omitempty"`
	Rename     *map[string]string `json:"rename,omitempty"`
	TTLSeconds *int64             `json:"ttl_seconds,omitempty"`
}

// PutDataPipelineRequest is dataPipelineBodySchema. The name travels in the path.
type PutDataPipelineRequest struct {
	InstanceID     string              `json:"instance_id"`
	SourceType     string              `json:"source_type"`
	SourceHost     string              `json:"source_host"`
	SourcePort     int64               `json:"source_port"`
	SourceDatabase string              `json:"source_database"`
	SourceUser     string              `json:"source_user"`
	SourcePassword *string             `json:"source_password,omitempty"`
	SourceTLS      string              `json:"source_tls"`
	Tables         []DataPipelineTable `json:"tables"`
}

// DeleteDataPipelineResult says what a delete left behind. SlotLeftInSource is
// set only after a forced delete whose cleanup failed: the API keeps trying to
// drop that slot for about four days.
type DeleteDataPipelineResult struct {
	Deleted          bool   `json:"deleted"`
	SlotLeftInSource string `json:"slot_left_in_source"`
}

// slow returns a copy of the client with a longer timeout.
//
// A PUT connects to the customer's source database before it answers (up to
// 25 s), and a DELETE first waits up to 30 s for the runner holding the
// pipeline to let go and then up to 25 s for the source to drop its
// replication slot. Both can pass the client's usual 60 s, and a response lost
// to our own timeout is a change made with nobody told.
func (c *Client) slow() *Client {
	cp := *c
	cp.http = &http.Client{Timeout: 3 * time.Minute}
	return &cp
}

func pipelinePath(name string) string {
	return "/data-integration/" + url.PathEscape(name)
}

// GetDataPipeline reads one pipeline. A pipeline that does not exist answers
// 404 pipeline_not_found.
func (c *Client) GetDataPipeline(ctx context.Context, name string) (*DataPipeline, error) {
	var out struct {
		Pipeline *DataPipeline `json:"pipeline"`
	}
	if err := c.do(ctx, http.MethodGet, pipelinePath(name), nil, &out); err != nil {
		return nil, err
	}
	if out.Pipeline == nil || out.Pipeline.Name == "" {
		return nil, &APIError{Status: http.StatusNotFound, Code: "pipeline_not_found", Message: "No such pipeline."}
	}
	return out.Pipeline, nil
}

// PutDataPipeline creates or changes a pipeline. It is an upsert on the name:
// the caller decides whether that is meant to be a create.
func (c *Client) PutDataPipeline(ctx context.Context, name string, req PutDataPipelineRequest) (*DataPipeline, error) {
	var out struct {
		Pipeline *DataPipeline `json:"pipeline"`
	}
	if err := c.slow().do(ctx, http.MethodPut, pipelinePath(name), req, &out); err != nil {
		return nil, err
	}
	if out.Pipeline == nil {
		return c.GetDataPipeline(ctx, name)
	}
	return out.Pipeline, nil
}

func (c *Client) pipelineAction(ctx context.Context, name, action string) (*DataPipeline, error) {
	var out struct {
		Pipeline *DataPipeline `json:"pipeline"`
	}
	if err := c.do(ctx, http.MethodPost, pipelinePath(name)+"/"+action, nil, &out); err != nil {
		return nil, err
	}
	if out.Pipeline == nil {
		return c.GetDataPipeline(ctx, name)
	}
	return out.Pipeline, nil
}

// StartDataPipeline runs a stopped pipeline. Refused with 402
// insufficient_credits when the organization's credit cannot cover it.
func (c *Client) StartDataPipeline(ctx context.Context, name string) (*DataPipeline, error) {
	return c.pipelineAction(ctx, name, "start")
}

// StopDataPipeline stops it. A PostgreSQL source keeps the replication slot,
// and so its WAL, until the pipeline is started again or deleted.
func (c *Client) StopDataPipeline(ctx context.Context, name string) (*DataPipeline, error) {
	return c.pipelineAction(ctx, name, "stop")
}

// ResnapshotDataPipeline copies every table again from the start.
func (c *Client) ResnapshotDataPipeline(ctx context.Context, name string) (*DataPipeline, error) {
	return c.pipelineAction(ctx, name, "resnapshot")
}

// DeleteDataPipeline deletes a pipeline and the replication slot it made in the
// source. Without force, a slot that cannot be dropped refuses the delete with
// 409 source_cleanup_failed (and leaves the pipeline stopped).
func (c *Client) DeleteDataPipeline(ctx context.Context, name string, force bool) (*DeleteDataPipelineResult, error) {
	p := pipelinePath(name)
	if force {
		p += "?force=true"
	}
	var out DeleteDataPipelineResult
	if err := c.slow().do(ctx, http.MethodDelete, p, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
