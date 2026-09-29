package llm

// Claude and GPT through a company's own cloud account: Amazon Bedrock,
// Google Vertex AI, Microsoft Foundry and Azure OpenAI. Each uses the
// cloud's standard credential chain (AWS profiles/SSO/roles, Google ADC,
// Entra ID via DefaultAzureCredential) or its API key, and reads the same
// environment variables as Claude Code and the vendors' SDKs, so a machine
// already set up for them needs nothing new.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Provider ids.
const (
	Bedrock     = "bedrock"
	Vertex      = "vertex"
	Foundry     = "foundry"
	AzureOpenAI = "azure-openai"
)

// Default models. Bedrock ids carry an anthropic. prefix; on Vertex, models
// before the 4.6 generation keep an @date suffix; Foundry takes deployment
// names, which default to the model id.
const (
	BedrockHaiku45  = "anthropic.claude-haiku-4-5"
	BedrockSonnet5  = "anthropic.claude-sonnet-5"
	VertexHaiku45   = "claude-haiku-4-5@20251001"
	VertexSonnet5   = "claude-sonnet-5"
	FoundryHaiku45  = "claude-haiku-4-5"
	FoundrySonnet5  = "claude-sonnet-5"
	anthropicVertex = "vertex-2023-10-16"
)

var (
	bedrockModels = []Model{
		{ID: "anthropic.claude-opus-5-5", Label: "Claude Opus 5.5"}, {ID: "anthropic.claude-opus-5", Label: "Claude Opus 5"},
		{ID: BedrockSonnet5, Label: "Claude Sonnet 5"}, {ID: BedrockHaiku45, Label: "Claude Haiku 4.5"},
		{ID: "anthropic.claude-fable-5-1", Label: "Claude Fable 5.1"},
	}
	vertexModels = []Model{
		{ID: "claude-opus-5-5", Label: "Claude Opus 5.5"}, {ID: "claude-opus-5", Label: "Claude Opus 5"},
		{ID: VertexSonnet5, Label: "Claude Sonnet 5"}, {ID: VertexHaiku45, Label: "Claude Haiku 4.5"},
		{ID: "claude-fable-5-1", Label: "Claude Fable 5.1"},
	}
	foundryModels = []Model{
		{ID: "claude-opus-5-5", Label: "Claude Opus 5.5"}, {ID: "claude-opus-5", Label: "Claude Opus 5"},
		{ID: FoundrySonnet5, Label: "Claude Sonnet 5"}, {ID: FoundryHaiku45, Label: "Claude Haiku 4.5"},
		{ID: "claude-fable-5-1", Label: "Claude Fable 5.1"},
	}
)

