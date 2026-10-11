# Configuration

Janus reads `janus.config.json` from the directory it is run in, or the
file named by `-config`. If the file does not exist, Janus writes one with
the defaults below and carries on.

```json
{
  "listen": "127.0.0.1:8787",
  "allowRemote": false,
  "upstream": { "baseUrl": "https://api.anthropic.com" },
  "local": { "baseUrl": "http://studio.tuesday.local:1234" },
  "features": { "route": false, "escalate": false, "compress": false, "guard": false },
  "logging": { "dir": "logs" }
}
```

| Field | Meaning |
| --- | --- |
| `listen` | Address Janus accepts connections on. Must be loopback unless `allowRemote` is true. |
| `allowRemote` | Permit a non-loopback `listen`. Janus forwards whatever credentials clients send, so leave this off unless you mean it. |
| `upstream.baseUrl` | The Anthropic API, or a gateway in front of it. |
| `local.baseUrl` | The LM Studio server. Not used yet. |
| `features.*` | One switch per mechanism. None is implemented yet, and Janus refuses to start if any is true. |
| `logging.dir` | Where logs will be written. Not used yet. |

A field left out of the file keeps its default. A field Janus does not
recognise is an error, so typos are caught.

## Overrides

Precedence, highest first: flag, environment variable, config file, default.

| Setting | Flag | Environment |
| --- | --- | --- |
| `listen` | `-listen` | `JANUS_LISTEN` |
| `upstream.baseUrl` | `-upstream` | `JANUS_UPSTREAM_URL` |
| `local.baseUrl` | `-local` | `JANUS_LOCAL_URL` |

Overrides apply to the running process only; they are not written back to
the file.
