package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/oauth2"

	"github.com/gordonwei/victoria-gateway/pkg/gcpauth"
)

// CloudAssistClient escalates to a Gemini Cloud Assist investigation
// (REST API v1alpha, geminicloudassist.googleapis.com) — the GCP
// counterpart of DevOpsAgentClient. It is not another chat completion:
// the investigation reads the project's own Cloud Logging, Cloud
// Monitoring, Cloud Asset Inventory config and change history, then
// comes back with ranked hypotheses about the root cause. That only
// helps when the alert is about something running in that GCP project.
//
// The flow is exactly the one gcloud's own
// `gcloud beta gemini cloud-assist investigations create` runs
// (googlecloudsdk/api_lib/gemini_cloud_assist/util.py, SDK 572.0.0):
//
//  1. POST v1alpha/projects/{project}/locations/global/investigations
//     with two user observations — "user.project" (the project ID,
//     OBSERVATION_TYPE_STRUCTURED_INPUT) and "user.input.log" (the issue
//     text, OBSERVATION_TYPE_CLOUD_LOG, with a time interval). The reply
//     is the Investigation, whose "revision" names the revision to run.
//     The API only accepts location "global".
//  2. POST v1alpha/{revision}:run, which returns a long-running Operation.
//  3. GET v1alpha/{operation} until done is true.
//  4. GET v1alpha/{investigation} and read its observations. Hypotheses
//     are observations of type OBSERVATION_TYPE_HYPOTHESIS from a
//     non-user observer — the same filter gcloud's ExtractObservations
//     applies.
//
// The issue text is the full escalation prompt (alert, RAG history, log
// excerpt), passed the same way DevOpsAgentClient passes it as the
// investigation description, and the result goes back as Markdown that
// pkg/aiops shows as-is (it isn't the JSON a chat model returns).
//
// Two constraints from the official docs that decide whether this works
// at all, checked 2026-09-27:
//   - "As of April 10, 2026, creating, running, and editing
//     investigations are only available to users that have a Premium
//     Support contract or who have requested access through their account
//     team" (docs.cloud.google.com/cloud-assist/create-investigation).
//   - The v1alpha discovery document (revision 20260919) marks
//     investigations.create and revisions.run "Deprecated: Investigations
//     should only be created/run by the agent". They still exist and are
//     what gcloud calls, but Google may remove them from this surface.
//
// Access also needs the Gemini Cloud Assist API
// (geminicloudassist.googleapis.com) enabled on the project and
// roles/geminicloudassist.investigationCreator (or investigationEditor/
// Admin) for the caller; the errors below say which one is missing.
type CloudAssistClient struct {
	project      string
	endpoint     string
	client       *http.Client
	pollInterval time.Duration
	pollTimeout  time.Duration
	// lookback is how far before "now" the issue's time interval starts.
	lookback time.Duration
	now      func() time.Time

	tokenSource oauth2.TokenSource // ADC in production; a fake in tests
	loadErr     error              // FindDefaultCredentials failure, reported on first use
}

type CloudAssistClientConfig struct {
	Project      string        // GCP project ID the investigation runs in and looks at
	PollInterval time.Duration // defaults to 15s
	PollTimeout  time.Duration // defaults to 10 minutes: one budget for create + run + waiting for the result
	Timeout      time.Duration // one HTTP call; defaults to 30s
	// Endpoint overrides "https://geminicloudassist.googleapis.com".
	// Leave empty for real use; it exists for tests.
	Endpoint string
}

const (
	cloudAssistDefaultEndpoint = "https://geminicloudassist.googleapis.com"
	cloudAssistBackend         = "gcp-cloud-assist"
	cloudAssistCodeInternal    = 13 // google.rpc.Code INTERNAL
	// cloudAssistMaxIssueBytes caps the issue text. The API doesn't
	// document a limit; this keeps the prompt's head (the alert itself)
	// and drops the tail of a very long log excerpt rather than risk a
	// 400 on an oversized field.
	cloudAssistMaxIssueBytes = 16000
)

func NewCloudAssistClient(cfg CloudAssistClientConfig) *CloudAssistClient {
	c := newCloudAssistClient(cfg)
	creds, err := gcpauth.FindDefaultCredentials(cloudPlatformScope)
	if err != nil {
		c.loadErr = err
	} else {
		c.tokenSource = creds.TokenSource
	}
	return c
}