func env(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func truthy(k string) bool {
	switch strings.ToLower(env(k)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ConfiguredCloud is the cloud provider this machine is explicitly set up
// for, for -classifier auto: the CLAUDE_CODE_USE_* switches Claude Code
// reads, or the provider's own settings. Empty when none is.
func ConfiguredCloud() string {
	switch {
	case truthy("CLAUDE_CODE_USE_BEDROCK") || env("AWS_BEARER_TOKEN_BEDROCK") != "":
		return Bedrock
	case truthy("CLAUDE_CODE_USE_VERTEX") || env("ANTHROPIC_VERTEX_PROJECT_ID") != "":
		return Vertex
	case truthy("CLAUDE_CODE_USE_FOUNDRY") || env("ANTHROPIC_FOUNDRY_RESOURCE", "ANTHROPIC_FOUNDRY_BASE_URL") != "":
		return Foundry
	case env("AZURE_OPENAI_ENDPOINT") != "":
		return AzureOpenAI
	}
	return ""
}

// ---- Amazon Bedrock ----

// BedrockLLM sends Claude (anthropic.claude-* ids) to the Messages API on
// Bedrock (bedrock-mantle), and every other model id to the Converse API.
// That includes Claude as a cross-region inference profile
// (us.anthropic.claude-...), a versioned id (...-v1:0) or an ARN: those are
// Converse/InvokeModel ids, not Messages API model names.
type BedrockLLM struct {
	Model  string
	Region string // default: AWS_REGION, AWS_DEFAULT_REGION, the profile's region, us-east-1
}

var mantleModel = regexp.MustCompile(`^anthropic\.claude-[a-z0-9.-]+$`)

func (b *BedrockLLM) ModelID() string {
	if b.Model == "" {
		return BedrockHaiku45
	}
	return b.Model
}

func (b *BedrockLLM) Name() string { return Bedrock }

func (b *BedrockLLM) Call(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	if mantleModel.MatchString(b.ModelID()) {
		return (&AnthropicLLM{Model: b.ModelID(), Platform: &mantle{region: b.Region}}).Call(ctx, req)
	}
	return b.converse(ctx, req)
}

var (
	awsMu  sync.Mutex
	awsCfg *aws.Config
)

// awsConfig loads the default AWS config once it loads: env keys, profiles
// and SSO, web identity (EKS), container and instance roles. A failure is
// retried on the next call.
func awsConfig(ctx context.Context) (aws.Config, error) {
	awsMu.Lock()
	defer awsMu.Unlock()
	if awsCfg == nil {
		cfg, err := awsconfig.LoadDefaultConfig(context.WithoutCancel(ctx))
		if err != nil {
			return aws.Config{}, err
		}
		awsCfg = &cfg
	}
	return *awsCfg, nil
}

func awsRegion(ctx context.Context, region string) string {
	if region != "" {
		return region
	}
	if r := env("AWS_REGION", "AWS_DEFAULT_REGION"); r != "" {
		return r
	}
	if cfg, err := awsConfig(ctx); err == nil && cfg.Region != "" {
		return cfg.Region
	}
	return "us-east-1"
}

// mantle is Claude in Amazon Bedrock: the Messages API at
// bedrock-mantle.{region}.api.aws, with a Bedrock API key or SigV4.
type mantle struct{ region string }

func (m *mantle) ID() string           { return Bedrock }
func (m *mantle) DefaultModel() string { return BedrockHaiku45 }
func (m *mantle) Body(map[string]any)  {}
func (m *mantle) URL(string) (string, error) {
	return fmt.Sprintf("https://bedrock-mantle.%s.api.aws/anthropic/v1/messages", awsRegion(context.Background(), m.region)), nil
}

func (m *mantle) Auth(ctx context.Context, req *http.Request, body []byte) error {
	req.Header.Set("anthropic-version", "2023-06-01")
	if tok := env("AWS_BEARER_TOKEN_BEDROCK"); tok != "" {
		req.Header.Set("x-api-key", tok)
		return nil
	}
	cfg, err := awsConfig(ctx)
	if err != nil {
		return err
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("no AWS credentials (aws configure, aws sso login, or AWS_BEARER_TOKEN_BEDROCK): %w", err)
	}
	sum := sha256.Sum256(body)
	return v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "bedrock-mantle", awsRegion(ctx, m.region), time.Now())
}

var (
	brMu      sync.Mutex
	brClients = map[string]*bedrockruntime.Client{}
)

func bedrockClient(ctx context.Context, region string) (*bedrockruntime.Client, error) {
	cfg, err := awsConfig(ctx)
	if err != nil {
		return nil, err
	}
	brMu.Lock()
	defer brMu.Unlock()
	if c := brClients[region]; c != nil {
		return c, nil
	}
	c := bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) {
		o.Region = region
		o.Retryer = bedrockRetryer(cfg.RetryMaxAttempts)
	})
	brClients[region] = c
	return c, nil
}

// bedrockRetryer: the SDK already retries throttling, 5xx and transient
// network errors with jittered backoff, so Converse doesn't go through
// doRetry. Its defaults are tuned for control-plane calls: 3 attempts, and
// a client-wide retry quota that a burst of concurrent reviews drains, after
// which every throttled call fails at once. Match httpRetry instead:
// 5 attempts, backoff up to 30s, no quota. AWS_MAX_ATTEMPTS still wins.
func bedrockRetryer(maxAttempts int) aws.Retryer {
	if maxAttempts <= 0 {
		maxAttempts = httpRetry.MaxRetries + 1
	}
	return retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = maxAttempts
		o.MaxBackoff = httpRetry.Max
		o.RateLimiter = ratelimit.None
	})
}

