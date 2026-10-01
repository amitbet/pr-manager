# Models and providers

With `-classifier auto` (the default) the first available provider wins:

1. `codex`: the Codex CLI logged in with ChatGPT (`codex login`). gpt-6-luna classifies, gpt-6-sol summarizes and reviews.
2. `claude-code`: the Claude Code CLI logged in with a Claude plan (`claude auth login`). Haiku 4.5 classifies, Opus 5.5 summarizes and reviews. It runs without the variables that would send it elsewhere (`ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_USE_BEDROCK`/`_VERTEX`/`_FOUNDRY` and the other cloud settings, `ANTHROPIC_MODEL` and the other model overrides), so the cloud providers' settings don't move it off the plan; set `PR_MANAGER_CLAUDE_CODE_KEEP_ENV=1` to keep them for a Claude Code you route through a gateway or cloud.
3. A cloud account this machine is set up for, the way Claude Code is: `bedrock` (`CLAUDE_CODE_USE_BEDROCK=1` or `AWS_BEARER_TOKEN_BEDROCK`), `vertex` (`CLAUDE_CODE_USE_VERTEX=1` or `ANTHROPIC_VERTEX_PROJECT_ID`), `foundry` (`CLAUDE_CODE_USE_FOUNDRY=1` or `ANTHROPIC_FOUNDRY_RESOURCE`), then `azure-openai` (`AZURE_OPENAI_ENDPOINT`).
4. `openai-api`: `OPENAI_API_KEY`. gpt-5.4-mini classifies, gpt-6-sol summarizes and reviews.
5. `claude-api`: `ANTHROPIC_API_KEY`. Haiku 4.5 classifies, Sonnet 5 summarizes and reviews.
6. Ollama.

The small model only classifies with `-summarizer off`. Otherwise the stronger model places each unit in its review call, and the small one only translates.

## Cloud and gateway providers

For companies that only allow models through their own cloud account or an LLM gateway. Each uses the cloud's standard credentials and the environment variables its SDKs and Claude Code already read. Haiku 4.5 classifies and Sonnet 5 summarizes and reviews, unless a model is picked.

| provider | credentials | settings | models |
| --- | --- | --- | --- |
| `bedrock` | the AWS chain (env keys, profiles and SSO, EKS web identity, instance roles), or a Bedrock API key in `AWS_BEARER_TOKEN_BEDROCK` | `AWS_REGION` (default: the profile's, else us-east-1) | `anthropic.claude-*` ids go to the Messages API on Bedrock (`bedrock-mantle.{region}.api.aws`); any other id, such as an inference-profile ARN, Nova or Llama, goes to the Converse API |
| `vertex` | Google Application Default Credentials (`gcloud auth application-default login`, a service account, workload identity) | `ANTHROPIC_VERTEX_PROJECT_ID` (or `GOOGLE_CLOUD_PROJECT`), `CLOUD_ML_REGION` (default `global`; `us`, `eu` or a region) | `claude-sonnet-5`, `claude-haiku-4-5@20251001`, ... |
| `foundry` | `ANTHROPIC_FOUNDRY_API_KEY`, else Entra ID (`az login`, managed or workload identity, a service principal in env) | `ANTHROPIC_FOUNDRY_RESOURCE` or `ANTHROPIC_FOUNDRY_BASE_URL` | deployment names (default `claude-haiku-4-5`, `claude-sonnet-5`) |
| `azure-openai` | `AZURE_OPENAI_API_KEY`, else Entra ID | `AZURE_OPENAI_ENDPOINT`; `AZURE_OPENAI_DEPLOYMENTS` (comma-separated) fills the model list | deployment names (default `AZURE_OPENAI_CLASSIFY_DEPLOYMENT` / `AZURE_OPENAI_REVIEW_DEPLOYMENT`, else `gpt-5.4-mini` / `gpt-6-sol`) |

Gateways: `OPENAI_BASE_URL` points `openai-api` at any OpenAI-compatible server (LiteLLM, Portkey, vLLM), and `ANTHROPIC_BASE_URL` points `claude-api` at an Anthropic-compatible one, with `ANTHROPIC_AUTH_TOKEN` as a bearer token when the gateway doesn't take `x-api-key`. Settings shows which gateway a provider uses.

Claude Opus 5.5 and Fable 5.1 reject a forced tool call, so on every Claude provider those models get `tool_choice: auto` with an instruction to call the tool. A model the tool doesn't know (a Foundry deployment name, say) is switched the first time the API says so. An answer without the call still fails the unit, as before.

The API providers end in `-api` so they aren't confused with the local assistants (`codex`, `claude-code`) that use a subscription. The old names `openai`, `anthropic` and `claude` still work on the command line and in requests.

The subscription providers run the CLI on the server's machine once per call (`codex exec --output-schema`, `claude -p --json-schema`) in an empty temp directory with no tools, hooks or MCP servers (with review tools on, the review runs in the PR-head worktree with read-only tools instead). API keys are removed from their environment, so a key in `.env` never takes the place of the subscription. Login is checked once per process (`codex login status`, `claude auth status`); a Codex login that uses an API key doesn't count as a subscription. Codex needs a strict schema, so optional fields are sent as nullable and nulls are dropped from the answer.

`make` reads `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` from `ENV_FILE` (default `.env`, git-ignored; copy `.env.example`) when they aren't already exported.

OpenAI reasoning effort is `-classify-effort` (default `low`) and `-review-effort` (default `medium`); `''` uses the model's default and `none` turns reasoning off. Any other effort goes through the Responses API, because gpt-5.6 and gpt-6 models only accept function tools with reasoning there. The output caps (4096 classify, 8192 review) include reasoning tokens. `codex` and `claude-code` take the same flags (`minimal` becomes `low`; Haiku has no effort setting). The Anthropic API ignores them: extended thinking can't be combined with the forced tool call each step uses. The effort is part of the result cache key and shows in the UI next to the model (`openai-api/gpt-6-sol @medium`).

Classify (with `-summarizer off`) runs `-j` calls at a time (default 8), each holding up to `-classify-batch` units (default 6); analyze runs `-review-j` (default 16; `0` uses `-j`). Review calls take much longer, especially with review tools, so the stage gets its own limit. Lower it if the provider starts rate-limiting.
