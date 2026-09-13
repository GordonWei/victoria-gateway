package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// BedrockClient speaks Amazon Bedrock's InvokeModel API for Anthropic
// Claude models — a general-purpose cloud escalation target for AWS,
// separate from (and not a replacement for) pkg/model.DevOpsAgentClient.
// DevOpsAgentClient exists for one specific scenario — investigating
// AWS-account infrastructure via AWS DevOps Agent's own AWS-side access —
// and was added for a 2026-10-31 conference talk about that exact
// capability. BedrockClient is the "just give me a strong model, hosted
// on AWS" option every other provider in this file offers for its own
// cloud, with none of DevOpsAgentClient's AWS-account-awareness or its
// Python/MCP subprocess dependency.
//
// Unlike every other client here (plain net/http against a JSON REST
// API), this one uses the official AWS SDK for Go v2 rather than hand-
// rolling the request. That's a deliberate exception to this package's
// usual minimal-dependency style: Bedrock's InvokeModel endpoint requires
// AWS SigV4 request signing, not a bearer token or API key, and getting
// SigV4 signing correct (and covering credential sources correctly — env
// vars, ~/.aws/credentials, an EC2/ECS/EKS instance role, SSO, ...) by
// hand is exactly the kind of subtly-wrong-in-an-edge-case code a
// hand-rolled implementation would be a poor place to reinvent. The SDK's
// default credential chain (via config.LoadDefaultConfig) is also simply
// what an AWS user expects to work out of the box — there's deliberately
// no access-key/secret config field here; that would both invite
// operators to put long-lived credentials in config.yaml and duplicate
// what the SDK already does better.
type BedrockClient struct {
	client  *bedrockruntime.Client
	model   string
	timeout time.Duration
	// loadErr carries a failure from config.LoadDefaultConfig, if any,
	// through to Chat/Available instead of the constructor returning an
	// error — this keeps BedrockClient's construction symmetric with
	// every other New*Client in this package (none of them can fail at
	// construction time either; a bad endpoint or key only surfaces once
	// something actually tries to use it).
	loadErr error
}

type BedrockClientConfig struct {
	// Region is the AWS region Bedrock is called in, e.g. "us-east-1".
	// Bedrock model availability varies by region — see AWS's "Model
	// support by AWS Region" table before picking one.
	Region string

	// Model is the Bedrock model ID or inference profile ID, e.g.
	// "us.anthropic.claude-haiku-4-5-20251001-v1:0". Not defaulted here
	// deliberately: Bedrock model IDs change as new models ship, and a
	// hard-coded default would go stale.
	//
	// Confirmed against a real account (2026-09-13, us-east-1): several
	// current-generation models reject the bare model ID for on-demand
	// invocation outright —
	//   ValidationException: Invocation of model ID
	//   anthropic.claude-haiku-4-5-20251001-v1:0 with on-demand
	//   throughput isn't supported. Retry your request with the ID or
	//   ARN of an inference profile that contains this model.
	// — and need the region-prefixed cross-region inference profile ID
	// instead (the "us." prefix above, not the bare "anthropic." one;
	// list available profiles with `aws bedrock list-inference-profiles`
	// or the equivalent SDK call). Older models like Claude 3 Haiku still
	// accept the bare ID directly. Which form a given model needs isn't
	// predictable from the ID alone — if InvokeModel returns this exact
	// error, that's the fix.
	Model string

	// Timeout bounds one InvokeModel call; 0 keeps the client's 60s
	// default, matching every other cloud client in this package.
	Timeout time.Duration

	// Endpoint overrides the SDK's default regional Bedrock Runtime
	// endpoint. Leave empty for real AWS use — this exists for pointing
	// at a local mock server in tests, or (in principle) a
	// Bedrock-API-compatible gateway.
	Endpoint string

	// credentialsProvider lets tests inject static fake credentials so
	// they don't touch the real environment/filesystem/IMDS credential
	// chain. Unexported: production callers always get the SDK's default
	// chain, which is the entire point of using the SDK here.
	credentialsProvider aws.CredentialsProvider
}

// bedrockAnthropicRequest is Bedrock's wire format for Anthropic Claude
// models — Anthropic's own Messages API shape, but with anthropic_version
// in place of a model field (the model is already selected by
// InvokeModel's ModelId parameter, not the request body) — see
// https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-anthropic-claude-messages.html.
// This is why it reuses Message and splitSystemMessage from the
// AnthropicClient code above rather than duplicating that shape from
// scratch.
type bedrockAnthropicRequest struct {
	AnthropicVersion string    `json:"anthropic_version"`
	System           string    `json:"system,omitempty"`
	Messages         []Message `json:"messages"`
	MaxTokens        int       `json:"max_tokens"`
	Temperature      float64   `json:"temperature"`
}