// bedrockBaseModel strips what Bedrock wraps a model id in, for capability
// checks: an ARN's resource type, a cross-region inference profile's
// geography (us., eu., apac., global. ...), and a date and -v1:0 version
// suffix. us.anthropic.claude-haiku-4-5-20251001-v1:0 becomes
// anthropic.claude-haiku-4-5, the id Claude has on the Messages API.
// Application inference profile ARNs stay opaque.
func bedrockBaseModel(id string) string {
	if strings.HasPrefix(id, "arn:") {
		if i := strings.LastIndexByte(id, '/'); i >= 0 {
			id = id[i+1:]
		}
	}
	id = bedrockGeo.ReplaceAllString(id, "")
	id = bedrockVersion.ReplaceAllString(id, "")
	return bedrockDate.ReplaceAllString(id, "")
}

var (
	bedrockGeo     = regexp.MustCompile(`^(?:us|us-gov|eu|apac|jp|au|ca|global)\.`)
	bedrockVersion = regexp.MustCompile(`-v\d+(?::[0-9a-z]+)*$`)
	bedrockDate    = regexp.MustCompile(`-\d{8}$`)
)

// converseForced: whether a ToolChoiceRequired request names the tool, or
// falls back to auto plus an instruction, as the Messages API path does.
// Converse also rejects a specific tool choice for models without it (Llama,
// some Mistral), which then land in noForcedTool.
func converseForced(model string, req LLMRequest) bool {
	return req.ToolChoice == ToolChoiceRequired && len(req.Tools) > 0 && forcedToolOK(bedrockBaseModel(model))
}

// toolChoiceRejected: a Converse 400 about toolChoice, e.g. "This model
// doesn't support the toolConfig.toolChoice.tool field."
func toolChoiceRejected(err error) bool {
	var ve *brtypes.ValidationException
	if !errors.As(err, &ve) {
		return false
	}
	m := strings.ToLower(ve.ErrorMessage())
	return strings.Contains(m, "toolchoice") || strings.Contains(m, "tool_choice")
}

// converse is the Converse API path, for models Bedrock doesn't serve
// through the Messages API (Nova, Llama, Mistral, inference profiles and
// versioned or ARN Claude ids).
func (b *BedrockLLM) converse(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	client, err := bedrockClient(ctx, awsRegion(ctx, b.Region))
	if err != nil {
		return nil, err
	}
	forced := converseForced(b.ModelID(), req)
	out, err := client.Converse(ctx, converseInput(b.ModelID(), req, forced))
	if err != nil && forced && toolChoiceRejected(err) {
		noForcedTool.Store(bedrockBaseModel(b.ModelID()), true)
		out, err = client.Converse(ctx, converseInput(b.ModelID(), req, false))
	}
	if err != nil {
		return nil, fmt.Errorf("bedrock converse %s: %w", b.ModelID(), err)
	}
	return parseConverseOutput(out)
}

// converseInput builds a Converse request. Without forced, a
// ToolChoiceRequired request gets auto tool choice and an instruction to
// call the first tool.
func converseInput(model string, req LLMRequest, forced bool) *bedrockruntime.ConverseInput {
	in := &bedrockruntime.ConverseInput{ModelId: aws.String(model)}
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			in.System = append(in.System, &brtypes.SystemContentBlockMemberText{Value: m.Content})
		case "user", "assistant":
			in.Messages = append(in.Messages, brtypes.Message{
				Role:    brtypes.ConversationRole(m.Role),
				Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: m.Content}},
			})
		}
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 2048
	}
	in.InferenceConfig = &brtypes.InferenceConfiguration{MaxTokens: aws.Int32(maxTokens)}
	if len(req.Tools) > 0 && req.ToolChoice != ToolChoiceNone {
		cfg := &brtypes.ToolConfiguration{}
		for _, def := range req.Tools {
			schema := def.InputSchema
			if len(schema) == 0 {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			cfg.Tools = append(cfg.Tools, &brtypes.ToolMemberToolSpec{Value: brtypes.ToolSpecification{
				Name:        aws.String(def.Name),
				Description: aws.String(def.Description),
				InputSchema: &brtypes.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema)},
			}})
		}
		switch {
		case forced && req.ToolChoice == ToolChoiceRequired:
			cfg.ToolChoice = &brtypes.ToolChoiceMemberTool{Value: brtypes.SpecificToolChoice{Name: aws.String(req.Tools[0].Name)}}
		case req.ToolChoice == ToolChoiceRequired:
			in.System = append(in.System, &brtypes.SystemContentBlockMemberText{
				Value: fmt.Sprintf("Answer by calling the %s tool exactly once. Do not answer in text.", req.Tools[0].Name),
			})
			cfg.ToolChoice = &brtypes.ToolChoiceMemberAuto{Value: brtypes.AutoToolChoice{}}
		default:
			cfg.ToolChoice = &brtypes.ToolChoiceMemberAuto{Value: brtypes.AutoToolChoice{}}
		}
		in.ToolConfig = cfg
	}
	return in
}

