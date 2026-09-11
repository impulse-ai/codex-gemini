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
