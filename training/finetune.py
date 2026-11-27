"""LoRA fine-tune of our error-explaining model on exported SFT data.

Runs on one CUDA GPU with 8+ GB (the free Colab T4 works). See README.md.

    python finetune.py --train data/sft.jsonl --eval data/sft.eval.jsonl \
        --extra data/catalog_tests.jsonl --out out/v1

Writes out/v1/lora (the adapter) and out/v1/gguf (a Q4_K_M GGUF for Ollama).
"""

import argparse
import json
import os

from datasets import Dataset
from trl import SFTConfig, SFTTrainer
from unsloth import FastLanguageModel
from unsloth.chat_templates import train_on_responses_only


def load(paths):
    rows = []
    for p in paths:
        if not p or not os.path.exists(p):
            continue
        with open(p, encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if line:
                    rows.append({"messages": json.loads(line)["messages"]})
    return rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--train", required=True)
    ap.add_argument("--eval", default="")
    ap.add_argument("--extra", nargs="*", default=[], help="more SFT files, e.g. the catalog bootstrap")
    # The Hugging Face twin of the Ollama base model (qwen3.5:4b). Check the
    # exact repo name on huggingface.co/unsloth before the first run.
    ap.add_argument("--base", default="unsloth/Qwen3.5-4B")
    ap.add_argument("--out", default="out/v1")
    ap.add_argument("--epochs", type=float, default=2)
    ap.add_argument("--lr", type=float, default=2e-4)
    ap.add_argument("--rank", type=int, default=16)
    ap.add_argument("--max-len", type=int, default=3072)
    ap.add_argument("--quant", default="q4_k_m", help="GGUF quantisation; '' skips the GGUF export")
    a = ap.parse_args()

    model, tok = FastLanguageModel.from_pretrained(a.base, max_seq_length=a.max_len, load_in_4bit=True)
    model = FastLanguageModel.get_peft_model(
        model,
        r=a.rank,
        lora_alpha=a.rank,
        lora_dropout=0,
        target_modules=["q_proj", "k_proj", "v_proj", "o_proj", "gate_proj", "up_proj", "down_proj"],
        use_gradient_checkpointing="unsloth",
        random_state=3407,
    )

    def render(row):
        # Serving runs with thinking off (think:false), so train that way.
        try:
            text = tok.apply_chat_template(row["messages"], tokenize=False, enable_thinking=False)
        except TypeError:
            text = tok.apply_chat_template(row["messages"], tokenize=False)
        return {"text": text}

    train = Dataset.from_list(load([a.train, *a.extra])).map(render, remove_columns=["messages"])
    ev_rows = load([a.eval])
    ev = Dataset.from_list(ev_rows).map(render, remove_columns=["messages"]) if ev_rows else None
    print(f"train {len(train)} examples, eval {len(ev_rows)}")

    trainer = SFTTrainer(
        model=model,
        tokenizer=tok,
        train_dataset=train,
        eval_dataset=ev,
        args=SFTConfig(
            dataset_text_field="text",
            max_seq_length=a.max_len,
            per_device_train_batch_size=2,
            gradient_accumulation_steps=4,
            num_train_epochs=a.epochs,
            learning_rate=a.lr,
            warmup_ratio=0.05,
            lr_scheduler_type="cosine",
            logging_steps=5,
            eval_strategy="epoch" if ev is not None else "no",
            optim="adamw_8bit",
            weight_decay=0.01,
            seed=3407,
            output_dir=os.path.join(a.out, "checkpoints"),
            report_to="none",
        ),
    )
    # Learn the answers only, not the evidence or the system prompt.
    trainer = train_on_responses_only(
        trainer, instruction_part="<|im_start|>user\n", response_part="<|im_start|>assistant\n"
    )
    trainer.train()

    model.save_pretrained(os.path.join(a.out, "lora"))
    tok.save_pretrained(os.path.join(a.out, "lora"))
    if a.quant:
        model.save_pretrained_gguf(os.path.join(a.out, "gguf"), tok, quantization_method=a.quant)
        print(f"GGUF written to {a.out}/gguf; see README.md to load it into Ollama")


if __name__ == "__main__":
    main()