func parseConverseOutput(out *bedrockruntime.ConverseOutput) (*LLMResponse, error) {
	resp := &LLMResponse{StopReason: mapBedrockStopReason(string(out.StopReason))}
	if out.Usage != nil {
		resp.Usage = Usage{
			InputTokens:  int(aws.ToInt32(out.Usage.InputTokens) + aws.ToInt32(out.Usage.CacheReadInputTokens) + aws.ToInt32(out.Usage.CacheWriteInputTokens)),
			OutputTokens: int(aws.ToInt32(out.Usage.OutputTokens)),
		}
	}
	msg, ok := out.Output.(*brtypes.ConverseOutputMemberMessage)
	if !ok {
		return resp, nil
	}
	var sb strings.Builder
	for _, blk := range msg.Value.Content {
		switch c := blk.(type) {
		case *brtypes.ContentBlockMemberText:
			sb.WriteString(c.Value)
		case *brtypes.ContentBlockMemberToolUse:
			args := map[string]any{}
			if c.Value.Input != nil {
				if err := c.Value.Input.UnmarshalSmithyDocument(&args); err != nil {
					return nil, fmt.Errorf("bedrock converse: tool input: %w", err)
				}
			}
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{Name: aws.ToString(c.Value.Name), Arguments: args, CallID: aws.ToString(c.Value.ToolUseId)})
		}
	}
	resp.Text = sb.String()
	return resp, nil
}

func mapBedrockStopReason(s string) StopReason {
	switch s {
	case "guardrail_intervened", "content_filtered":
		return StopRefusal
	}
	return mapAnthropicStopReason(s)
}

// awsSignals: whether this machine has any AWS setup worth probing, so the
// probe doesn't wait on the instance metadata service on every laptop.
func awsSignals() bool {
	if env("AWS_ACCESS_KEY_ID", "AWS_PROFILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" || truthy("CLAUDE_CODE_USE_BEDROCK") {
		return true
	}
	home, _ := os.UserHomeDir()
	return fileExists(env("AWS_CONFIG_FILE")) || fileExists(env("AWS_SHARED_CREDENTIALS_FILE")) ||
		fileExists(filepath.Join(home, ".aws", "config")) || fileExists(filepath.Join(home, ".aws", "credentials"))
}

func probeBedrock(ctx context.Context) Catalog {
	c := Catalog{Provider: Bedrock, Models: bedrockModels}
	region := awsRegion(ctx, "")
	if env("AWS_BEARER_TOKEN_BEDROCK") != "" {
		c.Available, c.Reason = true, "AWS_BEARER_TOKEN_BEDROCK, "+region
		return c
	}
	if !awsSignals() {
		c.Reason = "no AWS credentials (aws configure / aws sso login, or AWS_BEARER_TOKEN_BEDROCK)"
		return c
	}
	cfg, err := awsConfig(ctx)
	if err != nil {
		c.Reason = "AWS config: " + err.Error()
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		// The chain ends at the instance metadata service, whose error
		// hides what was actually missing.
		msg := firstLine(err.Error())
		if strings.Contains(msg, "IMDS") || strings.Contains(msg, "169.254.169.254") {
			msg = "none in the environment, profile or instance role"
		}
		c.Reason = "no AWS credentials (" + msg + "): aws configure / aws sso login, or AWS_BEARER_TOKEN_BEDROCK"
		return c
	}
	c.Available, c.Reason = true, fmt.Sprintf("AWS credentials (%s), %s", creds.Source, region)
	return c
}

// ---- Google Vertex AI ----

// vertex is Claude on Vertex AI: the model goes in the URL, and
// anthropic_version in the body. Auth is Application Default Credentials.
type vertex struct{}

func vertexProject() string {
	return env("ANTHROPIC_VERTEX_PROJECT_ID", "GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT")
}

// vertexRegion: global (the default) routes anywhere; us and eu are
// multi-region endpoints; anything else is one region.
func vertexRegion() string {
	if r := env("CLOUD_ML_REGION", "VERTEX_REGION"); r != "" {
		return r
	}
	return "global"
}

func (vertex) ID() string           { return Vertex }
func (vertex) DefaultModel() string { return VertexHaiku45 }

func (vertex) Body(p map[string]any) {
	delete(p, "model")
	p["anthropic_version"] = anthropicVertex
}

func (vertex) URL(model string) (string, error) {
	project := vertexProject()
	if project == "" {
		return "", errors.New("vertex: set ANTHROPIC_VERTEX_PROJECT_ID (or GOOGLE_CLOUD_PROJECT)")
	}
	region := vertexRegion()
	host := region + "-aiplatform.googleapis.com"
	switch region {
	case "global":
		host = "aiplatform.googleapis.com"
	case "us", "eu":
		host = "aiplatform." + region + ".rep.googleapis.com"
	}
	return fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:rawPredict", host, project, region, model), nil
}

var (
	gcpMu sync.Mutex
	gcpTS oauth2.TokenSource
)

// gcpTokenSource finds application default credentials once they are there;
// a failure is retried on the next call.
func gcpTokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	gcpMu.Lock()
	defer gcpMu.Unlock()
	if gcpTS == nil {
		ts, err := google.DefaultTokenSource(context.WithoutCancel(ctx), "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return nil, err
		}
		gcpTS = ts
	}
	return gcpTS, nil
}

