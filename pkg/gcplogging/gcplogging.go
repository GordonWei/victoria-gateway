// Package gcplogging implements pkg/aiops.LogSource against GCP Cloud
// Logging — an alternative to Loki for deployments whose logs already
// live there (GKE via the built-in Cloud Logging integration, Cloud Run,
// Compute Engine via the Ops Agent, Cloud Functions). Selected by
// config.LogSourceConfig.Type: "gcp_logging"; see pkg/cloudwatch for the
// AWS CloudWatch Logs equivalent.
//
// Unlike CloudWatch Logs Insights (async StartQuery/GetQueryResults),
// Cloud Logging's entries.list is a single synchronous call — no polling
// needed, which is why this client is plain net/http against the REST
// API (matching this codebase's usual style, see pkg/model's Gemini/
// Anthropic/OpenAI clients) rather than pulling in the much heavier
// cloud.google.com/go/logging SDK for what one HTTP call needs.
// Authentication still needs real Application Default Credentials
// (Cloud Logging is a project-scoped IAM-gated API, not a bare-API-key
// one) — golang.org/x/oauth2/google's FindDefaultCredentials is already
// available transitively (see go.mod) and is what BedrockClient's own
// doc comment calls out as the right trade-off for exactly this reason:
// getting credential-source coverage (env var, gcloud ADC file, GCE/GKE
// metadata server, workload identity) right by hand is a poor place to
// reinvent a wheel the standard library already turns.
package gcplogging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
)

// loggingScope is the OAuth2 scope entries.list requires read access for.
const loggingScope = "https://www.googleapis.com/auth/logging.read"

// termPlaceholder is substituted with LogIdentity.Term() in FilterTemplate
// — a plain string-replace token rather than a Sprintf verb, for the same
// reason pkg/cloudwatch uses one: a user's own filter clause routinely
// contains literal characters a format verb would choke on.
const termPlaceholder = "{{TERM}}"

// defaultFilterTemplate uses Cloud Logging Query Language's SEARCH()
// function — a full-text-ish search across textPayload, jsonPayload, and
// other structured fields, the closest single-clause equivalent to
// CloudWatch's @message substring filter or Loki's line filter. Like
// cloudwatch's defaultQueryTemplate, this is a reasonable starting point,
// not a claim that it's the right query for every log shape — structured
// JSON logs with a known field (e.g. `jsonPayload.host="{{TERM}}"`) will
// usually do better than a generic search.
const defaultFilterTemplate = `SEARCH("` + termPlaceholder + `")`

// Client queries GCP Cloud Logging's entries.list REST API.
type Client struct {
	projectID      string
	filterTemplate string
	endpoint       string
	httpClient     *http.Client
	timeout        time.Duration
	// tokenSource lets tests inject a fake token source so they never
	// touch the real ADC chain (env var, ~/.config/gcloud, GCE/GKE
	// metadata server). Unexported: production callers always get
	// google.FindDefaultCredentials's TokenSource.
	tokenSource oauth2.TokenSource
	// loadErr carries a FindDefaultCredentials failure through to
	// QueryRange instead of NewClient returning an error — the same
	// construction-never-fails shape BedrockClient and cloudwatch.Client
	// use, for the same reason (symmetric with every other New*Client
	// that also can't fail before something actually tries to use it).
	loadErr error
}

// ClientConfig configures a gcplogging.Client.
type ClientConfig struct {
	// ProjectID is the GCP project whose logs are searched — becomes
	// entries.list's resourceNames: ["projects/<ProjectID>"].
	ProjectID string

	// FilterTemplate overrides defaultFilterTemplate. Must contain the
	// literal token "{{TERM}}" exactly once — replaced with
	// LogIdentity.Term(). Leave empty to use defaultFilterTemplate.
	FilterTemplate string

	// Timeout bounds one QueryRange call. 0 defaults to 30s.
	Timeout time.Duration

	// Endpoint overrides the default "https://logging.googleapis.com".
	// Leave empty for real use — exists for pointing at a local mock
	// server in tests.
	Endpoint string
}

