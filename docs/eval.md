# Eval and OpenJev

```sh
./pr-manager eval -fixtures testdata/eval [-classifier ...] [-judge]
```

Each `testdata/eval/NAME.json` is either a past PR in a local clone (`repo`, `base`, `head`) or a saved diff plus the head versions of the changed files (`diff`, `head_dir`, and optionally `base_dir` with the merge-base versions). `labels` maps unit IDs (`file:Symbol`) or file paths to buckets. The key metric is **MISSES human→none**. Over-escalation is tolerable. The eval also re-places every unit under each review budget and prints the bucket counts, the human-labeled units outside human (`under`), and the units with a `must_find` issue outside human (`defects`). Use those to tune the budget steps.

`findings` scores the review itself, per unit ID or file:

```json
"findings": {"store/client.go": {
  "must_find":     [{"match": "transient\\w* errors?[^.]*cach", "min_severity": "low", "max_severity": "medium"}],
  "must_not_find": [{"match": "pointer"}, {"match": "reconnect", "min_severity": "medium"}]}}
```

`match` is a case-insensitive regexp over an issue's title and detail. A `must_find` rule counts as found when an issue matches at a severity inside its range, and as a severity miss when the only match is outside it. A `must_not_find` rule is broken by a matching issue at `min_severity` (default low) or above, so `{"match": "reconnect", "min_severity": "medium"}` accepts it as a low. The eval prints recall on `must_find`, the false positives, and precision over the issues on labeled keys. `inventory-error-cache` moves a retry loop into a new function and adds a negative cache for failed lookups. It checks that the review flags transient errors being cached like permanent ones, does not claim the value-typed `ReplyError` escapes `errors.As`, and rates the pre-existing reconnect as low at most.

`-judge` asks OpenJev how faithful each generated summary is, and lists the ones scoring below 0.5.

## OpenJev

[OpenJev](https://github.com/lookski/openjev) runs a local model for one forward pass and returns softmax probabilities over fixed answers. It generates no text, so its confidence values are real probabilities, not something the model reports about itself. It is fast and free. It can't reason before it answers, so here it only settles units it is very sure about (`DefaultJevAccept`: human ≥ 0.8, skim ≥ 0.9, none ≥ 0.97 plus p(behavior change) ≤ 0.03). Everything else goes to `-fallback`.

```sh
git clone https://github.com/lookski/openjev && cd openjev && pip install -e .
openjev-easy          # serves http://127.0.0.1:8771 (override with OPENJEV_BASE_URL / -openjev-url)
```

The default model (Qwen3-0.6B) is too small to judge code. Use a larger code model, then run `eval` with `-classifier openjev -fallback off` to see how much it gets right on its own before trusting it.