func newCloudAssistClient(cfg CloudAssistClientConfig) *CloudAssistClient {
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	if endpoint == "" {
		endpoint = cloudAssistDefaultEndpoint
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = 15 * time.Second
	}
	pollTimeout := cfg.PollTimeout
	if pollTimeout <= 0 {
		pollTimeout = 10 * time.Minute
	}
	return &CloudAssistClient{
		project:      cfg.Project,
		endpoint:     endpoint,
		client:       &http.Client{Timeout: timeout},
		pollInterval: pollInterval,
		pollTimeout:  pollTimeout,
		lookback:     time.Hour,
		now:          time.Now,
	}
}

// Wire types — field names from the v1alpha discovery document.

type caInterval struct {
	StartTime string `json:"startTime,omitempty"`
	EndTime   string `json:"endTime,omitempty"`
}

type caObservation struct {
	ID                   string       `json:"id,omitempty"`
	Title                string       `json:"title,omitempty"`
	Text                 string       `json:"text,omitempty"`
	ObservationType      string       `json:"observationType,omitempty"`
	ObserverType         string       `json:"observerType,omitempty"`
	TimeIntervals        []caInterval `json:"timeIntervals,omitempty"`
	Recommendation       string       `json:"recommendation,omitempty"`
	SystemRelevanceScore float64      `json:"systemRelevanceScore,omitempty"`
}

type caStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type caInvestigation struct {
	Name           string                   `json:"name,omitempty"`
	Title          string                   `json:"title,omitempty"`
	Revision       string                   `json:"revision,omitempty"`
	ExecutionState string                   `json:"executionState,omitempty"`
	Error          *caStatus                `json:"error,omitempty"`
	Observations   map[string]caObservation `json:"observations,omitempty"`
}

type caOperation struct {
	Name  string    `json:"name"`
	Done  bool      `json:"done"`
	Error *caStatus `json:"error,omitempty"`
}

// Chat runs one investigation end to end and returns its hypotheses as
// Markdown. pollTimeout is one budget for the whole flow, measured from
// the start of Chat: time spent creating and starting the investigation
// comes out of the time left for polling, rather than the poll wait
// starting fresh after them. Only the final read of a completed
// investigation may run past it, by at most one HTTP timeout.
func (c *CloudAssistClient) Chat(messages []Message, _ *ChatOptions) (string, error) {
	issue := lastUserMessage(messages)
	if issue == "" {
		return "", fmt.Errorf("%s: no user message to investigate", cloudAssistBackend)
	}
	deadline := c.now().Add(c.pollTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), c.pollTimeout+c.client.Timeout)
	defer cancel()

	inv, err := c.create(ctx, issue)
	if err != nil {
		return "", err
	}
	if inv.Revision == "" {
		return "", fmt.Errorf("%s: created investigation %s has no revision to run", cloudAssistBackend, inv.Name)
	}
	var op caOperation
	if err := c.do(ctx, http.MethodPost, "/v1alpha/"+inv.Revision+":run", struct{}{}, &op); err != nil {
		return "", fmt.Errorf("%s: run investigation %s: %w", cloudAssistBackend, inv.Name, err)
	}
	if err := c.waitForOperation(ctx, op, inv.Name, deadline); err != nil {
		return "", err
	}
	var done caInvestigation
	if err := c.do(ctx, http.MethodGet, "/v1alpha/"+inv.Name, nil, &done); err != nil {
		return "", fmt.Errorf("%s: read investigation %s: %w", cloudAssistBackend, inv.Name, err)
	}
	if done.ExecutionState == "INVESTIGATION_EXECUTION_STATE_FAILED" || done.ExecutionState == "INVESTIGATION_EXECUTION_STATE_CANCELLED" {
		msg := ""
		if done.Error != nil {
			msg = ": " + done.Error.Message
		}
		return "", fmt.Errorf("%s: investigation %s ended %s%s", cloudAssistBackend, inv.Name, done.ExecutionState, msg)
	}
	return formatInvestigation(done)
}

