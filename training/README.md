# Fine-tuning the error model

The loop is: **export → fine-tune → evaluate → deploy only if it's better**. How the model is used is covered in [docs/model.md](../docs/model.md).

## 1. Export

```sh
# Production rows (needs DATABASE_URL) + the golden set's catalog answers.
app export-training --format sft --out training/data/sft.jsonl          # writes sft.jsonl + sft.eval.jsonl
# Extra bootstrap data: the catalog test cases (about 100, one or more per catalog entry).
FIX_BOOTSTRAP_OUT=training/data/catalog_tests.jsonl go test ./internal/ai -run ExportCatalogBootstrap
# Later, once there are failed-then-worked pairs:
app export-training --format dpo --out training/data/dpo.jsonl
```

- **SFT targets:** fixes that **worked** (any origin), plus catalog answers nobody reported as failing. Generic prompts are never used as targets.
- **Eval split:** about 10% of error patterns go to `.eval.jsonl`, chosen by signature, so the same pattern never ends up in both splits.
- **`--no-db`:** exports only the bootstrap data.
- **Privacy:** `training/data/` is gitignored. The data is scrubbed but comes from customers, so keep it off shared drives, and delete local copies after training.

## 2. Fine-tune (Colab T4 or any GPU with 8+ GB)

This dev PC's Quadro P1000 (4 GB) is too small for this step.

```sh
pip install -r training/requirements.txt
python training/finetune.py --train training/data/sft.jsonl --eval training/data/sft.eval.jsonl \
  --extra training/data/catalog_tests.jsonl --out training/out/v1
```

- **What it does:** a QLoRA fine-tune (rank 16, 2 epochs). The loss is computed on the assistant's JSON answers only, with thinking turned off to match serving.
- **Output:** `training/out/v1/lora` (the adapter) and `training/out/v1/gguf` (Q4_K_M).
- **Base model:** check the `--base` default (`unsloth/Qwen3.5-4B`) against the Hugging Face repo name before the first run. It has to be the same model family as the `qwen3.5:4b` served in Ollama.
- **First run:** with only bootstrap data, it mostly teaches the output format and the catalog's voice. That's useful, but the real gains come from production rows marked `worked`.
- **When to retrain:** after about 300 new `worked` rows, or once a month. Check the counts with `select * from training_examples` grouped by outcome, or `TrainingCounts`.

## 3. Evaluate

```sh
ollama create errfix-qwen:v1 -f training/Modelfile        # run next to model.gguf
app eval --model-url http://localhost:11434 --model-name qwen3.5:4b       # baseline
app eval --model-url http://localhost:11434 --model-name errfix-qwen:v1 --min-pass 0.6
```

`eval --model-url` sends every golden case to the model with the catalog bypassed. It applies the same rubric as the catalog: required mentions, forbidden phrases, length, "how to check" and severity. It also counts invalid replies. **Promote a version only if it beats the one in production.**

## 4. Deploy

Load the GGUF on the model host and change `MODEL_NAME` (see docs/model.md). After that, watch real worked rates per model version in `fix_suggestions` (`origin='model'`, grouped by `model`).

## Later

- **DPO:** once `dpo.jsonl` has a few hundred pairs, train a preference pass on top of the SFT adapter (TRL `DPOTrainer`, same chat format).
- **Catalog candidates:** `model` answers with a high worked rate are good candidates for new catalog entries.
