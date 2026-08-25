#!/usr/bin/env python3
"""
Model Responsiveness Tester for NVIDIA Developer Build API

Tests TTFT (Time To First Token) and TPS (Tokens Per Second) for NVIDIA models.
Set NVIDIA_API_KEY environment variable with your API key from: https://build.nvidia.com/
"""

import time
import json
import os
import math
import requests
import argparse
from typing import List, Dict, Tuple
import concurrent.futures
from threading import Lock

# Global configuration - modify these as needed
API_ENDPOINT = (
    "https://integrate.api.nvidia.com/v1/chat/completions"  # NVIDIA Developer Build API
)
API_KEY = os.getenv("NVIDIA_API_KEY", "")  # NVIDIA API key from environment variable
MODELS = [
    "google/gemma-4-31b-it",
    "google/diffusiongemma-26b-a4b-it",
    "z-ai/glm-5.2",
    "minimaxai/minimax-m3",
    "nvidia/nemotron-3-super-120b-a12b",
    "nvidia/nemotron-3-ultra-550b-a55b",
    "stepfun-ai/step-3.7-flash",
    "thinkingmachines/inkling",
    "meta/muse-glimmer-30b",
    "poolside/laguna-xs-2.1",
    "deepseek-ai/deepseek-v4-flash-0731",
    "nvidia/nemotron-3-nano-30b-a3b",
    "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning",
    "nvidia/nemotron-3.5-lightning-30b-a3b"
]

# Test prompt - keep it short for consistent measurements
TEST_PROMPT = "Write a simple hello world function in Python."
MAX_TOKENS = 100  # Limit response length for testing
TIMEOUT = 20  # Request timeout in seconds


def safe_print(msg: str) -> None:
    """Thread-safe printing with lock to avoid interleaved output."""
    with print_lock:
        print(msg)

# Thread-safe print lock
print_lock = Lock()


def validate_endpoint(api_key: str, endpoint: str, models_to_test: List[str]) -> bool:
    """
    Validate that the endpoint is working and supports the required models.
    """
    headers = {"Authorization": f"Bearer {api_key}"}
    models_url = endpoint.replace("/chat/completions", "/models")

    try:
        with print_lock:
            print("🔍 Validating endpoint and API key...")
        response = requests.get(models_url, headers=headers, timeout=TIMEOUT)
        response.raise_for_status()

        data = response.json()
        available_models = [model["id"] for model in data.get("data", [])]

        with print_lock:
            print(f"✅ Endpoint reachable. Found {len(available_models)} available models.")

        # Check if our test models are available
        missing_models = []
        for model in models_to_test:
            if model not in available_models:
                missing_models.append(model)

        if missing_models:
            with print_lock:
                print(f"⚠️  Warning: {len(missing_models)} requested models not available:")
                for model in missing_models[:5]:  # Show first 5
                    print(f"   - {model}")
                if len(missing_models) > 5:
                    print(f"   ... and {len(missing_models) - 5} more")

        return True

    except requests.exceptions.RequestException as e:
        with print_lock:
            print(f"❌ Endpoint validation failed: {e}")
            if "401" in str(e):
                print("💡 Check your API key - authentication failed")
            elif "403" in str(e):
                print("💡 Check your API permissions - access denied")
            elif "404" in str(e):
                print("💡 Models endpoint not found - API may not support model listing")
        return False
    except Exception as e:
        with print_lock:
            print(f"❌ Unexpected error during validation: {e}")
        return False


def make_streaming_request(
    model: str, prompt: str, api_key: str, endpoint: str
) -> Tuple[float, float, int]:
    """
    Make a streaming request to the API and measure timing.

    Returns:
        ttft (float): Time to first token in seconds
        tps (float): Tokens per second for generation
        total_tokens (int): Total tokens generated
    """
    headers = {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}

    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": MAX_TOKENS,
        "stream": True,
        "temperature": 0.7,
    }

    start_time = time.time()
    ttft = None
    token_count = 0
    last_token_time = start_time

    try:
        with requests.post(
            endpoint, headers=headers, json=payload, stream=True, timeout=TIMEOUT
        ) as response:
            response.raise_for_status()

            for line in response.iter_lines():
                if line:
                    line = line.decode("utf-8")
                    if line.startswith("data: "):
                        data = line[6:]  # Remove 'data: ' prefix

                        if data == "[DONE]":
                            break

                        try:
                            chunk = json.loads(data)
                            
                            # Some APIs (like DeepSeek) include usage info
                            if "usage" in chunk and chunk["usage"]:
                                usage = chunk["usage"]
                                if "completion_tokens" in usage:
                                    token_count = max(token_count, usage["completion_tokens"])

                            if "choices" in chunk and chunk["choices"]:
                                delta = chunk["choices"][0].get("delta", {})
                                # Support both regular content and reasoning models
                                content = delta.get("content", "") or delta.get("reasoning_content", "")

                                if content:  # Actual token content (or reasoning)
                                    current_time = time.time()

                                    if ttft is None:
                                        # First token received
                                        ttft = current_time - start_time
                                    token_count += 1

                                    last_token_time = current_time

                        except json.JSONDecodeError:
                            continue

        # Calculate final metrics
        end_time = time.time()
        total_time = end_time - start_time

        if ttft is None:
            # No tokens received via delta - check if we got any from usage
            if token_count > 0:
                safe_print(f"INFO: {model} got {token_count} tokens from usage (no content delta)")
                ttft = 0.001  # Minimal TTFT if we got tokens from usage
            else:
                return float("inf"), 0.0, 0

        generation_time = total_time - ttft
        tps = token_count / generation_time if generation_time > 0 else 0.0

        return ttft, tps, token_count

    except Exception as e:
        safe_print(f"❌ [{model}] Error: {e}")
        return float("inf"), 0.0, 0