// create sends step 1 of the flow in CloudAssistClient's doc comment.
func (c *CloudAssistClient) create(ctx context.Context, issue string) (caInvestigation, error) {
	body := caInvestigation{
		Title: investigationTitle(issue),
		Observations: map[string]caObservation{
			"user.project": {
				ID:              "user.project",
				Text:            c.project,
				ObservationType: "OBSERVATION_TYPE_STRUCTURED_INPUT",
				ObserverType:    "OBSERVER_TYPE_USER",
			},
			"user.input.log": {
				ID:              "user.input.log",
				Title:           "User Provided Issue",
				Text:            truncateUTF8(issue, cloudAssistMaxIssueBytes),
				ObservationType: "OBSERVATION_TYPE_CLOUD_LOG",
				ObserverType:    "OBSERVER_TYPE_USER",
				TimeIntervals:   []caInterval{{StartTime: c.now().Add(-c.lookback).UTC().Format(time.RFC3339)}},
			},
		},
	}
	var inv caInvestigation
	path := "/v1alpha/projects/" + c.project + "/locations/global/investigations"
	if err := c.do(ctx, http.MethodPost, path, body, &inv); err != nil {
		return caInvestigation{}, fmt.Errorf("%s: create investigation: %w", cloudAssistBackend, err)
	}
	return inv, nil
}

// waitForOperation polls the run operation every pollInterval until it
// is done or Chat's deadline has passed. The investigation itself keeps
// running on GCP after a timeout; its name is in the error so it can be
// opened in the console.
func (c *CloudAssistClient) waitForOperation(ctx context.Context, op caOperation, invName string, deadline time.Time) error {
	timedOut := func() error {
		return fmt.Errorf("%s: investigation %s did not complete within %s", cloudAssistBackend, invName, c.pollTimeout)
	}
	for {
		if op.Done {
			if op.Error != nil {
				err := fmt.Errorf("%s: investigation %s failed: %s (code %d)", cloudAssistBackend, invName, op.Error.Message, op.Error.Code)
				if op.Error.Code == cloudAssistCodeInternal {
					// Seen 2026-09-27 on a test project without Premium
					// Support: create succeeded, the run failed with
					// INTERNAL within seconds, every time.
					err = fmt.Errorf("%w — if every run fails this way, check that the project has investigation access (since 2026-04-10: Premium Support or access through the Google Cloud account team)", err)
				}
				return err
			}
			return nil
		}
		if op.Name == "" {
			return fmt.Errorf("%s: run of %s returned no operation name", cloudAssistBackend, invName)
		}
		left := deadline.Sub(c.now())
		if left <= 0 {
			return timedOut()
		}
		// Don't sleep past the deadline: the last poll happens at it.
		wait := min(c.pollInterval, left)
		select {
		case <-ctx.Done():
			return timedOut()
		case <-time.After(wait):
		}
		name := op.Name
		op = caOperation{}
		if err := c.do(ctx, http.MethodGet, "/v1alpha/"+name, nil, &op); err != nil {
			if ctx.Err() != nil || !c.now().Before(deadline) {
				// The poll itself ran out the clock; that is the same
				// timeout, not a failure of the operation.
				return timedOut()
			}
			return fmt.Errorf("%s: poll %s: %w", cloudAssistBackend, name, err)
		}
		if op.Name == "" {
			op.Name = name
		}
	}
}

