# WeCom adapter connection settings

See the [pack README](../README.md) for setup and the remaining environment
settings. Set these optional values in the adapter's environment file.

| Variable | Default | Behavior |
| --- | --- | --- |
| `WECOM_WS_URL` | `wss://openws.work.weixin.qq.com` | Override the WebSocket endpoint for private deployments. |
| `WECOM_WS_CONNECT_ADDRESS` | Unset | Pin `openws.work.weixin.qq.com` to an IPv4 address, for example `120.233.240.159`, through the SDK's WebSocket lookup option. Other hostnames use normal DNS. The URL, TLS SNI and certificate hostname verification stay unchanged. Unset or empty preserves the usual behavior; an invalid value logs one startup warning and is ignored. Each pinned connection attempt logs `ws connect address pinned to <ip>`. This changes only WebSocket address resolution; routing and the uploader are unchanged. |

To roll back, remove/unset `WECOM_WS_CONNECT_ADDRESS` in the adapter's
environment file, then run **one** `gc service restart wecom` so the next
connection uses normal DNS. Unsetting it only in an interactive shell does
not remove a value still set in the service environment file.

Run the suite from this directory with `node --test 'test/*.test.js'`.
The connection tests use local TLS WebSocket servers and temporary test
certificates generated with `openssl`; no bot credentials are needed.
