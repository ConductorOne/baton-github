# Rate Limit Testing with mitmproxy

This guide explains how to use `rate_limit_proxy.py` to simulate GitHub API errors (503, 429, 403, etc.) for testing the connector's error handling.

## Prerequisites

Install mitmproxy:
```bash
brew install mitmproxy
```

## One-Time Setup: Trust mitmproxy's CA Certificate

The proxy intercepts HTTPS traffic, so you need to trust its CA certificate:

```bash
# Generate the certificate (run mitmproxy once)
mitmdump -p 8080
# Press Ctrl+C after it starts

# Add to macOS system keychain
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.mitmproxy/mitmproxy-ca-cert.pem
```

To remove when done testing:
```bash
sudo security delete-certificate -c mitmproxy /Library/Keychains/System.keychain
```

## Usage

### Terminal 1: Start the Proxy

```bash
cd /path/to/baton-github

# Basic: 503 errors after 10 requests
ERROR_AFTER=10 ERROR_TYPE=503 mitmdump -s test/rate_limit_proxy.py -p 8080

# 403 rate limits that escalate to 503 after 5 errors
ERROR_AFTER=10 ERROR_TYPE=403 ESCALATE_TO=503 ESCALATE_AFTER=5 RETRY_AFTER=5 \
  mitmdump -s test/rate_limit_proxy.py -p 8080
```

### Terminal 2: Run the Connector

```bash
HTTPS_PROXY=http://localhost:8080 \
./dist/darwin_arm64/baton-github \
  --app-id=YOUR_APP_ID \
  --app-privatekey-path=/path/to/private-key.pem \
  --orgs=your-org \
  --log-level=debug
```

## Configuration Options

| Variable | Default | Description |
|----------|---------|-------------|
| `ERROR_AFTER` | `10` | Return errors after N successful requests |
| `ERROR_TYPE` | `503` | Error type: `503`, `502`, `504`, `429`, `403` |
| `ERROR_MODE` | `block` | `block` = all errors, `intermittent` = periodic errors |
| `INTERMITTENT_FREQ` | `3` | For intermittent mode: error every N requests |
| `ESCALATE_TO` | _(none)_ | Switch to this error type after `ESCALATE_AFTER` errors |
| `ESCALATE_AFTER` | `5` | Number of errors before escalating |
| `RETRY_AFTER` | `60` | Seconds for `Retry-After` header and `X-RateLimit-Reset` |
| `RECOVER_AFTER` | `0` | Seconds until errors stop and requests succeed (0 = never recover) |

## Examples

### Simulate 503 Service Unavailable
```bash
ERROR_AFTER=5 ERROR_TYPE=503 RETRY_AFTER=10 mitmdump -s test/rate_limit_proxy.py -p 8080
```

### Simulate Rate Limiting (403 with X-RateLimit-Remaining: 0)
```bash
ERROR_AFTER=10 ERROR_TYPE=403 RETRY_AFTER=30 mitmdump -s test/rate_limit_proxy.py -p 8080
```

### Simulate Intermittent 502 Bad Gateway (every 3rd request)
```bash
ERROR_AFTER=5 ERROR_MODE=intermittent ERROR_TYPE=502 INTERMITTENT_FREQ=3 \
  mitmdump -s test/rate_limit_proxy.py -p 8080
```

### Simulate Escalating Errors (403 -> 503)
This simulates a scenario where rate limits escalate to service unavailability:
```bash
ERROR_AFTER=10 ERROR_TYPE=403 ESCALATE_TO=503 ESCALATE_AFTER=5 RETRY_AFTER=5 \
  mitmdump -s test/rate_limit_proxy.py -p 8080
```

### Simulate 503 with Recovery
This tests the SDK's retry logic - errors for 30 seconds, then requests succeed:
```bash
ERROR_AFTER=10 ERROR_TYPE=503 RECOVER_AFTER=30 RETRY_AFTER=10 \
  mitmdump -s test/rate_limit_proxy.py -p 8080
```

Output will show:
```
[1] GET /orgs/example
    <- 200 (Rate: 4999/5000)
...
[11] GET /orgs/example/members
>>> ERROR MODE STARTED (403)! Retry-After: 5s
>>> Returned 403 Rate Limited (error #1)
...
[16] GET /orgs/example/teams
>>> ESCALATING from 403 to 503 after 5 errors!
>>> Returned 503 Service Unavailable (error #6)
```

## Proxy Output

The proxy logs each request and its outcome:
- `[N]` - Request number
- `<- 200 (Rate: X/Y)` - Successful response with rate limit info
- `>>> ERROR MODE STARTED` - First error triggered
- `>>> ESCALATING` - Error type changed
- `>>> Returned NNN` - Error response sent

## Troubleshooting

### Certificate errors
If you see `x509: certificate is not trusted`:
1. Make sure you ran the one-time setup to trust the CA
2. Verify the cert is installed: `security find-certificate -c mitmproxy /Library/Keychains/System.keychain`

### Proxy not intercepting
- Ensure `HTTPS_PROXY` is set correctly
- Check mitmproxy is running on the correct port
- Verify the connector is making requests to `api.github.com`
