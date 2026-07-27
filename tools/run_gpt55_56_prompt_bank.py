#!/usr/bin/env python3
"""Run the Sub2API GPT-5.5/GPT-5.6 enhanced-instructions regression bank."""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


REPO_ROOT = Path(__file__).resolve().parents[1]
DEFAULT_CASES = REPO_ROOT / "docs" / "prompts" / "gpt-5.5-5.6-prompt-bank.json"
EXPECTED_MODEL_SCOPE = "[MODEL_SCOPE: gpt-5.5*, gpt-5.6*]\n"
GROUP_INSTRUCTIONS_OPEN = "<group_optional_instructions>"
GROUP_INSTRUCTIONS_CLOSE = "</group_optional_instructions>"
REQUIRED_CASE_FIELDS = {"id", "input", "required_tokens", "forbidden_tokens"}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cases", type=Path, default=DEFAULT_CASES)
    parser.add_argument(
        "--case",
        dest="case_ids",
        action="append",
        default=[],
        help="run only this case id; repeat for multiple cases",
    )
    parser.add_argument("--validate-only", action="store_true")
    parser.add_argument(
        "--inject-prompt",
        action="store_true",
        help="send the canonical prompt in the production group wrapper when server injection is absent",
    )
    parser.add_argument("--base-url", default=os.getenv("SUB2API_BASE_URL", ""))
    parser.add_argument("--api-key", default=os.getenv("SUB2API_API_KEY", ""))
    parser.add_argument("--model", default=os.getenv("SUB2API_MODEL", "gpt-5.6"))
    parser.add_argument(
        "--endpoint",
        choices=("responses", "chat_completions"),
        default="responses",
    )
    parser.add_argument("--timeout", type=float, default=180.0)
    parser.add_argument("--max-output-tokens", type=int, default=1600)
    parser.add_argument(
        "--reasoning-effort",
        choices=("none", "minimal", "low", "medium", "high", "xhigh"),
        default="low",
    )
    parser.add_argument(
        "--delay",
        type=float,
        default=0.0,
        help="seconds to wait between cases",
    )
    parser.add_argument(
        "--show-output",
        action="store_true",
        help="print a compact single-line preview for every response",
    )
    return parser.parse_args()


def load_bank(path: Path) -> dict[str, Any]:
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict) or data.get("version") != 1:
        raise ValueError("prompt bank must be an object with version=1")
    prompt_source = data.get("prompt_source")
    if not isinstance(prompt_source, str) or not prompt_source.strip():
        raise ValueError("prompt_source must be a non-empty repository-relative path")
    prompt_path = (REPO_ROOT / prompt_source).resolve()
    try:
        prompt_path.relative_to(REPO_ROOT.resolve())
    except ValueError as exc:
        raise ValueError("prompt_source escapes the repository") from exc
    prompt = prompt_path.read_text(encoding="utf-8")
    if len(prompt) > 16_384:
        raise ValueError("prompt_source exceeds the 16384-character group limit")
    if not prompt.startswith(EXPECTED_MODEL_SCOPE):
        raise ValueError(
            "prompt_source must begin with the GPT-5.5/GPT-5.6 model scope"
        )

    cases = data.get("cases")
    if not isinstance(cases, list) or not cases:
        raise ValueError("prompt bank cases must be a non-empty array")

    seen: set[str] = set()
    for index, case in enumerate(cases):
        if not isinstance(case, dict):
            raise ValueError(f"case #{index + 1} must be an object")
        missing = REQUIRED_CASE_FIELDS - case.keys()
        if missing:
            raise ValueError(f"case #{index + 1} missing: {sorted(missing)}")
        case_id = case["id"]
        if not isinstance(case_id, str) or not case_id.strip() or case_id in seen:
            raise ValueError(f"case #{index + 1} has an invalid or duplicate id")
        seen.add(case_id)
        if not isinstance(case["input"], str) or not case["input"].strip():
            raise ValueError(f"case {case_id!r} input must be a non-empty string")
        for field in ("required_tokens", "forbidden_tokens"):
            value = case[field]
            if not isinstance(value, list) or not all(
                isinstance(item, str) and item for item in value
            ):
                raise ValueError(f"case {case_id!r} {field} must be a string array")
        expected = case.get("expected_first_line")
        if expected is not None and not isinstance(expected, str):
            raise ValueError(
                f"case {case_id!r} expected_first_line must be a string"
            )
    return data


def endpoint_url(base_url: str, endpoint: str) -> str:
    suffix = "/v1/responses" if endpoint == "responses" else "/v1/chat/completions"
    normalized = base_url.rstrip("/")
    if normalized.endswith(suffix):
        return normalized
    if normalized.endswith("/v1"):
        return normalized + suffix.removeprefix("/v1")
    return normalized + suffix


def canonical_prompt(bank: dict[str, Any]) -> str:
    prompt_path = (REPO_ROOT / bank["prompt_source"]).resolve()
    prompt = prompt_path.read_text(encoding="utf-8")
    if prompt.startswith(EXPECTED_MODEL_SCOPE):
        prompt = prompt[len(EXPECTED_MODEL_SCOPE) :]
    return (
        f"{GROUP_INSTRUCTIONS_OPEN}\n"
        f"{prompt.strip()}\n"
        f"{GROUP_INSTRUCTIONS_CLOSE}"
    )