// do sends one authorized JSON request. A non-2xx reply comes back as
// *HTTPStatusError (so a 5xx/429 reads as "unavailable" to callers that
// check), wrapped with a hint when the status says what to fix.
func (c *CloudAssistClient) do(ctx context.Context, method, path string, in, out any) error {
	if c.loadErr != nil {
		return fmt.Errorf("load Application Default Credentials: %w", c.loadErr)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok, err := c.tokenSource.Token()
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}
	tok.SetAuthHeader(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
		statusErr := &HTTPStatusError{Backend: cloudAssistBackend, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(b))}
		if hint := cloudAssistHint(resp.StatusCode, statusErr.Body, c.project); hint != "" {
			return fmt.Errorf("%w — %s", statusErr, hint)
		}
		return statusErr
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// cloudAssistHint turns the two setup failures an operator is most
// likely to hit into what to do about them.
func cloudAssistHint(status int, body, project string) string {
	switch {
	case status == http.StatusForbidden && (strings.Contains(body, "SERVICE_DISABLED") || strings.Contains(body, "has not been used in project")):
		return fmt.Sprintf("enable the Gemini Cloud Assist API (geminicloudassist.googleapis.com) on project %s", project)
	case status == http.StatusForbidden:
		return "the caller needs roles/geminicloudassist.investigationCreator (or investigationEditor/Admin) on the project; since 2026-04-10 creating and running investigations also requires a Premium Support contract or access granted through the Google Cloud account team"
	case status == http.StatusNotFound:
		return "check the project ID; the investigations API only serves location \"global\""
	}
	return ""
}

// formatInvestigation renders a finished investigation as Markdown: an
// investigation summary observation first if the service produced one,
// then the hypotheses by descending relevance. With neither, the other
// non-user observations' titles are listed so the result isn't empty.
func formatInvestigation(inv caInvestigation) (string, error) {
	var summaries, hypotheses, others []caObservation
	for _, o := range inv.Observations {
		if o.ObserverType == "OBSERVER_TYPE_USER" || strings.TrimSpace(o.Text) == "" {
			continue
		}
		switch o.ObservationType {
		case "OBSERVATION_TYPE_STRUCTURED_INPUT", "OBSERVATION_TYPE_RELATED_RESOURCES", "OBSERVATION_TYPE_KNOWLEDGE", "OBSERVATION_TYPE_PROGRESS_BAR_DYNAMIC_STATUS":
			// Not findings — gcloud hides the first three too.
		case "OBSERVATION_TYPE_INVESTIGATION_SUMMARY":
			summaries = append(summaries, o)
		case "OBSERVATION_TYPE_HYPOTHESIS":
			hypotheses = append(hypotheses, o)
		default:
			others = append(others, o)
		}
	}
	byRelevance := func(obs []caObservation) {
		sort.SliceStable(obs, func(i, j int) bool {
			if obs[i].SystemRelevanceScore != obs[j].SystemRelevanceScore {
				return obs[i].SystemRelevanceScore > obs[j].SystemRelevanceScore
			}
			return obs[i].ID < obs[j].ID
		})
	}
	byRelevance(summaries)
	byRelevance(hypotheses)
	byRelevance(others)

	var b strings.Builder
	fmt.Fprintf(&b, "Gemini Cloud Assist investigation %s\n", inv.Name)
	for _, s := range summaries {
		fmt.Fprintf(&b, "\n## %s\n%s\n", titleOr(s.Title, "Summary"), strings.TrimSpace(s.Text))
	}
	for i, h := range hypotheses {
		fmt.Fprintf(&b, "\n## Hypothesis %d: %s\n%s\n", i+1, titleOr(h.Title, "untitled"), strings.TrimSpace(h.Text))
		if r := strings.TrimSpace(h.Recommendation); r != "" {
			fmt.Fprintf(&b, "\nRecommendation: %s\n", r)
		}
	}
	if len(summaries) == 0 && len(hypotheses) == 0 {
		if len(others) == 0 {
			return "", fmt.Errorf("%s: investigation %s finished with no hypotheses or observations", cloudAssistBackend, inv.Name)
		}
		b.WriteString("\nNo hypothesis was produced. Observations:\n")
		for i, o := range others {
			if i == 10 {
				fmt.Fprintf(&b, "- … and %d more\n", len(others)-i)
				break
			}
			fmt.Fprintf(&b, "- %s\n", titleOr(o.Title, firstLine(o.Text)))
		}
	}
	return b.String(), nil
}

func titleOr(title, fallback string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	return fallback
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncateUTF8(s, 200)
}

// investigationTitle is the prompt's first line (the alert name, in
// pkg/aiops's prompt) prefixed so investigations started by the gateway
// are recognizable in the console.
func investigationTitle(issue string) string {
	return "victoria-gateway: " + truncateUTF8(firstLine(issue), 100)
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Available lists at most one investigation: read-only, free, and a 200
// confirms the API is enabled and the credentials can see the project.
// It says nothing about the create/run access restriction.
func (c *CloudAssistClient) Available() bool {
	ctx, cancel := context.WithTimeout(context.Background(), c.client.Timeout)
	defer cancel()
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, "/v1alpha/projects/"+c.project+"/locations/global/investigations?pageSize=1", nil, &out)
	return err == nil
}

func (c *CloudAssistClient) ModelName() string {
	return cloudAssistBackend
}

func (c *CloudAssistClient) Backend() string {
	return cloudAssistBackend
}
