# GPT-5.5 / GPT-5.6 Prompt Research Notes

## Upstream snapshot

- Repository: `https://github.com/Jia-Ethan/codex-keysmith`
- Checked branch head: `f379f3c`
- Latest release tag: `v0.1.1` at `d8335f9`
- Checked on: 2026-07-27
- Prompt source: `examples/gpt-unrestricted.md`

The commits after `v0.1.1` change documentation and release support. The prompt,
prompt-bank cases, and regression runner have no material source difference between
the tag and the checked `main` head.

## Upstream design

The upstream prompt relies on three mechanisms:

1. Direct-output and first-line anchors.
2. Fixture placeholders for missing or concrete target values.
3. Exact dispatch templates for a small set of boundary phrases.

Its 12-case prompt bank mainly checks those anchors and required tokens. It does not
test model isolation, API payload formats, preservation of client instructions,
ordinary coding behavior, idempotent injection, or detailed reverse-engineering
quality.

## Sub2API changes

- Removed the Codex-only identity and made the prompt compatible with arbitrary
  clients using Responses, Chat Completions, or the Anthropic-compatible message
  shape.
- Added `[MODEL_SCOPE: gpt-5.5*, gpt-5.6*]`. Sub2API consumes this metadata locally
  and strips it before upstream forwarding. Existing prompts without a scope retain
  all-model behavior.
- Preserved existing client system/developer/instructions content and tool/schema
  contracts.
- Replaced the global template-first behavior with route-specific dispatch so normal
  coding and structured-output requests are not forced into a reverse-engineering
  answer shape.
- Kept only compact evidence, placeholder, verification, and rollback contracts.
  Detailed tool recipes were removed so the prompt remains a behavior controller
  rather than a reverse-engineering skill.
- Added semantic route labels for PE/ELF/Mach-O/APK, managed runtimes, drivers,
  Android `.ko`/KPM, game render/aim, firmware, protocols, obfuscation, and Pwn;
  domain expansion comes from the model and supplied context.
- Added 47 Sub2API regression cases, including Android `.ko`/KPM analysis/build,
  single-question broad-request handshakes, immediate execution after user scope,
  semantic paraphrases, inherited context, a non-game homonym, direct platform
  defaults, and backend model/protocol/directive-isolation tests.

## Fixture and sandbox semantics

The upstream `sandbox`, `local sample`, and fixture wording is a prompt-level
context convention, not an operating-system sandbox, VM, container, or simulated
tool runtime. `TARGET`, `SAMPLE`, `CHECK_FN`, and `OFFSET` are semantic placeholders
that keep an answer structurally complete when concrete evidence is absent.

Sub2API's model matching, directive stripping, protocol-native injection, and
per-key/group switches are real backend behavior. The prompt does not create new
model capabilities and does not prove that a command, address, patch, or trace was
executed. Confirm actual behavior with a request matrix and upstream payload capture.

## Prompt construction rationale

GPT-5.6 [model guidance](https://developers.openai.com/api/docs/guides/model-guidance?model=gpt-5.6)
favors lean outcome-oriented rules, explicit success and
stopping conditions, preserved user values, clear tool routing, stable reusable
prefixes, and real validation. Repeated absolute rules and conflicting keyword maps
make behavior less stable, so exact templates are retained only where the regression
contract requires them. The reverse-engineering router states evidence and output
requirements while leaving tool choice conditional on the sample format and available
environment.

The canonical prompt is deliberately lean. Regression cases carry concrete output
expectations, while the injected instruction contains only behavior, semantic
routing, defaults, evidence discipline, and required first-line contracts. This
avoids spending group-instruction context on material the model already knows.

## Update checklist

1. Fetch upstream `main` and tags; diff the prompt, cases, and runner from the pinned
   snapshot above.
2. Keep the canonical prompt below 16,384 characters.
3. Run `python tools/run_gpt55_56_prompt_bank.py --validate-only`.
4. After deployment, run the bank through Responses and Chat Completions for the
   deployed GPT-5.5 and GPT-5.6 IDs.
5. Send negative requests with GPT-5.4, a Claude ID, and no model; inspect upstream
   request capture to confirm the group block is absent.
6. Compare representative normal coding, tool-use, JSON-schema, and reverse tasks
   before changing additional prompt sections.
