# Cost comparison assumptions

[Back to README](../README.md#cost-comparison). Rates checked **September 11, 2026**; this is a dated reference, not a live billing calculator.

## Which Codex models?

The comparison uses **GPT-5.6 Luna** and **GPT-5.6 Terra**, the two least expensive members of the current GPT-5.6 family available in Codex. GPT-5.4 mini retired from ChatGPT-authenticated Codex on August 31, 2026; API-key access is unaffected. GPT-5.3-Codex-Spark is listed as a research preview on the credit rate card, without a numeric rate, so it is not assigned a speculative price. Sources: [Codex model availability](https://learn.chatgpt.com/docs/models), [Codex pricing](https://learn.chatgpt.com/docs/pricing).

## API dollars versus Codex credits

The README uses standard API prices for text/code and OpenAI's short-context tier. Batch, Flex, Fast mode, long-context premiums, cache writes/storage, embeddings, paid tools, taxes, and negotiated discounts are excluded. Cached-input prices apply only to actual provider cache hits; shared SQLite memory is not a provider cache hit. Google lists cached-input storage separately. Sources: [OpenAI API pricing](https://developers.openai.com/api/docs/pricing), [Google API pricing](https://ai.google.dev/gemini-api/docs/pricing#gemini-3.8-flash).

For ChatGPT-authenticated Codex, included usage and purchased credits are a different billing system:

| Model | Input credits / 1M | Cached input credits / 1M | Output credits / 1M |
| --- | ---: | ---: | ---: |
| GPT-5.6 Luna | 5 | 0.5 | 30 |
| GPT-5.6 Terra | 50 | 5 | 300 |

Credit purchase prices and discounts depend on the plan or agreement. At 100k uncached input plus 10k output, these rates correspond to **0.8 Luna credits** or **8 Terra credits**. Do not compare those numbers directly with Gemini dollars. API-key-authenticated Codex uses API rates. Source: [Codex pricing and credit rates](https://learn.chatgpt.com/docs/pricing).

## Reproducing the example

For rates quoted per million tokens:

```text
cost = (uncached_input × input_rate
      + cached_input × cached_rate
      + billable_output × output_rate) / 1,000,000
```

With 100,000 uncached input tokens and 10,000 billable output tokens:

| Model | One hypothetical worker | 30 identical workers |
| --- | ---: | ---: |
| GPT-5.6 Luna | $0.0320 | $0.96 |
| Gemini 3.8 Flash (2026 promotional rate) | $0.1125 | $3.375 |
| GPT-5.6 Terra | $0.3200 | $9.60 |

Google explicitly includes thinking tokens in output pricing. Count billable reasoning as output when applying this example; visible response text alone is insufficient. The example assumes equal billed token counts, which different models and tokenizers do not guarantee. Google's announced January 2027 Gemini rates make the same uncached example $0.225 per worker. Source: [Google API pricing](https://ai.google.dev/gemini-api/docs/pricing#gemini-3.8-flash).

## What this means for offloading

Gemini is not the cheapest option in this comparison: Luna has lower listed input and output prices. Whether offloading saves money depends on task success, repeated context, retries, and Codex's orchestration and final review. A stalled worker still consumes tokens. Thirty concurrent workers increase capacity, not necessarily efficiency.

Measure the combined Gemini work and Codex integration needed for an accepted change. This project's usage and outcome metrics help inspect token use and edits, but do not yet measure accepted changes or the counterfactual cost of having another model perform the same work. Existing subscription allowance may cover Codex usage without an additional purchase; Gemini API usage is billed separately under the Google account's applicable terms.

## Performance per dollar

**Evidence checked September 11, 2026.** Google's model card reports Gemini 3.8 Flash at 73.7% on DeepSWE v1.1 and 89.4% on Terminal-Bench 2.1. OpenAI reports Luna at 67.2% / 84.7% and Terra at 69.6% / 87.4%, respectively. Google's Terminal-Bench 4.0 comparison instead favors Terra: 23.6% versus Gemini's 19.1%. Sources: [Google model card](https://deepmind.google/models/model-cards/gemini-3-8-flash/), [OpenAI evaluations](https://openai.com/index/gpt-5-6/).

Google describes its DeepSWE result as self-computed with mini-swe-agent and high thinking; competitor results use leaderboard values at their highest-scoring thinking settings. Terminal-Bench 2.1 uses Terminus 2, with Gemini results self-computed and competitor results sourced externally. These are not one controlled comparison inside codex-gemini. This integration defaults to low thinking, and its workers cannot execute shell commands. Source: [Google evaluation methodology](https://deepmind.google/models/evals-methodology/gemini-3-8-flash).

### Illustrative efficiency, not measured benchmark spend

If every attempt cost the README's hypothetical 100k-input/10k-output amount **and** the published DeepSWE success rates applied, then:

```text
illustrative successes per dollar = success fraction / assumed cost per attempt
```

| Model | Assumed cost per attempt | Published DeepSWE score | Illustrative successes / $ |
| --- | ---: | ---: | ---: |
| GPT-5.6 Luna | $0.0320 | 67.2% | 21.00 |
| Gemini 3.8 Flash | $0.1125 | 73.7% | 6.55 |
| GPT-5.6 Terra | $0.3200 | 69.6% | 2.18 |

Under those assumptions, Gemini is about **3.01× Terra's efficiency**, while **Luna is about 3.21× Gemini's**. These are calculated scenarios, not measured tasks completed per dollar. The input/output mix is invented for comparison; actual benchmark token counts, caching, tool costs, and repair work are not included. Do not present these ratios as observed savings.

For Gemini to beat Luna using these success fractions, its actual all-in cost per attempt would need to be less than **1.097× Luna's** (`0.737 / 0.672`); the equal-token pricing example is 3.516×. Against Terra, the corresponding threshold is **1.059× Terra's** (`0.737 / 0.696`). Actual task-specific success rates can differ substantially.

### Establishing value in this integration

A defensible measurement uses the same repository snapshots, assignments, acceptance tests, and resource limits across models. Count failed attempts, retries, billable reasoning, embeddings, and Codex's review/repair work. Report total spend divided by independently accepted changes, alongside success rate and elapsed time. Preserve model IDs, reasoning settings, prices, cache usage, and sample size so results can be reproduced.

We have not run that controlled comparison. Current evidence supports evaluating Gemini as an alternative to Terra for bounded coding work; it does not establish a universal performance-per-dollar advantage, especially over Luna.