def request_payload(
    endpoint: str,
    model: str,
    prompt: str,
    instructions: str = "",
    max_output_tokens: int = 0,
    reasoning_effort: str = "",
) -> dict[str, Any]:
    if endpoint == "responses":
        payload: dict[str, Any] = {"model": model, "input": prompt, "stream": False}
        if instructions:
            payload["instructions"] = instructions
        if max_output_tokens > 0:
            payload["max_output_tokens"] = max_output_tokens
        if reasoning_effort:
            payload["reasoning"] = {"effort": reasoning_effort}
        return payload
    messages: list[dict[str, str]] = []
    if instructions:
        messages.append({"role": "system", "content": instructions})
    messages.append({"role": "user", "content": prompt})
    payload = {
        "model": model,
        "messages": messages,
        "stream": False,
    }
    if max_output_tokens > 0:
        payload["max_completion_tokens"] = max_output_tokens
    if reasoning_effort:
        payload["reasoning_effort"] = reasoning_effort
    return payload


def extract_text(data: dict[str, Any], endpoint: str) -> str:
    if endpoint == "chat_completions":
        choices = data.get("choices")
        if not isinstance(choices, list) or not choices:
            raise ValueError("response has no choices")
        content = choices[0].get("message", {}).get("content", "")
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            return "".join(
                block.get("text", "")
                for block in content
                if isinstance(block, dict) and isinstance(block.get("text"), str)
            )
        raise ValueError("chat response content is not text")

    if isinstance(data.get("output_text"), str):
        return data["output_text"]
    chunks: list[str] = []
    for item in data.get("output", []):
        if not isinstance(item, dict):
            continue
        for block in item.get("content", []):
            if not isinstance(block, dict):
                continue
            text = block.get("text")
            if isinstance(text, str):
                chunks.append(text)
    if not chunks:
        raise ValueError("Responses payload has no output text")
    return "".join(chunks)


def invoke(
    base_url: str,
    api_key: str,
    endpoint: str,
    model: str,
    prompt: str,
    instructions: str,
    max_output_tokens: int,
    reasoning_effort: str,
    timeout: float,
) -> str:
    body = json.dumps(
        request_payload(
            endpoint,
            model,
            prompt,
            instructions,
            max_output_tokens,
            reasoning_effort,
        )
    ).encode("utf-8")
    request = urllib.request.Request(
        endpoint_url(base_url, endpoint),
        data=body,
        method="POST",
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            "Accept": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            payload = json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")[:800]
        raise RuntimeError(f"HTTP {exc.code}: {detail}") from exc
    if not isinstance(payload, dict):
        raise ValueError("API response must be a JSON object")
    return extract_text(payload, endpoint)


def evaluate(case: dict[str, Any], output: str) -> list[str]:
    failures: list[str] = []
    normalized = output.lstrip("\ufeff\r\n ")
    first_line = normalized.splitlines()[0] if normalized else ""
    expected = case.get("expected_first_line")
    if expected and not first_line.startswith(expected):
        failures.append(f"first line {first_line!r} does not start with {expected!r}")
    for token in case["required_tokens"]:
        if token not in output:
            failures.append(f"missing token {token!r}")
    for token in case["forbidden_tokens"]:
        if token in output:
            failures.append(f"forbidden token {token!r}")
    return failures


def main() -> int:
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(line_buffering=True)
    args = parse_args()
    try:
        bank = load_bank(args.cases.resolve())
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"bank validation failed: {exc}", file=sys.stderr)
        return 2

    print(f"validated {len(bank['cases'])} cases from {args.cases}")
    if args.validate_only:
        return 0
    if not args.base_url or not args.api_key:
        print(
            "set SUB2API_BASE_URL and SUB2API_API_KEY or pass --base-url/--api-key",
            file=sys.stderr,
        )
        return 2

    selected_cases = bank["cases"]
    if args.case_ids:
        requested_ids = set(args.case_ids)
        selected_cases = [case for case in selected_cases if case["id"] in requested_ids]
        missing_ids = requested_ids - {case["id"] for case in selected_cases}
        if missing_ids:
            print(f"unknown case ids: {sorted(missing_ids)}", file=sys.stderr)
            return 2

    instructions = canonical_prompt(bank) if args.inject_prompt else ""
    print(
        f"running {len(selected_cases)} cases; "
        f"prompt_injection={'explicit' if instructions else 'server-side'}"
    )

    failed = 0
    for index, case in enumerate(selected_cases):
        model = case.get("model", args.model)
        try:
            output = invoke(
                args.base_url,
                args.api_key,
                args.endpoint,
                model,
                case["input"],
                instructions,
                args.max_output_tokens,
                args.reasoning_effort,
                args.timeout,
            )
            failures = evaluate(case, output)
        except (OSError, RuntimeError, ValueError, json.JSONDecodeError) as exc:
            failures = [str(exc)]
            output = ""
        if failures:
            failed += 1
            print(f"FAIL {case['id']}: {'; '.join(failures)}")
            if output:
                print("  " + output.replace("\n", " ")[:300])
        else:
            print(f"PASS {case['id']}")
        if args.show_output and output:
            print("  OUT " + output.replace("\r", " ").replace("\n", " ")[:500])
        if args.delay > 0 and index + 1 < len(selected_cases):
            time.sleep(args.delay)

    print(f"result: {len(selected_cases) - failed} passed, {failed} failed")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
