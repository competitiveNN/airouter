package main

import "encoding/json"

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
//   - client -> upstream: a Responses request is normalized to the Chat
//     Completions shape the upstream provider understands. Responses-only
//     fields (instructions, previous_response_id, text.format, reasoning)
//     are not representable upstream and are dropped by the translation.
//   - upstream -> client: the Chat Completions-shaped response/stream is
//     re-shaped into the Responses envelope the client expects.
//
// The request-side translation lives in responses_translate.go, the SSE event
// mapping in responses_stream.go, and the response-side shaping in
// responses_out.go. The WebSocket transport lives in responses_ws.go.

// responsesRequest is the client-facing /v1/responses request body.
//
// Only the fields we can meaningfully honor are modelled. Responses-only
// controls that the upstream Chat Completions surface cannot express
// (previous_response_id, text.format, reasoning, store, metadata) are parsed
// and ignored rather than rejected, so a spec-complete client can talk to the
// gateway without being told its request is malformed.
type responsesRequest struct {
	// Model is the logical model name (smart/work/fast/large). This is the
	// field that drives routing, exactly as in Chat Completions.
	Model string `json:"model"`

	// Input is either a bare string (treated as one user message) or an array
	// of typed items. Parsed by parseInputItems.
	Input json.RawMessage `json:"input"`

	// Instructions becomes a leading system message upstream.
	Instructions string `json:"instructions"`

	// Sampling controls map 1:1 onto the Chat Completions request.
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	MaxOutputTokens  *int     `json:"max_output_tokens,omitempty"`

	// Stream selects the SSE event surface instead of a single JSON envelope.
	Stream bool `json:"stream,omitempty"`

	// Accepted and ignored: not representable upstream. Kept as fields so
	// decoding a spec-complete client request does not error, and so the
	// gateway can tell "client sent reasoning" apart from "client omitted it"
	// if a future version needs to gate behavior on it.
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
	Text               json.RawMessage `json:"text,omitempty"`
}

// item is one entry of the Responses `input` / `output` array. The Responses
// spec uses a discriminated union on `type`; Go has no sum types, so one
// struct carries the union and callers switch on Type.
//
// Recognized types:
//   - "message"             role + content parts
//   - "function_call"       an assistant-issued tool call
//   - "function_call_output" the result of a tool call
//   - "reasoning"           dropped upstream (not representable)
type item struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`

	// message fields
	Role    string        `json:"role,omitempty"`
	Content []contentPart `json:"content,omitempty"`

	// function_call fields
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call_output fields
	Output string `json:"output,omitempty"`
}

// contentPart is one element of a message's content array. The Responses
// spec distinguishes input_text / output_text (plain text) from input_image
// (vision). We keep the type discriminator so vision requests are detectable
// upstream, and keep the text payload for token estimation.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// input_image carries the image reference. The spec allows either a bare
	// string or an object of the form {"url": "..."}; both are accepted and
	// normalized to a string. The value is never forwarded verbatim as text
	// (that would massively over-count tokens); it exists so image content can
	// be detected and so the Chat Completions translation can rebuild an
	// image_url part.
	ImageURL imageRef `json:"image_url,omitempty"`
}

// imageRef normalizes the two accepted encodings of an image reference into a
// single string, so downstream code has one representation to handle.
type imageRef string

// UnmarshalJSON accepts `"https://..."` or `{"url": "https://..."}`.
func (r *imageRef) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*r = imageRef(s)
		return nil
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*r = imageRef(obj.URL)
	return nil
}

// responseEnvelope is the client-facing /v1/responses response body.
type responseEnvelope struct {
	ID        string     `json:"id"`
	Object    string     `json:"object"`
	CreatedAt int64      `json:"created_at"`
	Status    string     `json:"status"`
	Model     string     `json:"model"`
	Output    []item     `json:"output"`
	Usage     *respUsage `json:"usage,omitempty"`
	Error     *respError `json:"error,omitempty"`
}

// respUsage is the Responses-shaped token accounting block.
type respUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// respError is the Responses-shaped error block.
type respError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type"`
}
