// Package cloudwatch implements pkg/aiops.LogSource against AWS CloudWatch
// Logs Insights — an alternative to Loki for deployments whose logs
// already live in CloudWatch (Lambda, ECS, EKS via the CloudWatch agent,
// EC2 via the unified agent) rather than a self-hosted Loki instance.
// Selected by config.LogSourceConfig.Type: "cloudwatch"; see
// pkg/gcplogging for the GCP Cloud Logging equivalent.
package cloudwatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
)

// termPlaceholder is substituted with LogIdentity.Term() in QueryTemplate.
// A plain string-replace token rather than fmt.Sprintf's %s: a
// user-supplied template is far more likely to contain a literal '%'
// (log lines routinely do) than this token, and Sprintf would then choke
// on — or silently misinterpret — every %-sign in the operator's own
// query, not just the one placeholder they meant to fill in.
const termPlaceholder = "{{TERM}}"

// defaultQueryTemplate is deliberately a full-text @message search, not a
// structured field match: CloudWatch Logs Insights has no equivalent of
// Loki's stream labels — whatever identifies "this alert's logs" (a pod
// name, a host) has to already appear somewhere in the log line itself
// for this to find anything. Confirmed against a real log group
// (2026-09-19): @timestamp comes back as "2024-03-26 06:17:25.758" (space
// separated, millisecond precision, UTC) — see parseTimestamp.
const defaultQueryTemplate = `fields @timestamp, @message | filter @message like /` + termPlaceholder + `/ | sort @timestamp desc`

// Client queries CloudWatch Logs Insights the same way BedrockClient
// queries Bedrock: the AWS SDK for Go v2, SigV4 signing, and the SDK's
// default credential chain handled entirely by config.LoadDefaultConfig
// rather than accepting long-lived keys in config.yaml. See
// pkg/model.BedrockClient's doc comment for why that trade-off is made
// here too.
type Client struct {
	client        *cloudwatchlogs.Client
	logGroupNames []string
	queryTemplate string
	timeout       time.Duration
	pollInterval  time.Duration // overridden by tests; real use gets 1s
	// loadErr mirrors BedrockClient's loadErr field — see its doc comment
	// for why construction never fails and the error surfaces on first use
	// instead.
	loadErr error
}

// ClientConfig configures a cloudwatch.Client.
type ClientConfig struct {
	// Region is the AWS region CloudWatch Logs is queried in, e.g.
	// "us-east-1". Log groups are region-scoped; this must match wherever
	// the target log groups actually live.
	Region string

	// LogGroupNames is which log group(s) to search — Logs Insights
	// supports querying several at once (StartQueryInput.LogGroupNames),
	// useful when the same workload logs to more than one group (e.g. one
	// per Lambda function behind an API Gateway).
	LogGroupNames []string

	// QueryTemplate overrides defaultQueryTemplate. Must contain the
	// literal token "{{TERM}}" exactly once — it's replaced with
	// LogIdentity.Term() (the most specific single identifying string
	// available for the alert: pod, then deployment/statefulset, then
	// host). Leave empty to use defaultQueryTemplate, which does a plain
	// @message substring search — fine as a starting point, but the right
	// query depends entirely on your own log shape (JSON structured logs
	// can filter on a specific field instead, e.g.
	// `fields @timestamp, @message | filter host = "{{TERM}}"`).
	QueryTemplate string

	// Timeout bounds one QueryRange call end-to-end, including the
	// StartQuery/GetQueryResults poll loop — Logs Insights queries are
	// asynchronous and can take several seconds over a large log group.
	// 0 defaults to 30s.
	Timeout time.Duration

	// Endpoint overrides the SDK's default regional CloudWatch Logs
	// endpoint. Leave empty for real AWS use — this exists for pointing
	// at a local mock server in tests.
	Endpoint string

	// credentialsProvider lets tests inject static fake credentials.
	// Unexported: production callers always get the SDK's default chain.
	credentialsProvider aws.CredentialsProvider
}

