package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// The AI features: semantic cache namespaces, agent memory stores, context
// models and their agent keys. Their routes live in the API under
// routes/semantic-cache.ts, routes/agent-memory.ts and routes/context-retriever.ts;
// the request shapes are packages/shared/src/{semantic-cache,agent-memory,context-retriever}.ts.
//
// Every settings write is a PUT that creates or replaces the whole object, so
// the request types below carry every field the API reads, not a patch.

// doAI is `do` with one difference: a request the API's schema rejects answers
//
//	{"error": {"code": "bad_request", "issues": {"formErrors": [...], "fieldErrors": {"field": [...]}}}}
//
// with no message, which `do` would surface as "bad_request (HTTP 400)". Here the
// issues become the message, so the operator reads which field and why.
func (c *Client) doAI(ctx context.Context, method, path string, body any, out any) error {
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
		apiErr := decodeAPIError(res.StatusCode, raw)
		if apiErr.Message == "" {
			apiErr.Message = issuesMessage(raw)
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

// issuesMessage renders zod's flattened issues as one line, field by field in
// a stable order. Empty when the body carries none.
func issuesMessage(raw []byte) string {
	var envelope struct {
		Error struct {
			Issues struct {
				FormErrors  []string            `json:"formErrors"`
				FieldErrors map[string][]string `json:"fieldErrors"`
			} `json:"issues"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return ""
	}
	is := envelope.Error.Issues
	parts := append([]string{}, is.FormErrors...)
	fields := make([]string, 0, len(is.FieldErrors))
	for f := range is.FieldErrors {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	for _, f := range fields {
		parts = append(parts, f+": "+strings.Join(is.FieldErrors[f], ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "The API rejected the request: " + strings.Join(parts, "; ")
}

func pathEscape(s string) string { return url.PathEscape(s) }

// ── Semantic cache ──────────────────────────────────────────────────────────

// SemanticCacheNamespace is a namespace's settings as the API shows them. The
// provider's API key is never returned; HasAPIKey says whether one is stored.
type SemanticCacheNamespace struct {
	Name              string  `json:"name"`
	Provider          string  `json:"provider"`
	Model             *string `json:"model"`
	BaseURL           *string `json:"base_url"`
	HasAPIKey         bool    `json:"has_api_key"`
	Dimensions        int64   `json:"dimensions"`
	Threshold         float64 `json:"threshold"`
	DefaultTTLSeconds int64   `json:"default_ttl_seconds"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

// SemanticCacheRequest is the PUT body (semanticCacheNamespaceBodySchema).
// APIKey omitted on an update keeps the stored key, for the same provider.
type SemanticCacheRequest struct {
	Provider          string   `json:"provider"`
	Model             *string  `json:"model,omitempty"`
	BaseURL           *string  `json:"base_url,omitempty"`
	APIKey            *string  `json:"api_key,omitempty"`
	Dimensions        int64    `json:"dimensions"`
	Threshold         *float64 `json:"threshold,omitempty"`
	DefaultTTLSeconds *int64   `json:"default_ttl_seconds,omitempty"`
}

func semanticCachePath(instanceID, name string) string {
	return "/instances/" + pathEscape(instanceID) + "/semantic-cache/" + pathEscape(name)
}

func (c *Client) PutSemanticCache(ctx context.Context, instanceID, name string, req SemanticCacheRequest) (*SemanticCacheNamespace, error) {
	var out struct {
		Namespace *SemanticCacheNamespace `json:"namespace"`
	}
	if err := c.doAI(ctx, http.MethodPut, semanticCachePath(instanceID, name), req, &out); err != nil {
		return nil, err
	}
	if out.Namespace == nil {
		return nil, fmt.Errorf("the API saved the namespace but returned none")
	}
	return out.Namespace, nil
}

// GetSemanticCache reads a namespace. A missing database answers 404
// not_found and a missing namespace 404 namespace_not_found; both are gone.
func (c *Client) GetSemanticCache(ctx context.Context, instanceID, name string) (*SemanticCacheNamespace, error) {
	var out struct {
		Namespace *SemanticCacheNamespace `json:"namespace"`
	}
	if err := c.doAI(ctx, http.MethodGet, semanticCachePath(instanceID, name), nil, &out); err != nil {
		return nil, err
	}
	if out.Namespace == nil {
		return nil, fmt.Errorf("the API returned no namespace for %s", name)
	}
	return out.Namespace, nil
}

// DeleteSemanticCache deletes the namespace and every entry cached in it. The
// database must be running: the entries are removed from it.
func (c *Client) DeleteSemanticCache(ctx context.Context, instanceID, name string) error {
	return c.doAI(ctx, http.MethodDelete, semanticCachePath(instanceID, name), nil, nil)
}

// ── Agent memory ────────────────────────────────────────────────────────────

type AgentMemoryCustomType struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AgentMemoryStore is a store's settings as the API shows them. Neither API key
// is returned; the has_* flags say whether one is stored.
type AgentMemoryStore struct {
	Name                   string                  `json:"name"`
	EmbedProvider          string                  `json:"embed_provider"`
	EmbedModel             *string                 `json:"embed_model"`
	EmbedBaseURL           *string                 `json:"embed_base_url"`
	HasEmbedAPIKey         bool                    `json:"has_embed_api_key"`
	Dimensions             int64                   `json:"dimensions"`
	LLMProvider            string                  `json:"llm_provider"`
	LLMModel               *string                 `json:"llm_model"`
	LLMBaseURL             *string                 `json:"llm_base_url"`
	HasLLMAPIKey           bool                    `json:"has_llm_api_key"`
	ExtractionDelaySeconds int64                   `json:"extraction_delay_seconds"`
	SummarizeAfter         int64                   `json:"summarize_after"`
	KeepRecent             int64                   `json:"keep_recent"`
	SessionTTLSeconds      int64                   `json:"session_ttl_seconds"`
	MemoryTTLSeconds       int64                   `json:"memory_ttl_seconds"`
	DedupeThreshold        float64                 `json:"dedupe_threshold"`
	CustomTypes            []AgentMemoryCustomType `json:"custom_types"`
	MemoryTypes            []string                `json:"memory_types"`
	RedactPII              bool                    `json:"redact_pii"`
	CreatedAt              string                  `json:"created_at"`
	UpdatedAt              string                  `json:"updated_at"`
}

// AgentMemoryRequest is the PUT body (agentMemoryStoreBodySchema).
type AgentMemoryRequest struct {
	EmbedProvider          string                  `json:"embed_provider"`
	EmbedModel             *string                 `json:"embed_model,omitempty"`
	EmbedBaseURL           *string                 `json:"embed_base_url,omitempty"`
	EmbedAPIKey            *string                 `json:"embed_api_key,omitempty"`
	Dimensions             int64                   `json:"dimensions"`
	LLMProvider            string                  `json:"llm_provider"`
	LLMModel               *string                 `json:"llm_model,omitempty"`
	LLMBaseURL             *string                 `json:"llm_base_url,omitempty"`
	LLMAPIKey              *string                 `json:"llm_api_key,omitempty"`
	ExtractionDelaySeconds *int64                  `json:"extraction_delay_seconds,omitempty"`
	SummarizeAfter         *int64                  `json:"summarize_after,omitempty"`
	KeepRecent             *int64                  `json:"keep_recent,omitempty"`
	SessionTTLSeconds      *int64                  `json:"session_ttl_seconds,omitempty"`
	MemoryTTLSeconds       *int64                  `json:"memory_ttl_seconds,omitempty"`
	DedupeThreshold        *float64                `json:"dedupe_threshold,omitempty"`
	CustomTypes            []AgentMemoryCustomType `json:"custom_types"`
	RedactPII              *bool                   `json:"redact_pii,omitempty"`
}

func agentMemoryPath(instanceID, name string) string {
	return "/instances/" + pathEscape(instanceID) + "/agent-memory/" + pathEscape(name)
}

func (c *Client) PutAgentMemoryStore(ctx context.Context, instanceID, name string, req AgentMemoryRequest) (*AgentMemoryStore, error) {
	if req.CustomTypes == nil {
		req.CustomTypes = []AgentMemoryCustomType{}
	}
	var out struct {
		Store *AgentMemoryStore `json:"store"`
	}
	if err := c.doAI(ctx, http.MethodPut, agentMemoryPath(instanceID, name), req, &out); err != nil {
		return nil, err
	}
	if out.Store == nil {
		return nil, fmt.Errorf("the API saved the memory store but returned none")
	}
	return out.Store, nil
}

func (c *Client) GetAgentMemoryStore(ctx context.Context, instanceID, name string) (*AgentMemoryStore, error) {
	var out struct {
		Store *AgentMemoryStore `json:"store"`
	}
	if err := c.doAI(ctx, http.MethodGet, agentMemoryPath(instanceID, name), nil, &out); err != nil {
		return nil, err
	}
	if out.Store == nil {
		return nil, fmt.Errorf("the API returned no memory store for %s", name)
	}
	return out.Store, nil
}

// DeleteAgentMemoryStore deletes the store and every session and memory in it.
// The database must be running.
func (c *Client) DeleteAgentMemoryStore(ctx context.Context, instanceID, name string) error {
	return c.doAI(ctx, http.MethodDelete, agentMemoryPath(instanceID, name), nil, nil)
}

// ── Context retriever ───────────────────────────────────────────────────────

type ContextField struct {
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	Description *string `json:"description,omitempty"`
}

type ContextRelationship struct {
	Name        string  `json:"name"`
	Entity      string  `json:"entity"`
	Field       string  `json:"field"`
	Description *string `json:"description,omitempty"`
}

type ContextEntity struct {
	Name          string                `json:"name"`
	Description   string                `json:"description"`
	Prefix        string                `json:"prefix"`
	Storage       string                `json:"storage,omitempty"`
	IDField       string                `json:"id_field,omitempty"`
	Fields        []ContextField        `json:"fields"`
	Relationships []ContextRelationship `json:"relationships"`
}

type ContextModel struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Instance    struct {
		ID string `json:"id"`
	} `json:"instance"`
	Entities  []ContextEntity `json:"entities"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

// ContextTool is one tool the model gives an agent; only its name is kept.
type ContextTool struct {
	Name string `json:"name"`
}

// ContextAgentKey is an agent key as listed; the key itself is shown only by
// the create.
type ContextAgentKey struct {
	ID         string                    `json:"id"`
	Name       string                    `json:"name"`
	KeyPrefix  string                    `json:"key_prefix"`
	Entities   []string                  `json:"entities"`
	Filters    map[string]map[string]any `json:"filters"`
	CreatedAt  string                    `json:"created_at"`
	LastUsedAt *string                   `json:"last_used_at"`
	Revoked    bool                      `json:"revoked"`
}

// ContextModelDetail is GET /context-models/:name: the model, its tools and
// its agent keys that are not revoked.
type ContextModelDetail struct {
	Model ContextModel      `json:"model"`
	Tools []ContextTool     `json:"tools"`
	Keys  []ContextAgentKey `json:"keys"`
}

// ContextModelRequest is the PUT body (contextModelBodySchema).
type ContextModelRequest struct {
	InstanceID  string          `json:"instance_id"`
	Description string          `json:"description"`
	Entities    []ContextEntity `json:"entities"`
}

func contextModelPath(name string) string { return "/context-models/" + pathEscape(name) }

// PutContextModel creates or changes a model and rebuilds its search indexes
// in the database, which must be running.
func (c *Client) PutContextModel(ctx context.Context, name string, req ContextModelRequest) (*ContextModelDetail, error) {
	var out ContextModelDetail
	if err := c.doAI(ctx, http.MethodPut, contextModelPath(name), req, &out); err != nil {
		return nil, err
	}
	if out.Model.Name == "" {
		return nil, fmt.Errorf("the API saved the context model but returned none")
	}
	return &out, nil
}

func (c *Client) GetContextModel(ctx context.Context, name string) (*ContextModelDetail, error) {
	var out ContextModelDetail
	if err := c.doAI(ctx, http.MethodGet, contextModelPath(name), nil, &out); err != nil {
		return nil, err
	}
	if out.Model.Name == "" {
		return nil, fmt.Errorf("the API returned no context model for %s", name)
	}
	return &out, nil
}

// DeleteContextModel deletes the model, its search indexes and its agent keys.
// The records the indexes covered stay in the database.
func (c *Client) DeleteContextModel(ctx context.Context, name string) error {
	return c.doAI(ctx, http.MethodDelete, contextModelPath(name), nil, nil)
}

// ContextAgentKeyRequest is the POST body (contextAgentKeyBodySchema). Filter
// values are sent as strings: the API compares a numeric field by Number(value),
// so "5" and 5 filter alike.
type ContextAgentKeyRequest struct {
	Name     string                       `json:"name"`
	Entities []string                     `json:"entities,omitempty"`
	Filters  map[string]map[string]string `json:"filters"`
}

func (c *Client) CreateContextAgentKey(ctx context.Context, model string, req ContextAgentKeyRequest) (string, *ContextAgentKey, error) {
	if req.Filters == nil {
		req.Filters = map[string]map[string]string{}
	}
	var out struct {
		Key      string           `json:"key"`
		AgentKey *ContextAgentKey `json:"agent_key"`
	}
	if err := c.doAI(ctx, http.MethodPost, contextModelPath(model)+"/keys", req, &out); err != nil {
		return "", nil, err
	}
	if out.AgentKey == nil || out.AgentKey.ID == "" || out.Key == "" {
		return "", nil, fmt.Errorf("the API made the agent key but did not return it")
	}
	return out.Key, out.AgentKey, nil
}

// RevokeContextAgentKey revokes a key. A key already revoked answers 404
// key_not_found, as does one that never existed.
func (c *Client) RevokeContextAgentKey(ctx context.Context, model, keyID string) error {
	return c.doAI(ctx, http.MethodDelete, contextModelPath(model)+"/keys/"+pathEscape(keyID), nil, nil)
}

// ContextMCPURL is where an agent holding an agent key speaks the Model Context
// Protocol. There is nothing to look up: the key names its model.
func (c *Client) ContextMCPURL() string { return c.baseURL + "/context/mcp" }

// filterString spells a filter value the way the provider stores it. The API
// keeps whatever it was sent, so a key made in the console may hold numbers.
func filterString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
