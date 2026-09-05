# Grok Tool Integer Args Plugin

[中文](README.zh-CN.md)

CLIProxyAPI (CPA) native plugin that rewrites whole-number JSON floats inside tool-call `arguments` (for example `23000.0` -> `23000`) so Codex clients that reject floating-point tool parameters keep working with Grok/xAI models.

## What It Does

- Hooks non-streaming responses via `response.intercept_after`
- Hooks streaming chunks via `response.intercept_stream_chunk`
- Finds tool-call `arguments` fields (string JSON or object) and converts whole-number floats to JSON integers
- Leaves true decimals untouched (`1.5` stays `1.5`)
- Fail-open: partial/invalid JSON is left unchanged
- Keeps streaming: each chunk is rewritten independently; the stream is never buffered into a non-stream response
- Skips incomplete `response.function_call_arguments.delta` events
- Repairs missing Responses stream `sequence_number` fields using delivered history and a per-request fallback when that history is evicted; existing values are preserved
- Can disable sequence repair with `repair_sequence_numbers: false`

## Requirements

- CPA host **v7.2.129 or newer** (RPC schema 3)

## Install

Download a release asset for your platform, extract the dynamic library, and place it under CPA's plugin directory. The library basename must be `grok-tool-int-args` so CPA maps it to `plugins.configs.grok-tool-int-args`.

Release archives contain the expected platform filename:

- `grok-tool-int-args.dll` on Windows
- `grok-tool-int-args.so` on Linux
- `grok-tool-int-args.dylib` on macOS

Enable dynamic plugins and point `plugins.dir` at the directory containing the library:

```yaml
plugins:
  enabled: true
  dir: "/absolute/path/to/plugins"
  configs:
    grok-tool-int-args:
      enabled: true
      priority: 20
      models: ["grok", "xai"]
      chat_completions: true
      responses: true
      include_custom_input: false
      repair_sequence_numbers: true
```

## Configuration

### CPA Plugin Store

This repository includes a CPA plugin registry at:

`https://raw.githubusercontent.com/yueziji/grok-tool-int-args-plugin/main/registry.json`

Add that URL as a custom plugin source in CPA. CPA will use the GitHub Releases of this repository to check for and install updates. Release assets use the exact plugin-store naming format `grok-tool-int-args_<version>_<goos>_<goarch>.zip`; CPA verifies them with the accompanying `checksums.txt`.

Supported platforms: `windows/amd64`, `linux/amd64`, `linux/arm64`, and `darwin/arm64`.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `models` | array | `["grok", "xai"]` | Case-insensitive rules matched against `Model` / `RequestedModel` at the start of the name or right after a separator (`-`, `_`, `.`, `/`, `:`, `@`). `xai` matches `xai-beta` and `openrouter/x-ai/grok-4` but not `pixai-diffusion`. An explicit empty list (`models: []`) matches all models; a null value (`models:` with nothing after it) keeps the default. |
| `chat_completions` | bool | `true` | Rewrite Chat Completions tool arguments. |
| `responses` | bool | `true` | Rewrite Responses / `openai-response` tool arguments. |
| `include_custom_input` | bool | `false` | Also rewrite JSON in custom-tool `input`, including `response.custom_tool_call_input.done` events. Plain text and incomplete JSON are preserved. |
| `repair_sequence_numbers` | bool | `true` | Fill missing `sequence_number` on Responses stream events from delivered history, with a per-request fallback when history is evicted. |

`plugins.configs.<id>.enabled` is owned by CPA and controls whether the plugin is active.

## Streaming Behavior

This plugin does **not** convert streaming into non-streaming.

- Stream chunks are processed one-by-one and immediately returned to CPA
- Standard SSE `data:` frames and bare JSON websocket chunks are both supported
- Only complete argument payloads are rewritten (`function_call_arguments.done`, `output_item.done`, completed outputs, full chat tool_calls, etc.)
- Incomplete argument deltas are skipped on purpose
- Responses events missing `sequence_number` use the host-provided delivered history first. A fallback keyed by the host's `RequestID` continues numbering if an event exceeds the host's 1 MiB history limit or the recent window contains no sequence number. Existing sequence numbers are preserved
- Fragmented Chat Completions arguments are isolated by request, response, choice, and tool; other chunk content continues downstream, and the complete arguments are emitted once valid JSON closes
- If one tool's arguments exceed 1 MiB, the buffered bytes are flushed unchanged and all later fragments for that tool pass through until the choice finishes
- A finishing chunk flushes any incomplete arguments that are still withheld
- Active argument buffers do not expire on an idle timer. The host's `request.complete` notification releases buffers and sequence fallback state on success, failure, rejection, or cancellation; plugin shutdown also clears all state
- If one JSON event is split across two upstream chunks, neither chunk is valid JSON at interception time, so its `sequence_number` cannot be repaired; this is an inherent limitation of chunk-level interception

## Build Locally

Requires Go 1.26+ with CGO enabled.

```bash
# Windows
go build -buildmode=c-shared -o grok-tool-int-args.dll .

# Linux
go build -buildmode=c-shared -o grok-tool-int-args.so .

# macOS
go build -buildmode=c-shared -o grok-tool-int-args.dylib .
```

Run tests (pure-logic tests also work with CGO disabled):

```bash
go test .
CGO_ENABLED=0 go test .
```

## Example Rewrite

Before:

```json
{
  "type": "function_call",
  "name": "shell",
  "arguments": "{\"timeout_ms\":23000.0,\"command\":\"ls\"}"
}
```

After:

```json
{
  "type": "function_call",
  "name": "shell",
  "arguments": "{\"timeout_ms\":23000,\"command\":\"ls\"}"
}
```

## Compatibility

- Built against `github.com/router-for-me/CLIProxyAPI/v7`
- Uses CPA plugin ABI v1 / RPC schema v3
- Requires CPA hosts v7.2.129+ that load native plugins from `plugins.dir`

## License

MIT
