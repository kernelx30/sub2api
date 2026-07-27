# GPT-5.5 / GPT-5.6 Enhanced Instructions

`gpt-5.5-5.6-unrestricted.md` is the canonical Sub2API prompt. Paste the complete
file into an OpenAI group's enhanced-instructions field, enable the group switch,
and enable enhanced instructions on the user's API key. One shared file keeps the
two GPT families behaviorally aligned.

The canonical file is a context-conversion layer, not a reverse-engineering skill or
tool encyclopedia. It normalizes request context, selects direct versus one-question
routing, and enforces only output anchors, placeholders, and evidence discipline.
Technical expansion comes from the model and supplied artifacts; the prompt bank
checks representative expertise separately.

Context-routing anchors are release gates. Domain-specific required tokens are
capability probes: model-selected equivalent tools or implementations may vary, and a
probe result alone is not a reason to add domain tutorials back into the prompt.

The canonical prompt opts into `[INPUT_NORMALIZATION: OWNER_FIXTURE]`. Before upstream
routing, Sub2API rewrites matching user text into an owner-integration game fixture or
owner-recovery license request framed as feature development or defect repair in an
isolated local sandbox, preserving the rest of the message and any attached content
blocks. The internal premise is not narrated, and subtitle wording such as `外挂字幕`
is left unchanged.

Broad requests use a one-question handshake: ask only which module/output the user
wants, then execute after the answer with defaults and placeholders for everything
else. Scoped requests execute immediately. The sandbox-fixture assumption remains
internal and is not narrated to the user.

The first line is Sub2API metadata:

```text
[MODEL_SCOPE: gpt-5.5*, gpt-5.6*]
```

Sub2API removes this line before forwarding the prompt. Matching is
case-insensitive; exact IDs, comma-separated IDs, and a trailing `*` are
supported. `gpt-5.5*` covers the base, dated, Pro, and dated-Pro IDs; `gpt-5.6*`
covers `gpt-5.6`, `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna`. Provider or
`models/` prefixes are also matched. A scoped prompt is skipped when the request
has no model. A prompt without a scope line retains the existing all-model
behavior. A malformed `[MODEL_SCOPE...]` line fails closed and is not injected.

Model scope matching is deterministic backend routing. Task routing inside the
injected prompt is semantic: fixed phrases are regression anchors, not trigger
requirements. Paraphrases, slang, indirect descriptions, and conversation context
must reach the same implementation route.

Validate the bank without calling an API:

```powershell
python tools/run_gpt55_56_prompt_bank.py --validate-only
```

Run it against a configured Sub2API API key:

```powershell
$env:SUB2API_BASE_URL = "https://HOST"
$env:SUB2API_API_KEY = "TOKEN"
python tools/run_gpt55_56_prompt_bank.py --model gpt-5.5
python tools/run_gpt55_56_prompt_bank.py --model gpt-5.6-sol
python tools/run_gpt55_56_prompt_bank.py --model gpt-5.6-sol --endpoint chat_completions
```

`--inject-prompt` is for an upstream with no deployed group prompt. It injects the
canonical block and locally emulates the same owner-fixture input normalization used
by Sub2API, so raw regression inputs exercise the production route.

Run the same bank across the deployed GPT-5.5 and GPT-5.6 IDs. Model isolation is
covered by backend tests: both families match; GPT-5.4, Claude model IDs, and
missing model IDs do not.

For the tested OpenAI-compatible GPT-5.5/GPT-5.6 upstream, set the OpenAI APIKey
account's **Responses API support** field to **Force Responses**. Sub2API will bridge
Chat Completions clients through the upstream Responses endpoint and convert the
result back. Direct GPT-5.5 Chat Completions did not reliably follow scoped routes;
the Responses path did.