def test_model(model: str, api_key: str, endpoint: str) -> Dict[str, float]:
    """Test a single model and return results."""
    safe_print(f"\n🔍 [{model}] Starting test...")

    ttft, tps, tokens = make_streaming_request(model, TEST_PROMPT, api_key, endpoint)

    # Use math.isinf for reliable infinity check
    if math.isinf(ttft):
        safe_print(f"❌ [{model}] FAILED to respond")
        return {"ttft": float("inf"), "tps": 0.0, "tokens": 0, "model": model}

    # Ensure tps is valid (handle edge case where generation_time is 0)
    if math.isnan(tps) or tps < 0:
        tps = 0.0

    safe_print(f"   [{model}] TTFT: {ttft * 1000:.1f} ms")
    safe_print(f"   [{model}] TPS: {tps:.1f} tokens/sec")
    return {"ttft": ttft, "tps": tps, "tokens": tokens, "model": model}


def main():
    # Declare globals that will be modified
    global API_ENDPOINT, API_KEY, TEST_PROMPT, MAX_TOKENS, TIMEOUT

    parser = argparse.ArgumentParser(
        description="Test model responsiveness for NVIDIA Developer Build API"
    )
    parser.add_argument(
        "--endpoint",
        default=API_ENDPOINT,
        help=f"API endpoint (default: {API_ENDPOINT})",
    )
    parser.add_argument(
        "--api-key",
        default=API_KEY,
        help=f"API key (overrides NVIDIA_API_KEY env var, default: {API_KEY if API_KEY else '(from env)'})",
    )
    parser.add_argument(
        "--models",
        nargs="+",
        default=MODELS,
        help=f"Models to test (default: {', '.join(MODELS)})",
    )
    parser.add_argument("--model", help="Single model to test (overrides --models)")
    parser.add_argument(
        "--prompt",
        default=TEST_PROMPT,
        help=f"Test prompt (default: \"{TEST_PROMPT}\")",
    )
    parser.add_argument(
        "--max-tokens",
        type=int,
        default=MAX_TOKENS,
        help=f"Max tokens to generate (default: {MAX_TOKENS})",
    )
    parser.add_argument(
        "--timeout",
        type=int,
        default=TIMEOUT,
        help=f"Request timeout in seconds (default: {TIMEOUT})",
    )
    parser.add_argument(
        "--concurrency",
        type=int,
        default=4,
        help="Number of concurrent model tests (default: 4)",
    )

    args = parser.parse_args()

    # Determine models to test
    models_to_test = [args.model] if args.model else args.models

    # Update globals from args
    API_ENDPOINT = args.endpoint
    API_KEY = args.api_key or API_KEY  # Use arg if provided, otherwise env var
    TEST_PROMPT = args.prompt
    MAX_TOKENS = args.max_tokens
    TIMEOUT = args.timeout

    # Validate API key is set
    if not API_KEY:
        safe_print("❌ NVIDIA_API_KEY environment variable not set!")
        safe_print("💡 Get your API key from: https://build.nvidia.com/")
        safe_print("💡 Set it with: export NVIDIA_API_KEY=your-api-key-here")
        exit(1)

    safe_print("🚀 NVIDIA Model Responsiveness Tester")
    safe_print(f"📍 Endpoint: {API_ENDPOINT}")
    safe_print(f"📝 Prompt: {TEST_PROMPT}")
    safe_print(f"🎯 Max tokens: {MAX_TOKENS}")
    safe_print(f"⏱️  Timeout: {TIMEOUT}s")
    safe_print(f"🤖 Models to test: {', '.join(models_to_test)}")
    safe_print(f"🔑 API Key: {'*' * 8 + API_KEY[-4:] if len(API_KEY) > 12 else API_KEY}")
    safe_print(f"🔄 Concurrency: {args.concurrency}")

    # Validate endpoint before starting tests
    if not validate_endpoint(API_KEY, API_ENDPOINT, models_to_test):
        print("💥 Cannot proceed with tests due to endpoint/API issues.")
        exit(1)

    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as executor:
        future_to_model = {
            executor.submit(test_model, model, API_KEY, API_ENDPOINT): model
            for model in models_to_test
        }
        for future in concurrent.futures.as_completed(future_to_model):
            model = future_to_model[future]
            try:
                result = future.result()
                # test_model already adds model key, but ensure it's there
                if "model" not in result:
                    result["model"] = model
                results.append(result)
            except Exception as exc:
                safe_print(f"❌ [{model}] Exception: {exc}")
                results.append({"model": model, "ttft": float("inf"), "tps": 0.0, "tokens": 0})

    # Sort results by TTFT (ascending, failed models last)
    results.sort(key=lambda x: x["ttft"] if not math.isinf(x["ttft"]) else float("inf"))

    # Summary table
    safe_print(f"\n{'=' * 80}")
    safe_print("📊 SUMMARY RESULTS")
    safe_print(f"{'=' * 80}")
    safe_print(f"{'Model':<45} {'TTFT (ms)':>12} {'TPS':>10} {'Tokens':>8}")
    safe_print("-" * 80)

    for result in results:
        model = result["model"]
        ttft = result["ttft"]
        tps = result["tps"]
        tokens = result["tokens"]

        if math.isinf(ttft):
            safe_print(f"{model:<45} {'FAILED':>12} {'-':>10} {'-':>8}")
        else:
            safe_print(f"{model:<45} {ttft * 1000:>12.1f} {tps:>10.1f} {tokens:>8}")

    safe_print(f"{'=' * 80}")


if __name__ == "__main__":
    main()