func (vertex) Auth(ctx context.Context, req *http.Request, _ []byte) error {
	ts, err := gcpTokenSource(ctx)
	if err != nil {
		return fmt.Errorf("no Google credentials (gcloud auth application-default login): %w", err)
	}
	tok, err := ts.Token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	return nil
}

func probeVertex(ctx context.Context) Catalog {
	c := Catalog{Provider: Vertex, Models: vertexModels}
	project := vertexProject()
	if project == "" {
		c.Reason = "ANTHROPIC_VERTEX_PROJECT_ID (or GOOGLE_CLOUD_PROJECT) is not set"
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform"); err != nil {
		c.Reason = "no Google credentials (gcloud auth application-default login)"
		return c
	}
	c.Available, c.Reason = true, fmt.Sprintf("Google ADC, project %s, %s", project, vertexRegion())
	return c
}

// ---- Microsoft Foundry ----

// foundry is Claude in Microsoft Foundry: the Messages API under the
// resource's endpoint, with an API key or an Entra ID token. Models are
// deployment names.
type foundry struct{}

func foundryBase() string {
	if u := env("ANTHROPIC_FOUNDRY_BASE_URL"); u != "" {
		return strings.TrimSuffix(strings.TrimRight(u, "/"), "/v1")
	}
	if r := env("ANTHROPIC_FOUNDRY_RESOURCE"); r != "" {
		return "https://" + r + ".services.ai.azure.com/anthropic"
	}
	return ""
}

func (foundry) ID() string           { return Foundry }
func (foundry) DefaultModel() string { return FoundryHaiku45 }
func (foundry) Body(map[string]any)  {}

func (foundry) URL(string) (string, error) {
	base := foundryBase()
	if base == "" {
		return "", errors.New("foundry: set ANTHROPIC_FOUNDRY_RESOURCE (or ANTHROPIC_FOUNDRY_BASE_URL)")
	}
	return base + "/v1/messages", nil
}

