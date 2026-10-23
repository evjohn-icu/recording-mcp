# Worldline decision model experiment

The scored comparison uses 34 fixed synthetic examples, too few to establish production accuracy. A separate paired private-data trial below measures agreement and structural consistency, not accuracy.

## Reproduction

- Fixture: `fixtures/worldline-decision-experiment.json`, version `worldline-decision-experiment-v2`, SHA-256 `08a056b35ef64aa57ed00a33949ce95aa776d4f622965d0687deac943cd0fcd6`.
- Runner: `scripts/worldline-decision-experiment.py`, SHA-256 `ea1a46de84bf9844966386f58f7c7b13025991c2144dadc0af257232868e26b1`.
- 34 cases: 17 topic-alias comparisons and 17 summary-support judgments. The runner uses seed `20261023`, the same synthetic warmup, one question per case, a 45-second request timeout, and validates each full probability distribution.
- The Decision-v3 services use their packaged typed-logit readouts and calibrated probabilities. 0.8B uses FP16 CUDA; 2B uses BF16 CUDA and its own render-v2/block-attention implementation.
- Jev v2 uses the already-running local SystemOne wrapper, which requests next-token letter log probabilities from the Q8 GGUF model and normalizes those logits over the declared choices. This is a distinct prompt/readout/calibration path from Decision-v3. It is not a generated JSON answer presented as a typed distribution.

## Results

| Model | Model/weight identity | Load time | Overall | Alias task | Summary task | Request latency p50 / p95 |
|---|---|---:|---:|---:|---:|---:|
| Jev-Style 0.8B Decision-v3 | `b023d1f9c7858fbf01504577a3bfc349ea5c7385` | 5.58 s | 24/34 (70.6%) | 13/17 (76.5%) | 11/17 (64.7%) | 93.9 / 119.0 ms |
| Jev-Style 2B Decision-v3 | `5bad2d53ae04e832e2b30aa2a89bec76ec03deec` | 8.26 s | 27/34 (79.4%) | 12/17 (70.6%) | 15/17 (88.2%) | 362.0 / 467.1 ms |
| Jev-Style v2 Q8 GGUF | API reports `jev-style-2b-v2`; API does not report a weight version. Local GGUF SHA-256: `5c2aa0d35b24a27f03228b2c62ebaaebd9b5b785844634d4217278d822751494`. | Not available; service was already running | 17/34 (50.0%) | 8/17 (47.1%) | 9/17 (52.9%) | 129.8 / 156.0 ms |

The v2 adapter source used for this run had SHA-256 `c2607896b44bb9ba3b9dc71442f3b8f9a774bc1067b5cae5eb778348222e0044`. Its API does not expose that source or GGUF fingerprint; these hashes are recorded separately from the API model identity.

The two error rates below count only the declared negative examples: false-same-topic is `non-same_topic` alias cases predicted as `same_topic` (13 cases); false-supported is summary cases other than `supported` predicted as `supported` (14 cases).

| Model | False same-topic | False supported |
|---|---:|---:|
| Jev-Style 0.8B Decision-v3 | 1/13 (7.7%) | 3/14 (21.4%) |
| Jev-Style 2B Decision-v3 | 5/13 (38.5%) | 0/14 (0%) |
| Jev-Style v2 Q8 GGUF | 5/13 (38.5%) | 8/14 (57.1%) |

## Confusion by expected label

Each cell lists the predicted labels and counts for that expected-label row. `insufficient` abbreviates `insufficient_evidence`; `different`, `related`, and `same` abbreviate the corresponding `*_topic` labels. Summary labels are written in full.

### Topic alias

| Expected | 0.8B Decision-v3 | 2B Decision-v3 | Jev v2 Q8 |
|---|---|---|---|
| `different_topic` (5) | different 5 | different 5 | different 2, related 3 |
| `insufficient_evidence` (4) | insufficient 4 | insufficient 2, same 2 | different 1, insufficient 2, same 1 |
| `related_topic` (4) | insufficient 3, same 1 | related 1, same 3 | same 4 |
| `same_topic` (4) | same 4 | same 4 | same 4 |

### Summary support

| Expected | 0.8B Decision-v3 | 2B Decision-v3 | Jev v2 Q8 |
|---|---|---|---|
| `insufficient_evidence` (2) | `no_evidence` 1, `supported` 1 | `insufficient_evidence` 2 | `insufficient_evidence` 1, `supported` 1 |
| `no_evidence` (1) | `no_evidence` 1 | `no_evidence` 1 | `no_evidence` 1 |
| `plan_not_fact` (4) | `plan_not_fact` 4 | `plan_not_fact` 4 | `plan_not_fact` 4 |
| `supported` (3) | `supported` 2, `wrong_time` 1 | `supported` 2, `insufficient_evidence` 1 | `supported` 3 |
| `unsupported` (1) | `no_evidence` 1 | `no_evidence` 1 | `supported` 1 |
| `wrong_subject` (3) | `supported` 2, `wrong_subject` 1 | `wrong_subject` 3 | `supported` 3 |
| `wrong_time` (3) | `wrong_time` 3 | `wrong_time` 3 | `supported` 3 |

## Limits

