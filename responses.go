package main

// Responses API translation layer.
//
// Open Responses (https://www.openresponses.org) is an open, vendor-neutral
// specification built on the OpenAI Responses API (/v1/responses). It models a
// generation as a sequence of typed *items* rather than a flat messages array.
//
// Airouter's client surface is now two APIs:
//   - OpenAI Chat Completions (/v1/chat/completions) -- the original surface.
//   - Open Responses / OpenAI Responses (/v1/responses) -- the new surface.
//
// Both surfaces route through the same router/proxy/fallback machinery; the
// only difference is the request and response shape. This file owns the
// request/response types for the Responses surface and the translation to
// and from the Chat Completions shape the upstream providers understand.
//
// Translation directions:
//   client -> upstream:  a Responses request is normalized to the Chat
//     Completions shape the upstream provider understands, while preserving
//     Responses-only fields (instructions, previous_response_id, text.format,
//     reasoning) as metadata the gateway can act on.
//   upstream -> client:  the Chat Completions-shaped response/stream is
//     re-shaped into the Responses envelope the client expects.
//
// The request-side translation and SSE event mapping live in
// responses_translate.go and responses_stream.go. The response-side shaping
// lives in responses_out.go. The WebSocket transport lives in
// responses_ws.go. The compaction endpoint lives in responses_compact.go.