// bedrockAnthropicVersion is the fixed value Bedrock's Anthropic Claude
// models require in the request body — not related to which Claude model
// version is being invoked (that's ModelId), and not expected to change;
// it identifies the wire schema version, and AWS's own examples have used
// this same literal value since the Messages API launched on Bedrock.
const bedrockAnthropicVersion = "bedrock-2023-05-31"

func NewBedrockClient(cfg BedrockClientConfig) *BedrockClient {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	loadOpts := []func(*config.LoadOptions) error{config.WithRegion(cfg.Region)}
	if cfg.credentialsProvider != nil {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(cfg.credentialsProvider))
	}
	awsCfg, err := config.LoadDefaultConfig(context.Background(), loadOpts...)

	var client *bedrockruntime.Client
	if err == nil {
		client = bedrockruntime.NewFromConfig(awsCfg, func(o *bedrockruntime.Options) {
			if cfg.Endpoint != "" {
				o.BaseEndpoint = aws.String(cfg.Endpoint)
			}
		})
	}

	return &BedrockClient{
		client:  client,
		model:   cfg.Model,
		timeout: timeout,
		loadErr: err,
	}
}

// Chat maps the shared Message/ChatOptions shape onto Bedrock's Anthropic
// Claude InvokeModel request, the same way AnthropicClient.Chat does for
// the native Anthropic API — the system-message extraction and response
// shape (content blocks with type "text") are identical, since Bedrock's
// Claude wire format is Anthropic's own Messages API with the model name
// moved out of the body. See anthropicResponse in model.go, reused here
// rather than duplicated.
func (c *BedrockClient) Chat(messages []Message, opts *ChatOptions) (string, error) {
	if c.loadErr != nil {
		return "", fmt.Errorf("load AWS config: %w", c.loadErr)
	}

	maxTokens := 1024
	temperature := 0.1
	if opts != nil {
		if opts.MaxTokens > 0 {
			maxTokens = opts.MaxTokens
		}
		if opts.Temperature > 0 {
			temperature = opts.Temperature
		}
	}

	system, chatMessages := splitSystemMessage(messages)

	reqBody := bedrockAnthropicRequest{
		AnthropicVersion: bedrockAnthropicVersion,
		System:           system,
		Messages:         chatMessages,
		MaxTokens:        maxTokens,
		Temperature:      temperature,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	out, err := c.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(c.model),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return "", fmt.Errorf("bedrock InvokeModel failed: %w", err)
	}

	var result anthropicResponse
	if err := json.Unmarshal(out.Body, &result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	for _, block := range result.Content {
		if block.Type == "text" && block.Text != "" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("bedrock returned no text content block")
}

// Available does a lightweight reachability/credential check by sending a
// minimal (1-token) real InvokeModel request, the same approach
// AnthropicClient.Available uses and for the same reason: Bedrock has no
// unauthenticated health endpoint, and a request that reaches the service
// at all — even one that comes back as an auth or permission error —
// still confirms the endpoint and region are correct; a misconfigured
// model ID or IAM policy is a separate, later failure mode from "can't
// reach Bedrock in this region."
func (c *BedrockClient) Available() bool {
	if c.loadErr != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	body, err := json.Marshal(bedrockAnthropicRequest{
		AnthropicVersion: bedrockAnthropicVersion,
		Messages:         []Message{{Role: "user", Content: "ping"}},
		MaxTokens:        1,
	})
	if err != nil {
		return false
	}

	_, err = c.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(c.model),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err == nil {
		return true
	}
	// A network-level failure (wrong endpoint, DNS, connection refused)
	// never gets an HTTP response at all. An auth/permission/validation
	// error from Bedrock itself did get one — smithy-go wraps any
	// non-2xx response in *smithyhttp.ResponseError, which is what
	// httpStatusCoder below matches — and that's still a successful round
	// trip to the service, so it counts as "available" per the doc
	// comment above: a bad model ID or IAM policy is a separate,
	// diagnosable-later failure mode from "can't reach Bedrock at all."
	//
	// The status code itself has to be checked, not just whether the
	// error implements httpStatusCoder: a connection failure that never
	// reached the server still gets wrapped in a *smithy.OperationError
	// whose chain includes a synthetic response-error type reporting
	// StatusCode() == 0 (confirmed by inspecting the error chain for a
	// connection-refused case directly — "https response error
	// StatusCode: 0" appears in its message) — errors.As matches the
	// *type*, so checking only that would misclassify "never reached the
	// server" as available.
	var httpErr httpStatusCoder
	return errors.As(err, &httpErr) && httpErr.HTTPStatusCode() != 0
}

// httpStatusCoder matches smithy-go's *transport/http.ResponseError
// without importing that package just for a type assertion — any error
// that can report an HTTP status code arrived as an actual response from
// the far end, successful or not.
type httpStatusCoder interface {
	HTTPStatusCode() int
}

func (c *BedrockClient) ModelName() string {
	return c.model
}

func (c *BedrockClient) Backend() string {
	return "bedrock"
}