The 2B Decision-v3 model scored better on summary support in this fixture (15/17), but that is not enough to publish a fact or change a worldline claim without review. Its alias task has five false same-topic predictions among 13 non-same examples; it must not make a final merge or same-fact decision. The 0.8B model made one false same-topic prediction among those 13 cases, so at most use its output as a related-content suggestion for review; it still missed all four `related_topic` examples. The 0.8B Decision-v3 path was fastest here. Jev v2's error rates do not support automatic use for these two tasks.

Treat all three outputs as shadow suggestions pending human review. Do not turn any model relation into identity confirmation, fact merge, or published truth. The aggregate fixture ranking does not transfer to real data. The superseded 32-case fixture-v1 run is excluded from this report.

## Local 2B service command

To run Decision-v3 2B through the same typed SystemOne adapter on loopback:

```bash
/tmp/jev-decision-venv/bin/python /home/ev/code/recording-mcp/scripts/decision-local-server.py \
  --verify \
  --model-dir /home/ev/.cache/huggingface/hub/models--chaoliangUNSW--Jev-Style-2B-Decision-v3/snapshots/5bad2d53ae04e832e2b30aa2a89bec76ec03deec \
  --device auto --dtype auto --addr 127.0.0.1:8799
```

The adapter reports `Jev-Style-2B-Decision-v3` and snapshot `5bad2d53ae04e832e2b30aa2a89bec76ec03deec`. It auto-selects BF16 on CUDA for this model's packaged runtime. The current 0.8B service uses the same port and must be stopped before starting this command.

## Saved synthetic results

Per-case outputs, distributions, warmup and latency measurements are available in [0.8B results](evals/worldline-decision-v2-0.8b.json), [2B results](evals/worldline-decision-v2-2b.json), and [v2 results](evals/worldline-decision-v2-jev-style-v2.json). These files contain synthetic case IDs and labels only, not private recordings.

A post-hoc check on this same small fixture found four alias cases where both Decision-v3 models returned `same_topic`; all four match their fixture labels. This is not an independent validation set and does not justify automatic merging. The private-data trial therefore preserves each model's separate judgment and asks for human review, including where both agree.


## Paired local private-data trial (2026-10-23)

PASS — Both Decision-v3 models evaluated the same frozen 37 inputs: 13 different-title topic pairs and 24 summary checks. Both returned 37 valid predictions; source versions and complete evidence were checked before and after each run. No profile writes were performed. Private text and per-case results remain in ignored local files.

Sampling is bounded, not representative: the first 300 current thread summaries, at most 64 thread details, macro-topic round-robin summary selection, and title bigram Jaccard >= 0.2 for alias shortlisting. Cases with incomplete evidence or more than two evidence references were skipped rather than truncated. Of the summary cases, 23 have one source and one has two; claim kinds are intention (2), observation (10), request (10), and suggestion (2).

| Check | 0.8B Decision-v3 | 2B Decision-v3 |
|---|---:|---:|
| Valid responses | 37/37 | 37/37 |
| Alias: same / insufficient | 12 / 1 | 12 / 1 |
| Summary: supported | 11 | 19 |
| Summary: no evidence | 12 | 3 |
| Summary: plan not fact / insufficient | 1 / 0 | 0 / 2 |

The models agree on 26 cases and disagree on 11: aliases 13/13 agree; summaries 13/24 agree. Agreement is not correctness. FAIL — structural consistency: all 24 summary inputs include original evidence, yet the models emitted 15 `no_evidence` judgments in total. The comparator preserves these outputs, warns that they cannot be adopted, and places affected cases first. Evidence existence must be checked in code, separately from whether a quote supports a claim.

The trial supports testing 0.8B for topic-association suggestions and 2B for summary review; neither may automatically merge topics or confirm reality. NOT_RUN — human gold-label adjudication of private cases, private semantic accuracy, and production-scale validation. This experiment does not change production retrieval or automatically publish model judgments.

Local artifacts (private, not committed):

- `data/worldline-local/decision-trial/prepared/trial-input.json`: frozen inputs.
- `data/worldline-local/decision-trial/{0.8b,2b}/trial-results.json`: separate model results.
- `data/worldline-local/decision-trial/comparison/comparison.html`: side-by-side Chinese review report, quotes, and local source links.

Reproduce using `scripts/worldline-private-trial.py --help`, then run `scripts/compare-worldline-trials.py` on the two output directories. The latter requires identical input snapshots and performs no model calls or memory updates. Reports are private snapshots; they are not indexed memories or automatically maintained projections.

PASS — 24 synthetic experiment/private-runner/comparator unit tests. PASS — headless Chrome opened the real local report: 37 cases, 15 warnings, 53 source links, an authenticated source page returned HTTP 200, and no JavaScript errors occurred. This checks rendering/navigation, not semantic correctness or audio playback. NOT_RUN — audio playback in this experiment.

PASS — final original-library integrity check: 25,113 files retain their size and modification time, and the database SHA-256 is unchanged. The 0.8B service is restored on loopback port 8799; its health endpoint reports `Jev-Style-0.8B-Decision-v3`. Web port 8790 and the existing model service on 8082 return HTTP 200. No migration is introduced by these experiment scripts; rollback consists of stopping their local adapter and removing the optional private reports.
