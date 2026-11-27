# Our own model for errors outside the catalog

The catalog (`internal/ai/catalog*.go`) explains errors we know. When an issue matches no catalog entry, the app used to send a generic diagnose-and-fix prompt. With a model configured, it first asks our own model (Qwen, served by Ollama) for a specific explanation and fix prompt.

## How it works

- `ai.Analyzer.Analyze` → `ExplainWithModel`. It's called only when `Explain` returned the generic prompt (origin `agent`), and runs before the DB transaction.
- The model gets `ModelSystem` plus `Evidence(c)`: the scrubbed samples, stack, code references and previous failed attempts. Its reply is constrained to JSON by a schema (Ollama `format`), with `think:false`.
- `ParseModelSuggestion` validates the reply. If the model is down, times out or replies with invalid JSON, the generic prompt is used and a warning is logged. The model never blocks analysis.
- A model suggestion is stored with `origin='model'`, `prompt_version='model-v1'`, and the `model`, `tokens_*` and `latency_ms` columns filled in.
- Retries reach the model too. After a failed fix, `get_issue` and regressions re-analyze, and the model sees "Fixes already tried that did NOT work".

## Training data

Every suggestion, of any origin, writes a `training_examples` row in the same transaction. The row holds the scrubbed evidence (`input`) and the explanation before the data-driven additions (`output`).

- `verify.ReportFix` adds the agent's summary and the files it changed.
- `verify.Record` adds the outcome: worked, failed or partial, explicit or implicit.
- Projects opt out on Setup → "Help improve fix suggestions". That stops collection and deletes the project's existing rows. It's described in the privacy policy under §4 "training".
- `TrainingScrub` runs on top of ingest scrubbing and removes emails, IP addresses, known key formats, query values and long opaque strings.

`app export-training` turns the rows into fine-tuning files. See `training/README.md`.

## App configuration

| Env | Default | |
|---|---|---|
| `MODEL_URL` | unset (feature off) | e.g. `https://model.example.com`, or `http://localhost:11434` in dev |
| `MODEL_NAME` | `qwen3.5:4b` | switch to `errfix-qwen:vN` after a fine-tune passes the eval |
| `MODEL_TOKEN` | | bearer token checked by the model host's Caddy |
| `MODEL_TIMEOUT` | `90s` | per request; a CPU host needs about 60s for one answer |

## The model host

Northflank's free sandbox can't run a 4B model, so the model runs on its own box. A CPU VPS with 8 GB of RAM is enough (for example a Hetzner CX32). Expect about 4 tokens/s, or roughly a minute per explanation, which is fine because analysis is a background job.

```sh
# on the VPS, with Docker installed and DNS for MODEL_DOMAIN pointing at it
cd deploy/model && cp env.example .env   # fill MODEL_DOMAIN, MODEL_TOKEN
docker compose up -d
docker compose exec ollama ollama pull qwen3.5:4b
curl -H "Authorization: Bearer $MODEL_TOKEN" https://$MODEL_DOMAIN/api/version
```

Then set `MODEL_URL`, `MODEL_NAME` and `MODEL_TOKEN` as secrets on the Northflank service and redeploy.

To load a fine-tuned version, copy `model.gguf` and `training/Modelfile` into `deploy/model/models/vN/`, run `docker compose exec ollama sh -c 'cd /models/vN && ollama create errfix-qwen:vN -f Modelfile'`, then change `MODEL_NAME`.

## Local development

```sh
ollama pull qwen3.5:4b
MODEL_URL=http://localhost:11434 ./bin/app.exe serve
FIX_MODEL_URL=http://localhost:11434 go test ./internal/ai -run OllamaLive -v   # one live call
```

On this dev machine, the GPU build of Ollama 0.40.16 crashes on the Quadro P1000 ("PTX was compiled with an unsupported toolchain"). Update the NVIDIA driver, or run a CPU-only instance:

```sh
CUDA_VISIBLE_DEVICES=-1 GGML_VK_VISIBLE_DEVICES=-1 OLLAMA_LLM_LIBRARY=cpu OLLAMA_HOST=127.0.0.1:11435 ollama serve
```