// NewClient builds a cloudwatch.Client. Like BedrockClient, construction
// never fails — a bad region or missing credentials only surfaces once
// QueryRange actually tries to use them.
func NewClient(cfg ClientConfig) *Client {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	queryTemplate := cfg.QueryTemplate
	if queryTemplate == "" {
		queryTemplate = defaultQueryTemplate
	}

	loadOpts := []func(*config.LoadOptions) error{config.WithRegion(cfg.Region)}
	if cfg.credentialsProvider != nil {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(cfg.credentialsProvider))
	}
	awsCfg, err := config.LoadDefaultConfig(context.Background(), loadOpts...)

	var client *cloudwatchlogs.Client
	if err == nil {
		client = cloudwatchlogs.NewFromConfig(awsCfg, func(o *cloudwatchlogs.Options) {
			if cfg.Endpoint != "" {
				o.BaseEndpoint = aws.String(cfg.Endpoint)
			}
		})
	}

	return &Client{
		client:        client,
		logGroupNames: cfg.LogGroupNames,
		queryTemplate: queryTemplate,
		timeout:       timeout,
		pollInterval:  time.Second,
		loadErr:       err,
	}
}

// QueryRange implements aiops.LogSource. It starts a Logs Insights query
// built from id.Term() and polls until it completes or c.timeout elapses.
func (c *Client) QueryRange(ctx context.Context, id aiops.LogIdentity, start, end time.Time, limit int) ([]aiops.LogEntry, error) {
	if c.loadErr != nil {
		return nil, fmt.Errorf("load AWS config: %w", c.loadErr)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	queryString := strings.ReplaceAll(c.queryTemplate, termPlaceholder, id.Term())

	startInput := &cloudwatchlogs.StartQueryInput{
		LogGroupNames: c.logGroupNames,
		StartTime:     aws.Int64(start.Unix()),
		EndTime:       aws.Int64(end.Unix()),
		QueryString:   aws.String(queryString),
	}
	if limit > 0 {
		startInput.Limit = aws.Int32(int32(limit))
	}

	started, err := c.client.StartQuery(ctx, startInput)
	if err != nil {
		return nil, fmt.Errorf("cloudwatch StartQuery failed: %w", err)
	}

	return c.pollResults(ctx, *started.QueryId)
}

// pollResults implements the required StartQuery/GetQueryResults poll loop
// — Logs Insights has no synchronous "run and return results" call.
// Terminal non-Complete statuses (Failed/Cancelled/Timeout/Unknown) are
// reported as errors rather than "no logs found": those mean the query
// itself didn't run to completion, which is a materially different
// problem from "ran fine, matched nothing."
func (c *Client) pollResults(ctx context.Context, queryID string) ([]aiops.LogEntry, error) {
	for {
		result, err := c.client.GetQueryResults(ctx, &cloudwatchlogs.GetQueryResultsInput{
			QueryId: aws.String(queryID),
		})
		if err != nil {
			return nil, fmt.Errorf("cloudwatch GetQueryResults failed: %w", err)
		}

		switch result.Status {
		case types.QueryStatusComplete:
			return parseResults(result.Results), nil
		case types.QueryStatusScheduled, types.QueryStatusRunning:
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("cloudwatch query %s did not complete before timeout: %w", queryID, ctx.Err())
			case <-time.After(c.pollInterval):
			}
		default:
			return nil, fmt.Errorf("cloudwatch query %s ended with status %s", queryID, result.Status)
		}
	}
}

// cloudwatchTimestampLayout is the exact format Logs Insights' @timestamp
// field comes back in — verified against a real query result (2026-09-19):
// "2024-03-26 06:17:25.758". Not documented as a stable contract anywhere
// AWS publishes, but consistent across every result row observed; if this
// ever changes, parseResults skips the unparseable row rather than
// erroring the whole query (a partial answer beats none for something
// this best-effort).
const cloudwatchTimestampLayout = "2006-01-02 15:04:05.000"

func parseResults(rows [][]types.ResultField) []aiops.LogEntry {
	entries := make([]aiops.LogEntry, 0, len(rows))
	for _, row := range rows {
		var ts time.Time
		var line string
		for _, field := range row {
			if field.Field == nil || field.Value == nil {
				continue
			}
			switch *field.Field {
			case "@timestamp":
				if parsed, err := time.Parse(cloudwatchTimestampLayout, *field.Value); err == nil {
					ts = parsed
				}
			case "@message":
				line = *field.Value
			}
		}
		if line == "" {
			continue
		}
		entries = append(entries, aiops.LogEntry{Timestamp: ts, Line: line})
	}
	return entries
}