func (foundry) Auth(ctx context.Context, req *http.Request, _ []byte) error {
	req.Header.Set("anthropic-version", "2023-06-01")
	if key := env("ANTHROPIC_FOUNDRY_API_KEY"); key != "" {
		req.Header.Set("api-key", key)
		return nil
	}
	tok, err := entraToken(ctx, "https://ai.azure.com/.default")
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

func probeFoundry(ctx context.Context) Catalog {
	c := Catalog{Provider: Foundry, Models: foundryModels}
	base := foundryBase()
	if base == "" {
		c.Reason = "ANTHROPIC_FOUNDRY_RESOURCE (or ANTHROPIC_FOUNDRY_BASE_URL) is not set"
		return c
	}
	if env("ANTHROPIC_FOUNDRY_API_KEY") != "" {
		c.Available, c.Reason = true, "ANTHROPIC_FOUNDRY_API_KEY, "+base
		return c
	}
	if _, err := entraToken(ctx, "https://ai.azure.com/.default"); err != nil {
		c.Reason = "no ANTHROPIC_FOUNDRY_API_KEY and no Entra ID login (az login): " + firstLine(err.Error())
		return c
	}
	c.Available, c.Reason = true, "Entra ID, "+base
	return c
}

// ---- Azure OpenAI ----

// NewAzureOpenAI is an OpenAI client for an Azure OpenAI (or Foundry)
// resource's v1 API. Models are deployment names.
func NewAzureOpenAI(model string) *OpenAILLM {
	return &OpenAILLM{
		Model:    model,
		BaseURL:  azureOpenAIBase(),
		Provider: AzureOpenAI,
		Auth: func(ctx context.Context, req *http.Request) error {
			if key := env("AZURE_OPENAI_API_KEY"); key != "" {
				req.Header.Set("api-key", key)
				return nil
			}
			tok, err := entraToken(ctx, "https://cognitiveservices.azure.com/.default")
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+tok)
			return nil
		},
	}
}

// azureOpenAIBase maps AZURE_OPENAI_ENDPOINT (https://NAME.openai.azure.com,
// with or without /openai/v1) to the root the /v1/... paths go under.
func azureOpenAIBase() string {
	u := strings.TrimRight(env("AZURE_OPENAI_ENDPOINT"), "/")
	if u == "" {
		return "https://missing-AZURE_OPENAI_ENDPOINT.invalid"
	}
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/v1"), "/openai")
	return u + "/openai"
}

func probeAzureOpenAI(ctx context.Context) Catalog {
	c := Catalog{Provider: AzureOpenAI}
	if env("AZURE_OPENAI_ENDPOINT") == "" {
		c.Reason = "AZURE_OPENAI_ENDPOINT is not set"
		return c
	}
	how := "AZURE_OPENAI_API_KEY"
	if env("AZURE_OPENAI_API_KEY") == "" {
		if _, err := entraToken(ctx, "https://cognitiveservices.azure.com/.default"); err != nil {
			c.Reason = "no AZURE_OPENAI_API_KEY and no Entra ID login (az login): " + firstLine(err.Error())
			return c
		}
		how = "Entra ID"
	}
	// Deployments can't be listed with the data-plane API, so the list is
	// AZURE_OPENAI_DEPLOYMENTS if set, else the OpenAI model names.
	c.Available, c.Reason = true, how+", "+env("AZURE_OPENAI_ENDPOINT")
	for _, d := range strings.Split(env("AZURE_OPENAI_DEPLOYMENTS"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			c.Models = append(c.Models, Model{ID: d})
		}
	}
	if len(c.Models) > 0 {
		c.Live = true
	} else {
		c.Models = fallbackModels[OpenAIAPI]
	}
	return c
}

// ---- Entra ID ----

var (
	entraCred azcore.TokenCredential // set once created; a failure is retried
	entraMu   sync.Mutex
	entraToks = map[string]azcore.AccessToken{} // scope -> token
)

// entraToken gets an Entra ID token for scope from DefaultAzureCredential
// (env service principal, workload identity, managed identity, az login),
// cached until five minutes before it expires. The Azure CLI credential
// runs az on every call, so the cache matters.
func entraToken(ctx context.Context, scope string) (string, error) {
	entraMu.Lock()
	defer entraMu.Unlock()
	if t, ok := entraToks[scope]; ok && time.Until(t.ExpiresOn) > 5*time.Minute {
		return t.Token, nil
	}
	if entraCred == nil {
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return "", err
		}
		entraCred = cred
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t, err := entraCred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{scope}})
	if err != nil {
		return "", err
	}
	entraToks[scope] = t
	return t.Token, nil
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