// NewClient builds a gcplogging.Client, resolving Application Default
// Credentials immediately (unlike NewBedrockClient/cloudwatch.NewClient,
// which defer their AWS config load's error to first use — ADC
// resolution here is itself cheap and synchronous, so there's no reason
// to hide a bad credential setup until the first alert needs logs).
func NewClient(cfg ClientConfig) *Client {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	filterTemplate := cfg.FilterTemplate
	if filterTemplate == "" {
		filterTemplate = defaultFilterTemplate
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://logging.googleapis.com"
	}

	creds, err := google.FindDefaultCredentials(context.Background(), loggingScope)

	c := &Client{
		projectID:      cfg.ProjectID,
		filterTemplate: filterTemplate,
		endpoint:       endpoint,
		httpClient:     &http.Client{Timeout: timeout},
		timeout:        timeout,
		loadErr:        err,
	}
	if err == nil {
		c.tokenSource = creds.TokenSource
	}
	return c
}

// entriesListRequest is Cloud Logging's entries.list request body — see
// https://cloud.google.com/logging/docs/reference/v2/rest/v2/entries/list.
type entriesListRequest struct {
	ResourceNames []string `json:"resourceNames"`
	Filter        string   `json:"filter"`
	OrderBy       string   `json:"orderBy"`
	PageSize      int32    `json:"pageSize,omitempty"`
}

type entriesListResponse struct {
	Entries []logEntry `json:"entries"`
}

type logEntry struct {
	Timestamp   string `json:"timestamp"`
	TextPayload string `json:"textPayload"`
	// JSONPayload is decoded as raw JSON text rather than a fixed struct
	// — its shape is whatever the logging source chose to emit, and this
	// client only needs *some* readable line, not a specific field.
	JSONPayload json.RawMessage `json:"jsonPayload,omitempty"`
}

// line returns the best available text representation of this entry: the
// plain-text payload if the source logged one, otherwise the raw JSON
// payload as-is (still readable, just not reformatted) — mirroring the
// same "give the LLM something over nothing" spirit as
// aiops.parseLokiResponse skipping only truly-empty rows, never trying to
// guess a nicer rendering of structured logs it doesn't understand.
func (e logEntry) line() string {
	if e.TextPayload != "" {
		return e.TextPayload
	}
	// json.RawMessage captures a literal "null" (4 bytes) when the wire
	// JSON explicitly sends jsonPayload:null, which some servers do even
	// for a field that's genuinely absent — that's not usable log
	// content any more than an actually-missing key would be.
	if len(e.JSONPayload) > 0 && string(e.JSONPayload) != "null" {
		return string(e.JSONPayload)
	}
	return ""
}

// cloudLoggingTimestampLayout is RFC3339 with nanosecond precision, the
// format Cloud Logging's REST API documents entries.timestamp in (e.g.
// "2026-09-19T18:30:00.123456789Z").
const cloudLoggingTimestampLayout = time.RFC3339Nano

// QueryRange implements aiops.LogSource.
func (c *Client) QueryRange(ctx context.Context, id aiops.LogIdentity, start, end time.Time, limit int) ([]aiops.LogEntry, error) {
	if c.loadErr != nil {
		return nil, fmt.Errorf("load GCP application default credentials: %w", c.loadErr)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	filterBody := strings.ReplaceAll(c.filterTemplate, termPlaceholder, id.Term())
	filter := fmt.Sprintf(`timestamp >= %q AND timestamp <= %q AND %s`,
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), filterBody)

	reqBody := entriesListRequest{
		ResourceNames: []string{"projects/" + c.projectID},
		Filter:        filter,
		OrderBy:       "timestamp desc",
	}
	if limit > 0 {
		reqBody.PageSize = int32(limit)
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	token, err := c.tokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("get GCP access token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v2/entries:list", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to Cloud Logging failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloud logging returned %d: %s", resp.StatusCode, truncate(respBody, 200))
	}

	var result entriesListResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	entries := make([]aiops.LogEntry, 0, len(result.Entries))
	for _, e := range result.Entries {
		line := e.line()
		if line == "" {
			continue
		}
		ts, _ := time.Parse(cloudLoggingTimestampLayout, e.Timestamp) // zero Time on parse failure; still a usable entry
		entries = append(entries, aiops.LogEntry{Timestamp: ts, Line: line})
	}
	return entries, nil
}

func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}
