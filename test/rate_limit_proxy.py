"""
mitmproxy script to simulate GitHub API errors (503, 429, etc).

Usage:
    mitmproxy -s rate_limit_proxy.py -p 8080

Then run the connector with:
    HTTPS_PROXY=http://localhost:8080 ./dist/darwin_arm64/baton-github ...

Configuration options (set as environment variables):
    ERROR_AFTER=10           # Return errors after N requests (default: 10)
    ERROR_MODE=block         # "block" = always error, "intermittent" = error every N requests
    ERROR_TYPE=503           # "503", "502", "504", "429", "403" (default: 503)
    INTERMITTENT_FREQ=3      # For intermittent mode: error every N requests (default: 3)

    # Escalation mode: start with one error, switch to another after N more requests
    ESCALATE_TO=503          # Switch to this error type after ESCALATE_AFTER requests
    ESCALATE_AFTER=5         # Number of errors before escalating (default: 5)

    # Timing
    RETRY_AFTER=60           # Seconds for Retry-After header and X-RateLimit-Reset (default: 60)

    # Recovery
    RECOVER_AFTER=0          # Seconds until errors stop and requests succeed again (0 = never recover)

    # Domain filtering
    TARGET_DOMAINS=api.github.com  # Comma-separated list of domains to apply errors to (default: api.github.com)

Example - 403 rate limits that escalate to 503 after 5 errors, with 5s retry:
    ERROR_AFTER=10 ERROR_TYPE=403 ESCALATE_TO=503 ESCALATE_AFTER=5 RETRY_AFTER=5 mitmdump -s rate_limit_proxy.py -p 8080

Example - 503 errors that recover after 30 seconds:
    ERROR_AFTER=10 ERROR_TYPE=503 RECOVER_AFTER=30 RETRY_AFTER=10 mitmdump -s rate_limit_proxy.py -p 8080
"""

import os
import time
from mitmproxy import http

# Configuration
ERROR_AFTER = int(os.environ.get("ERROR_AFTER", "10"))
ERROR_MODE = os.environ.get("ERROR_MODE", "block")  # "block" or "intermittent"
ERROR_TYPE = os.environ.get("ERROR_TYPE", "503")  # "503", "502", "504", "429", "403"
INTERMITTENT_FREQ = int(os.environ.get("INTERMITTENT_FREQ", "3"))

# Escalation config
ESCALATE_TO = os.environ.get("ESCALATE_TO", "")  # Empty = no escalation
ESCALATE_AFTER = int(os.environ.get("ESCALATE_AFTER", "5"))

# Retry delay (seconds)
RETRY_AFTER = int(os.environ.get("RETRY_AFTER", "60"))

# Recovery (seconds, 0 = never recover)
RECOVER_AFTER = int(os.environ.get("RECOVER_AFTER", "0"))

# Domain filter (comma-separated, default: api.github.com)
TARGET_DOMAINS = os.environ.get("TARGET_DOMAINS", "api.github.com").split(",")

# State
request_count = 0
error_count = 0
error_started = False
error_start_time = 0
escalated = False
recovered = False
reset_time = 0

# Error responses
ERROR_RESPONSES = {
    "503": (503, b'{"message":"Service temporarily unavailable"}', "Service Unavailable"),
    "502": (502, b'{"message":"Bad gateway"}', "Bad Gateway"),
    "504": (504, b'{"message":"Gateway timeout"}', "Gateway Timeout"),
    "429": (429, b'{"message":"Too many requests"}', "Too Many Requests"),
    "403": (403, b'{"message":"API rate limit exceeded","documentation_url":"https://docs.github.com/rest/overview/resources-in-the-rest-api#rate-limiting"}', "Rate Limited"),
}


def request(flow: http.HTTPFlow) -> None:
    global request_count, error_count, error_started, error_start_time, escalated, recovered, reset_time

    # Only intercept requests to target domains
    if not any(domain in flow.request.host for domain in TARGET_DOMAINS):
        return

    request_count += 1
    print(f"[{request_count}] {flow.request.method} {flow.request.path}")

    # Check for recovery
    if error_started and RECOVER_AFTER > 0 and not recovered:
        elapsed = time.time() - error_start_time
        if elapsed >= RECOVER_AFTER:
            recovered = True
            print(f">>> RECOVERED after {int(elapsed)}s! Requests will succeed now.")

    # Check if we should return an error
    should_error = False

    if request_count > ERROR_AFTER and not recovered:
        if ERROR_MODE == "block":
            should_error = True
            if not error_started:
                error_started = True
                error_start_time = time.time()
                reset_time = int(time.time()) + RETRY_AFTER
                print(f">>> ERROR MODE STARTED ({ERROR_TYPE})! Retry-After: {RETRY_AFTER}s")
                if RECOVER_AFTER > 0:
                    print(f">>> Will recover after {RECOVER_AFTER}s")
        elif ERROR_MODE == "intermittent":
            # Error every INTERMITTENT_FREQ requests after ERROR_AFTER
            requests_since_start = request_count - ERROR_AFTER
            if requests_since_start % INTERMITTENT_FREQ == 0:
                should_error = True
                print(f">>> INTERMITTENT ERROR ({ERROR_TYPE})")

    if should_error:
        error_count += 1

        # Determine which error type to use (check for escalation)
        current_error_type = ERROR_TYPE
        if ESCALATE_TO and error_count > ESCALATE_AFTER:
            if not escalated:
                escalated = True
                print(f">>> ESCALATING from {ERROR_TYPE} to {ESCALATE_TO} after {ESCALATE_AFTER} errors!")
            current_error_type = ESCALATE_TO

        status_code, body, description = ERROR_RESPONSES.get(
            current_error_type, ERROR_RESPONSES["503"]
        )

        headers = {"Content-Type": "application/json"}

        # Add rate limit headers for 403/429
        if current_error_type in ("403", "429"):
            headers.update({
                "X-RateLimit-Limit": "5000",
                "X-RateLimit-Remaining": "0",
                "X-RateLimit-Reset": str(reset_time),
                "X-RateLimit-Used": "5000",
                "X-RateLimit-Resource": "core",
            })

        # Add Retry-After header for 503/429
        if current_error_type in ("503", "429"):
            headers["Retry-After"] = str(RETRY_AFTER)

        flow.response = http.Response.make(status_code, body, headers)
        print(f">>> Returned {status_code} {description} (error #{error_count})")


def response(flow: http.HTTPFlow) -> None:
    """Log rate limit headers from real responses."""
    if not any(domain in flow.request.host for domain in TARGET_DOMAINS):
        return

    remaining = flow.response.headers.get("X-RateLimit-Remaining", "?")
    limit = flow.response.headers.get("X-RateLimit-Limit", "?")
    print(f"    <- {flow.response.status_code} (Rate: {remaining}/{limit})")
