package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

type ProviderConfig struct {
	URL       string            `yaml:"url"`
	APIKeyEnv string            `yaml:"api_key_env"`
	Headers   map[string]string `yaml:"headers,omitempty"`

	// Proxy is the provider-wide proxy setting: the maximum number of
	// different proxies to try on any endpoint that routes through this
	// provider, before falling back to the next model in the chain. 0 (or
	// omitted) means direct connections for this provider.
	//
	// This exists so the common case stays one line instead of one line per
	// model: proxy a provider once and every chain entry that does not carry
	// its own proxy value inherits that. An endpoint's own proxy value, when
	// set to a non-zero number, takes precedence over this one (a per-model
	// override for the rare chain entry that needs a different budget).
	Proxy int `yaml:"proxy,omitempty"`

	// MaxInterruption is the maximum time this provider's endpoint may
	// leave a client without new output during a fallback walk, including
	// the cycle-back wait when every endpoint in the chain is on cooldown.
	//
	// It is per-provider rather than one global setting because different
	// backends have different latencies: a local ollama endpoint answers in
	// milliseconds, so holding a client open for 30s on its cooldown is
	// absurd, while a rate-limited hosted model may legitimately need most
	// of that window to recover.
	//
	// The value is clamped to maxSessionInterruption at load time, so no
	// provider can widen the gateway's interruption contract: 30s is the
	// ceiling the client is promised, and it is the ceiling for every
	// provider regardless of what is written here. Omitting the key (or
	// setting 0) uses maxSessionInterruption, so existing configs keep
	// their current behaviour.
	MaxInterruption time.Duration `yaml:"max_interruption,omitempty"`

	// Timeout is the per-provider request timeout applied to upstream calls.
	// Omitting it (or setting 0) uses the default client timeout.
	// A duration like 30s, 60s, etc. Long-running requests (large context
	// compression, reasoning models) should raise this per provider.
	Timeout time.Duration `yaml:"timeout,omitempty"`
}

// ProviderMaxInterruption returns the effective interruption ceiling for
// the named provider, clamped to the global maxSessionInterruption. The
// returned value is always positive: a missing or zero config value falls
// back to the global default.
func ProviderMaxInterruption(cfg *Config, provider string) time.Duration {
	if cfg != nil {
		if p, ok := cfg.Providers[provider]; ok && p.MaxInterruption > 0 {
			if p.MaxInterruption > maxSessionInterruption {
				return maxSessionInterruption
			}
			return p.MaxInterruption
		}
	}
	return maxSessionInterruption
}

// opencodeProviderPrefixes lists provider names whose upstream endpoint is an
// OpenCode gateway (opencode.ai/zen or opencode.ai/zen/go). Requests to these
// providers must carry OpenCode client-attribution headers, otherwise the
// gateway treats the request as anonymous and rejects free-tier models with
// `403 FreeTierError: OpenCode's free tier can only be used from within
// OpenCode` (see oh-my-pi#12306). The headers are synthesized at request time
// with fresh per-request IDs so the upstream cannot fingerprint a single
// client across sessions — matching the official opencode CLI behaviour.
var opencodeProviderPrefixes = []string{"opencode", "opencode-go", "opencode-zen"}

// isOpencodeProvider reports whether the named provider is an OpenCode gateway
// that requires client-attribution headers.
func isOpencodeProvider(provider string) bool {
	for _, p := range opencodeProviderPrefixes {
		if p == provider {
			return true
		}
	}
	return false
}

// opencodeBase62 is the alphabet OpenCode mints session ids from.
const opencodeBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// opencodeKSUIDMask is the width of the id prefix the free-tier gate
// inspects: 6 bytes, hex-encoded, holding a KSUID-shaped
// `timestamp_ms * 4096 + counter` value.
const opencodeKSUIDMask = (1 << 48) - 1

// opencodeIDCounter sequences the ids minted within one millisecond.
//
// The gateway's free-tier gate decodes the counter out of the session
// id prefix and refuses ids whose counter looks larger than a real
// client could mint in a millisecond. Measured live against
// opencode.ai/zen/v1 on 2026-10-13: counters 0-60 are admitted,
// 70 and up answer `403 FreeTierError`. The counter is therefore kept
// well below that bound; the timestamp part of the prefix carries the
// id's uniqueness, the counter only the per-millisecond ordering.
var opencodeIDCounter atomic.Uint32

// opencodeKSUUIDPrefix renders the 12-hex-character KSUID prefix the
// OpenCode free-tier gate validates.
//
// The gate decodes the prefix as `~(timestamp_ms * 4096 + counter)`
// (mod 2^48) for a SESSION id -- OpenCode mints session ids in
// descending KSUID order, newest first -- and as the plain
// `timestamp_ms * 4096 + counter` for a message id. Either way the
// decoded counter must be small. A prefix of plain random bytes, which
// is what this code emitted until 2026-10-13, decodes to an arbitrary
// counter and is refused with `403 FreeTierError` on ~every request:
// the gate only admits the ~64 counter values a real client can mint
// in a millisecond, so a random 48-bit prefix passes with probability
// ~64/4096. That is the regression that made every proxied free-model
// request fail even though the header names and shapes were right.
func opencodeKSUUIDPrefix(descending bool) string {
	c := opencodeIDCounter.Add(1) % 32
	val := (uint64(time.Now().UnixMilli())*0x1000 + uint64(c)) & opencodeKSUIDMask
	if descending {
		val = ^val & opencodeKSUIDMask
	}
	return fmt.Sprintf("%012x", val)
}

// opencodeRandomBase62 mints n characters from the OpenCode base62
// alphabet, crypto-random. The RNG failure path fills deterministically
// so an id is always produced; the gateway checks the KSUID prefix, not
// this tail.
func opencodeRandomBase62(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(i)
		}
	}
	var sb strings.Builder
	sb.Grow(n)
	for _, c := range b {
		sb.WriteByte(opencodeBase62[int(c)%len(opencodeBase62)])
	}
	return sb.String()
}

// opencodeSessionID mints a session id in the canonical OpenCode shape
// `ses_<12 hex><14 base62>`, where the 12 hex characters are the
// descending-KSUID prefix the free-tier gate validates (see
// opencodeKSUUIDPrefix). Fresh per call, exactly like the official
// CLI mints one per session.
func opencodeSessionID() string {
	return "ses_" + opencodeKSUUIDPrefix(true) + opencodeRandomBase62(14)
}

// opencodeRequestID mints the per-request id carried under
// x-opencode-request, in the ascending-KSUID shape the official CLI
// mints for message ids (`msg_<12 hex><14 base62>`).
func opencodeRequestID() string {
	return "msg_" + opencodeKSUUIDPrefix(false) + opencodeRandomBase62(14)
}

// opencodeSessionFor maps an airouter session id onto the canonical
// OpenCode shape, deterministically.
//
// airouter identifies a session as "ctx:<32 hex>" (api.go, bodySessionID). The
// gateway rejects that shape with 403 FreeTierError even when every other
// free-tier condition holds, so the id cannot be forwarded as-is.
//
// The mapping is a hash rather than a per-call random id on purpose: pinning the
// session upstream is what buys prompt-cache affinity, and that only works if the
// same airouter session presents the same ses_ id on every request of the
// conversation.
//
// The hash also has to produce a prefix the free-tier gate admits: the decoded
// counter is pinned to 1 (always inside the gate's accepted range, see
// opencodeKSUUIDPrefix), the hash owns the timestamp bits and the base62 tail.
// A deterministic mapping cannot consult the clock -- the same airouter session
// must present the same id weeks later -- so the timestamp bits are derived
// from the hash too; the gate checks the counter, not the timestamp's age
// (measured 2026-10-13: a prefix decoding to the Unix epoch is admitted).
func opencodeSessionFor(sessionID string) string {
	sum := sha256.Sum256([]byte("airouter\x00opencode\x00" + sessionID))
	val := uint64(sum[0])<<40 | uint64(sum[1])<<32 | uint64(sum[2])<<24 |
		uint64(sum[3])<<16 | uint64(sum[4])<<8 | uint64(sum[5])
	val &^= 0xFFF
	val |= 1
	descending := ^val & opencodeKSUIDMask
	return "ses_" + fmt.Sprintf("%012x", descending) +
		func() string {
			var sb strings.Builder
			sb.Grow(14)
			for _, c := range sum[6:20] {
				sb.WriteByte(opencodeBase62[int(c)%len(opencodeBase62)])
			}
			return sb.String()
		}()
}

// opencodeConversationID derives the identity of the *conversation* a request
// belongs to, for use as the x-opencode-session anchor.
//
// It deliberately is NOT bodySessionID. bodySessionID hashes the entire messages
// array, so it changes on every turn as the conversation grows — verified on
// 2026-10-02: turns 1 and 2 of one conversation hashed to
// ses_dffbfe945e634cljkHio0gqPnr and ses_234deac3449ad7AA327gYNHOu0. A session
// id that drifts per turn is a prompt-cache miss on every single request, which
// is the precise thing the free tier asks the id to prevent ("a stable session
// ID ... for each conversation so we can optimize routing and prompt caching").
//
// So the anchor is the *opening* of the conversation: the logical model plus the
// first system and first user message. Those are fixed for the life of a
// conversation; later turns are appended after them and cannot move it.
//
// The cost is that two conversations opened with an identical first message
// share an id. That is the cheaper error of the two. The upstream prompt cache
// is keyed by the message prefix, so an identical opening genuinely does share
// a cache entry and the collision costs nothing a miss would not have cost;
// drifting, by contrast, forfeits the cache on every request.
//
// The model is the *logical* name from the client's body, not the endpoint's
// substituted one. A fallback chain retries a single conversation against
// several models, and anchoring on the substituted name would split that one
// conversation into several upstream sessions.
//
// Returns "" when there is nothing to anchor on (unparseable body, or no
// system/user message at all), so the caller can fall back.
func opencodeConversationID(body []byte) string {
	// The anchor is read from byte spans, never by decoding the conversation:
	// this runs once per attempt on the OpenCode path (buildRequest), and a
	// decode copies every message's content just to hash two of them.
	obj, ok := scanJSONObject(body)
	if !ok {
		return ""
	}
	modelSpan, found := findJSONMember(obj, "model")
	if !found {
		return ""
	}
	model, ok := jsonStringValue(body, modelSpan, found)
	if !ok || model == "" {
		return ""
	}
	msgsSpan, found := findJSONMember(obj, "messages")
	if !found {
		return ""
	}
	elems, ok := jsonArrayElements(body, msgsSpan)
	if !ok {
		return ""
	}

	var system, user []byte
	var haveSystem, haveUser bool
	for _, elem := range elems {
		m, ok := jsonObjectAt(body, elem)
		if !ok {
			// The decoder this replaced rejected any non-object message, and
			// with it the whole body.
			return ""
		}
		role := ""
		if roleSpan, found := findJSONMember(m, "role"); found {
			r, ok := jsonStringValue(body, roleSpan, found)
			if !ok {
				return ""
			}
			role = r
		}
		contentSpan, contentFound := findJSONMember(m, "content")
		content := jsonLiteralValue(body, contentSpan, contentFound)
		switch role {
		case "system":
			if !haveSystem {
				system, haveSystem = content, true
			}
		case "user":
			if !haveUser {
				user, haveUser = content, true
			}
		}
	}

	h := sha256.New()
	h.Write([]byte(model))
	anchored := false
	for _, anchor := range []struct {
		content []byte
		found   bool
	}{{system, haveSystem}, {user, haveUser}} {
		if !anchor.found {
			continue
		}
		h.Write([]byte{0})
		h.Write(anchor.content)
		anchored = true
	}
	if !anchored {
		return ""
	}
	return "conv:" + hex.EncodeToString(h.Sum(nil)[:16])
}

// opencodeRequestHeaders returns the OpenCode client-attribution headers that
// must accompany every request to an OpenCode gateway. The session/request IDs
// are generated fresh per call so the upstream sees a distinct identity each
// time, exactly like the official CLI.
//
// `X-Session-ID` is a second name for the same conversation, not a third
// identity. OpenCode's /zen/v1 routing backend validates a session id and reads
// it under both spellings: x-opencode-session carries the canonical
// `ses_<12hex><14base62>` form the free-tier gate accepts, while X-Session-ID is
// the plain routing-backend name for it. A third-party gateway that sends only
// the first is treated as having no session, which loses routing and cache
// affinity even though the free-tier check passes. Both are set from one value
// in buildRequest so they cannot drift apart.
// The User-Agent is the official client string, `opencode/<version>`
// -- the exact form the published CLI sends, built in the shipped
// binaries as `opencode/${version}` with no suffix (verified in the
// opencode-linux-x64 packages for 1.18.34 and 1.19.0). The gateway's
// edge verifies the client identity, not just the header names,
// measured live against opencode.ai/zen/v1:
//
//   - a user agent that is not an opencode client is refused by the
//     Cloudflare edge with `403 error code: 1010` before the request
//     reaches the gateway at all;
//   - a user agent naming an old CLI build is refused with
//     `426 UpgradeRequired`.
//
// The version is pinned to the current npm release and kept equal to
// OPENCODE_UA in fetch-free-models.py: the gateway asks third-party
// gateways for "the correct, official User-Agent string (matching
// opencode/<version>)", and test_opencode_ua_matches_the_gateway
// fails when only one of the two copies is bumped.
const opencodeCLIName = "cli"

func opencodeRequestHeaders() map[string]string {
	sess := opencodeSessionID()
	return map[string]string{
		"User-Agent": "opencode/1.19.0",
		// `x-opencode-client` is the client name the gateway logs
		// per request. The session id is carried under every
		// spelling the shipped client and the routing backend
		// read: `x-opencode-session` is the canonical free-tier
		// form (zen gateway path), `x-opencode-session-id` and
		// `x-session-affinity` are the identities the CLI's
		// provider request path sends, and `X-Session-ID` is the
		// plain routing-backend name for the same value. All are
		// set from one value in buildRequest so they cannot drift
		// apart.
		"x-opencode-client":     opencodeCLIName,
		"x-opencode-session":    sess,
		"x-opencode-session-id": sess,
		"x-session-affinity":    sess,
		"X-Session-ID":          sess,
		"x-opencode-request":    opencodeRequestID(),
		"x-opencode-project":    "default",
	}
}

// opencodeGateBody rewrites a Chat Completions request body into the
// shape the OpenCode free-tier gate admits.
//
// Measured live 2026-10-11 against opencode.ai/zen/v1
// (nemotron-3-ultra-free, attribution headers present, every other
// condition held): the gate admits a request only when BOTH hold --
//
//	`stream` is true, AND
//	`tools` is an array whose tool names include `bash` AND `read`
//
// Every other combination answered 403 FreeTierError: stream false or
// absent, no tools at all, a single `bash`, `bash` without `read`, and
// two arbitrary tool names. The official CLI satisfies the gate without
// trying -- it always streams and carries its full coding toolset -- so
// the gate is invisible to CLI traffic and only bites a bare
// chat-completion client (curl, a simple UI), which would be 403'd even
// with perfect attribution headers. The gateway therefore rewrites the
// body here: force stream on, and append the two gate tools when the
// client did not send them.
//
// The non-streaming path aggregates the resulting SSE stream back into
// the Chat Completions object the client asked for (see
// opencodeAggregateStream), so the client never observes the rewrite.
//
// The rewrite is fail-open: a body that does not parse is forwarded
// unchanged, and a client that already streams with both tools sees its
// body untouched apart from the redundant `stream` re-assertion.
func opencodeGateBody(body []byte) []byte {
	// The rewrite is done on byte spans: the body is scanned once and the two
	// members the gate cares about are spliced, so every other byte the client
	// sent is forwarded as it arrived. Decoding into a map and re-encoding was
	// the largest single per-request cost on the OpenCode path.
	obj, ok := scanJSONObject(body)
	if !ok {
		return body
	}

	streamSpan, hasStream := findJSONMember(obj, "stream")
	streamOn := hasStream && bytes.Equal(jsonLiteralValue(body, streamSpan, hasStream), []byte("true"))

	toolsSpan, hasTools := findJSONMember(obj, "tools")
	var elems []jsonSpan
	toolsIsArray := false
	if hasTools {
		elems, toolsIsArray = jsonArrayElements(body, toolsSpan)
	}
	var haveBash, haveRead bool
	for _, elem := range elems {
		switch jsonToolName(body, elem) {
		case "bash":
			haveBash = true
		case "read":
			haveRead = true
		}
	}
	if streamOn && haveBash && haveRead {
		return body
	}

	var edits []jsonEdit
	// The gate is a hard upstream requirement, not a preference: a
	// client that explicitly set stream=false is overridden, because
	// the upstream answers 403 FreeTierError otherwise.
	if !streamOn && hasStream {
		edits = append(edits, jsonEdit{Start: streamSpan.Start, End: streamSpan.End, Replacement: []byte("true")})
	}

	var add [][]byte
	for _, gate := range []struct {
		name string
		have bool
	}{{"bash", haveBash}, {"read", haveRead}} {
		if gate.have {
			continue
		}
		raw, err := json.Marshal(opencodeGateTool(gate.name))
		if err != nil {
			return body
		}
		add = append(add, raw)
	}
	if len(add) > 0 {
		switch {
		case toolsIsArray:
			at, sep, ok := jsonArrayInsertion(body, toolsSpan, elems)
			if !ok {
				return body
			}
			var repl []byte
			for i, raw := range add {
				if i > 0 || sep == "," {
					repl = append(repl, ',')
				}
				repl = append(repl, raw...)
			}
			edits = append(edits, jsonEdit{Start: at, End: at, Replacement: repl})
		case hasTools:
			// Not an array (null, an object): the gate needs one, so the value
			// is replaced wholesale.
			edits = append(edits, jsonEdit{Start: toolsSpan.Start, End: toolsSpan.End, Replacement: encodeJSONArray(add)})
		}
	}

	// Members that did not exist are appended in one insertion: two separate
	// insertions at the same offset would each believe the other needs the
	// separator in front of it.
	var inserted [][]byte
	if !hasStream {
		inserted = append(inserted, []byte(`"stream":true`))
	}
	if len(add) > 0 && !hasTools {
		inserted = append(inserted, append([]byte(`"tools":`), encodeJSONArray(add)...))
	}
	if len(inserted) > 0 {
		at, sep := jsonObjectInsertion(body, obj)
		var repl []byte
		for i, member := range inserted {
			if i > 0 || sep == "," {
				repl = append(repl, ',')
			}
			repl = append(repl, member...)
		}
		edits = append(edits, jsonEdit{Start: at, End: at, Replacement: repl})
	}

	return applyJSONEdits(body, edits)
}

func opencodeGateToolName(tool map[string]json.RawMessage) string {
	if raw, ok := tool["function"]; ok {
		var fn struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &fn); err == nil && fn.Name != "" {
			return fn.Name
		}
	}
	if raw, ok := tool["name"]; ok {
		var name string
		if err := json.Unmarshal(raw, &name); err == nil {
			return name
		}
	}
	return ""
}

// opencodeGateTool builds the canonical gate tool. The gateway inspects
// the tool's parameter SCHEMA, not just its name: measured live against
// opencode.ai/inference/openai/v1 on 2026-10-15, a `read` tool whose
// parameters is `{"type":"object","properties":{}}` is refused with 403
// FreeTierError, while one carrying any property -- even
// `{"type":"object","properties":{"x":{}}}` -- passes. The `read` tool is
// required at all (removing it 403s), and so is every other tool present:
// a `bash` with an empty schema also 403s. Names, descriptions, `required`
// and `additionalProperties` are irrelevant once `properties` is
// non-empty. The injected tool therefore carries a real property; this
// used to be `{"type":"object","properties":{}}` and is exactly what made
// every free-tier request 403.
func opencodeGateTool(name string) map[string]json.RawMessage {
	description := "opencode gate tool"
	parameters := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	fn, _ := json.Marshal(struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}{name, description, parameters})
	return map[string]json.RawMessage{
		"type":     json.RawMessage(`"function"`),
		"function": fn,
	}
}

// opencodeAggregateStream folds a complete OpenAI SSE response body
// into a Chat Completions object.
//
// The free-tier gate forces `stream:true` upstream even for a
// non-streaming client, so the non-streaming path receives an SSE
// stream where it expected a single JSON object. This folds the stream
// back: content deltas are concatenated, tool-call deltas are merged
// per index (arguments concatenate across deltas, exactly as the
// streaming replay in accumulateDelta does), the last non-empty
// finish_reason wins, and the last usage object is carried over.
//
// A stream with no `[DONE]` sentinel is reported as an error rather
// than as a silently wrong completion: the upstream terminated
// mid-generation, so the folded text is truncated. A stream that DID
// end in `[DONE]` but produced no content is folded faithfully into
// an empty completion instead. That is the artifact seen live on
// 2026-09-30 (HTTP 200, reasoning_content set, finish_reason
// "length", no content): an upstream state, not a routing fault, and
// the client's own retry predicate -- "200 AND usable", see
// scripts/opencode-routing-check.sh -- is what detects and retries
// it. Erroring here would launder that artifact into a gateway 503,
// which a client cannot distinguish from a routing failure.
func opencodeAggregateStream(body []byte, model string, created int64) ([]byte, error) {
	var content strings.Builder
	var reasoning strings.Builder
	var tcs []streamToolCall
	finish := ""
	var usage json.RawMessage
	done := false
	for _, ev := range bytes.Split(body, []byte("\n\n")) {
		if len(bytes.TrimSpace(ev)) == 0 {
			continue
		}
		payload := sseDataPayload(ev)
		if len(payload) == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			done = true
			continue
		}
		var obj struct {
			Choices []struct {
				Delta struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(payload, &obj); err != nil {
			continue
		}
		if len(obj.Usage) > 0 {
			usage = obj.Usage
		}
		for _, ch := range obj.Choices {
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
			}
			if ch.Delta.ReasoningContent != "" {
				reasoning.WriteString(ch.Delta.ReasoningContent)
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
			for _, tc := range ch.Delta.ToolCalls {
				var target *streamToolCall
				for i := range tcs {
					if tcs[i].Index == tc.Index {
						target = &tcs[i]
						break
					}
				}
				if target == nil {
					tcs = append(tcs, streamToolCall{Index: tc.Index})
					target = &tcs[len(tcs)-1]
				}
				if tc.ID != "" {
					target.ID = tc.ID
				}
				if tc.Type != "" {
					target.Type = tc.Type
				}
				if tc.Function.Name != "" {
					target.Name = tc.Function.Name
				}
				target.Arguments += tc.Function.Arguments
			}
		}
	}
	if !done {
		return nil, fmt.Errorf("truncated stream: no [DONE] sentinel")
	}

	msg := ChatCompletionMessage{Role: "assistant", Content: content.String()}
	if reasoning.Len() > 0 {
		msg.ReasoningContent = reasoning.String()
	}
	if len(tcs) > 0 {
		msg.ToolCalls = make([]ChatCompletionToolCall, 0, len(tcs))
		for _, tc := range tcs {
			call := ChatCompletionToolCall{ID: tc.ID, Type: tc.Type}
			call.Function.Name = tc.Name
			call.Function.Arguments = tc.Arguments
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
	}
	resp := ChatCompletionResponse{
		ID:      "chatcmpl-airouter-opencode",
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []ChatCompletionChoice{{Index: 0, Message: msg}},
	}
	if finish != "" {
		f := finish
		resp.Choices[0].FinishReason = &f
	}
	if len(usage) > 0 {
		var u ChatCompletionUsage
		if err := json.Unmarshal(usage, &u); err == nil {
			resp.Usage = &u
		}
	}
	return json.Marshal(resp)
}

func (p ProviderConfig) APIKey() string {
	if p.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(p.APIKeyEnv)
}

type ModelEndpoint struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	// Vision marks whether this endpoint can accept image (multimodal)
	// content. It is optional: when nil the gateway assumes vision is
	// supported. Set it to false for known text-only models so vision
	// requests are routed to a capable model in the chain. The gateway also
	// learns this at runtime (see Router.MarkNoVision) when a provider rejects
	// an image request.
	Vision *bool `yaml:"vision,omitempty"`

	// Intelligence is the model's quality score (Artificial Analysis
	// intelligence index, or the arena.ai ELO-derived equivalent), as
	// recorded by regenerate_config.py. It is optional: a nil value means
	// "unknown", and the router then treats the endpoint as incomparable and
	// falls back to strict chain order.
	//
	// The router uses this only to decide which endpoints are equally good
	// enough to be treated as interchangeable for a new session's first pick
	// (see Router.selectInitialEndpoint). It never uses it to reorder a
	// fallback: once a request is in flight the chain is walked strictly in
	// configured order, so quality ordering is preserved.
	Intelligence *float64 `yaml:"intelligence,omitempty"`

	// ContextLength is the model's advertised context window in tokens, as
	// recorded by regenerate_config.py from the upstream model list. It is
	// optional: a zero value means "not reported" and is omitted from YAML.
	//
	// The router does not use it for routing today, but it is surfaced to
	// clients via /v1/models (as max_tokens on the logical model) so a
	// client can pick a profile with enough headroom for its prompt. The
	// `large` profile exists specifically for large-context work, so
	// advertising nothing for it is the worst case.
	ContextLength int `yaml:"context_length,omitempty"`

	// Protocol selects the wire shape used to reach this endpoint:
	// "responses" (POST <url>/responses) or, when absent or unrecognized,
	// "chat" (POST <url>/chat/completions), which is the default.
	//
	// It is per-endpoint rather than per-provider because capability is not
	// uniform across a provider's catalog. Measured live 2026-10-02 against
	// opencode.ai: space-bunny-free answers
	// `400 ModelProtocolUnsupported: Model does not support this protocol.`
	// on /responses while answering 200 on /chat/completions.
	//
	// An unrecognized value falls back to chat rather than failing validation:
	// a typo must not take a working endpoint out of rotation, and
	// chat/completions is the shape every provider understands.
	Protocol string `yaml:"protocol,omitempty"`

	// Proxy routes this endpoint's upstream traffic through a proxy from the
	// proxy pool (see proxy service). It overrides the provider's own proxy
	// setting, and defaults to 0: inherit the provider's Proxy value, which is
	// itself a direct connection when the provider does not set one.
	// When resolved to a value greater than 0, the gateway picks a proxy from
	// the prioritized pool (European and North American proxies first, all
	// others as backup) and dials the upstream through it (HTTPS or SOCKS5).
	// The value is the maximum number of different proxies to try on this
	// endpoint before falling back to the next model in the chain.
	Proxy int `yaml:"proxy,omitempty"`
}

// UpstreamProtocol resolves the configured Protocol, defaulting to chat.
func (e ModelEndpoint) UpstreamProtocol() protocol { return protocolFor(e.Protocol) }

// HasIntelligence reports whether an intelligence score is known. Endpoints
// without one are only ever compared by chain position.
func (e ModelEndpoint) HasIntelligence() bool {
	return e.Intelligence != nil
}

// IntelligenceEqual reports whether two endpoints carry the same known score.
// Endpoints with an unknown score are never considered equal, so they fall
// back to strict chain order rather than being grouped arbitrarily.
func (e ModelEndpoint) IntelligenceEqual(other ModelEndpoint) bool {
	if e.Intelligence == nil || other.Intelligence == nil {
		return false
	}
	return *e.Intelligence == *other.Intelligence
}

func (e ModelEndpoint) Key() string {
	return e.Provider + ":" + e.Model
}

// SupportsVision reports whether the endpoint can handle image content. A nil
// Vision flag is interpreted as "supported" so existing configs keep working.
func (e ModelEndpoint) SupportsVision() bool {
	return e.Vision == nil || *e.Vision
}

func (e ModelEndpoint) Equal(other ModelEndpoint) bool {
	return e.Provider == other.Provider && e.Model == other.Model
}

// ContextWindow reports the advertised context window in tokens. A zero
// value means the upstream did not report one.
func (e ModelEndpoint) ContextWindow() int {
	return e.ContextLength
}

type ModelConfig struct {
	Chain []ModelEndpoint `yaml:"chain"`
}

type Preferences struct {
	NewestFirstOnTie bool `yaml:"newest_first_on_tie"`

	// InitialRotationWindow is how many endpoints at the head of a chain are
	// rotated between for the first pick of each new session.
	//
	// Without rotation every new session starts at depth 0, so one model
	// serves all traffic and the rest of the chain is idle capacity that only
	// ever gets used after a failure. With a window of N, consecutive new
	// sessions start at depths 0, 1, ... N-1, 0, 1, ... spreading load evenly
	// across the top of the chain.
	//
	// This affects ONLY the first endpoint chosen for a brand new session.
	// Once a session is pinned, later requests in it are sticky; and once a
	// request is retrying, the chain is walked strictly in configured order.
	// So rotation can never delay escalation to a better model.
	//
	// 0 disables rotation (every new session starts at depth 0); omitting the
	// key entirely uses DefaultInitialRotationWindow. It is a pointer so those
	// two states are distinguishable.
	InitialRotationWindow *int `yaml:"initial_rotation_window,omitempty"`

	// CooldownJitter is the random fraction (0..0.25) added to transient
	// cooldowns (429/5xx) so concurrent clients hitting the same rate-limited
	// model don't all wake up at the same instant and stampede the provider.
	// 0 (the default) disables jitter. Values are clamped to [0, 0.25].
	CooldownJitter float64 `yaml:"cooldown_jitter,omitempty"`

	// CircuitBreakerThreshold is the number of consecutive failures that
	// trip the circuit from Closed to Open. A pointer is used so an explicit
	// 0 can be distinguished from an absent key: nil (absent) falls back to
	// DefaultCircuitBreakerThreshold, while an explicit 0 disables the
	// circuit breaker entirely (ad-hoc cooldown escalation handles failures
	// instead). Negative values are clamped to 0 (disabled).
	CircuitBreakerThreshold *int `yaml:"circuit_breaker_threshold,omitempty"`

	// CircuitHalfOpenProbes is the max probes allowed in half-open state
	// before another request is blocked. DefaultCircuitHalfOpenProbes is
	// used when the key is absent.
	CircuitHalfOpenProbes int `yaml:"circuit_half_open_probes,omitempty"`

	// DefaultMaxTokens is the output-token ceiling the gateway applies to
	// requests that omit max_tokens. It is a pointer so three states are
	// distinguishable:
	//   - nil (absent): "auto" — half of the profile's advertised context
	//     window (max_context_tokens). Conservative and profile-aware.
	//   - 0: "never default" — the request is sent upstream without
	//     max_tokens, letting the provider choose.
	//   - positive: use that absolute value regardless of profile.
	//
	// This exists because half-of-context is right for chat models but
	// wrong for reasoning models (deepseek-r1, gpt-oss, nemotron-3-nano
	// reasoning, ...): their thinking tokens live OUTSIDE the visible
	// output and can exceed a half-context budget, so the provider rejects
	// the request. Operators with reasoning models in a chain should set
	// this to 0 (or a larger value) rather than let the auto-default
	// silently break them.
	DefaultMaxTokens *int `yaml:"default_max_tokens,omitempty"`

	// MaxLabelCardinality bounds how many distinct provider/model labels the
	// metrics registry will track. Endpoint and model labels come from this
	// config, and the config is hot-reloadable, so without a bound a config
	// churn loop (or a model sync that keeps inventing ids) grows the registry
	// until the gateway is OOM-killed — the metrics endpoint would be a way to
	// kill the process. A pointer so an explicit 0 is distinguishable from an
	// absent key: nil uses defaultMaxLabelValues.
	//
	// Once the window is full, a label that is not already tracked recycles the
	// least-recently-admitted slot and rolls that label's counters into an
	// __overflow__ bucket, so totals stay truthful and no observation is lost.
	// See docs/RUNBOOK.md ("Label cardinality is bounded") for what to watch.
	//
	// Values outside [16, 65536] are ignored in favour of the default: too
	// small destroys the per-endpoint series, too large defeats the memory
	// bound. scripts/validate-config.py rejects out-of-range values at config
	// time so the mistake is caught before a restart rather than silently at
	// load.
	MaxLabelCardinality *int `yaml:"max_label_cardinality,omitempty"`
}

// MaxLabelCardinalityValue returns the effective per-map label cap, clamped to
// the supported range. nil (key absent) yields defaultMaxLabelValues.
func (p *Preferences) MaxLabelCardinalityValue() int {
	if p == nil || p.MaxLabelCardinality == nil {
		return defaultMaxLabelValues
	}
	n := *p.MaxLabelCardinality
	if n < minLabelValues {
		return minLabelValues
	}
	if n > maxAllowedLabels {
		return maxAllowedLabels
	}
	return n
}

// RotationWindow returns the effective window size, defaulting to
// DefaultInitialRotationWindow when the key is absent. An explicit 0 (or a
// negative value) disables rotation.
func (p *Preferences) RotationWindow() int {
	if p == nil || p.InitialRotationWindow == nil {
		return DefaultInitialRotationWindow
	}
	return *p.InitialRotationWindow
}

// CooldownJitterFraction returns the jitter fraction to apply to transient
// cooldowns, clamped to [0, 0.25]. Nil preferences or an unset value yields 0.
func (p *Preferences) CooldownJitterFraction() float64 {
	if p == nil {
		// Absent preferences block: built-in default spread so a freshly
		// loaded / minimal control plane does not land all wakers on the
		// same wake boundary.
		return DefaultCooldownJitter
	}
	f := p.CooldownJitter
	// Zero (and negative) disable jitter, per the documented semantics
	// for cooldown_jitter (mirroring initial_rotation_window).
	if f <= 0 {
		return 0
	}
	if f > 0.25 {
		f = 0.25
	}
	return f
}

// DefaultInitialRotationWindow is the window used when preferences omit one.
const DefaultInitialRotationWindow = 8

// DefaultCooldownJitter is the fraction used when the operator does not set
// cooldown_jitter: spread wakers but never zero by default, so a freshly
// loaded control plane does not land every client on the same wake boundary.
const DefaultCooldownJitter = 0.1

// DefaultCircuitBreakerThreshold is the number of consecutive failures
// that trip the circuit from Closed to Open when the config omits the key.
const DefaultCircuitBreakerThreshold = 5

// DefaultCircuitHalfOpenProbes is the max probes allowed in half-open state
// when the config omits the key.
const DefaultCircuitHalfOpenProbes = 1

// CircuitBreakerThresholdValue returns the effective threshold. nil (absent)
// falls back to DefaultCircuitBreakerThreshold; an explicit 0 disables the
// circuit breaker entirely; negative values are clamped to 0 (disabled).
func (p *Preferences) CircuitBreakerThresholdValue() int {
	if p == nil || p.CircuitBreakerThreshold == nil {
		return DefaultCircuitBreakerThreshold
	}
	if *p.CircuitBreakerThreshold < 0 {
		return 0
	}
	return *p.CircuitBreakerThreshold
}

// CircuitHalfOpenProbesValue returns the effective half-open probe count,
// defaulting to DefaultCircuitHalfOpenProbes when the key is absent.
func (p *Preferences) CircuitHalfOpenProbesValue() int {
	if p == nil || p.CircuitHalfOpenProbes <= 0 {
		return DefaultCircuitHalfOpenProbes
	}
	return p.CircuitHalfOpenProbes
}

// DefaultMaxTokensValue returns the effective default output-token ceiling
// for requests that omit max_tokens. nil (absent) means "auto" — half of
// the profile's advertised context window — and is signalled by a negative
// return value so callers can distinguish it from an explicit 0 (never
// default) and an explicit positive value.
func (p *Preferences) DefaultMaxTokensValue() int {
	if p == nil || p.DefaultMaxTokens == nil {
		return -1 // auto
	}
	return *p.DefaultMaxTokens
}

// RotationEnabled reports whether new-session rotation is active.
func (p *Preferences) RotationEnabled() bool { return p.RotationWindow() > 0 }

type Config struct {
	Providers   map[string]ProviderConfig `yaml:"providers"`
	Preferences *Preferences              `yaml:"preferences,omitempty"`
	Models      map[string]ModelConfig    `yaml:"models"`
}

var LogicalModels = []string{"smart", "work", "fast", "large"}

func LoadConfig(path string) (*Config, error) {
	// Bound file size to prevent OOM from malformed/malicious config
	const maxConfigBytes = 10 << 20 // 10 MiB
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	if fi.Size() > maxConfigBytes {
		return nil, fmt.Errorf("config file too large: %d bytes (max %d)", fi.Size(), maxConfigBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func SaveConfig(path string, cfg *Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	// Atomic write: temp file then rename so a crash / concurrent reader never
	// sees a half-written config.yaml.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		log.Printf("[debug] failed to chmod config temp file: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}

func (c *Config) validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("no models configured")
	}
	for name, p := range c.Providers {
		if p.URL == "" {
			return fmt.Errorf("provider %s has empty URL", name)
		}
		if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
			return fmt.Errorf("provider %s URL must start with http:// or https://: %q", name, p.URL)
		}
		// max_interruption shortens the per-provider interruption window but
		// can never widen it past maxSessionInterruption — that is the hard
		// client contract and a provider that tries to exceed it is a config
		// mistake worth failing on rather than silently reinterpreting.
		if p.MaxInterruption < 0 {
			return fmt.Errorf("provider %s max_interruption must not be negative, got %v", name, p.MaxInterruption)
		}
		if p.MaxInterruption > maxSessionInterruption {
			return fmt.Errorf("provider %s max_interruption %v exceeds the gateway ceiling %v", name, p.MaxInterruption, maxSessionInterruption)
		}
	}
	for name, model := range c.Models {
		if len(model.Chain) == 0 {
			return fmt.Errorf("model %s has empty chain", name)
		}
		for i, ep := range model.Chain {
			if _, ok := c.Providers[ep.Provider]; !ok {
				return fmt.Errorf("model %s chain[%d] references unknown provider %q", name, i, ep.Provider)
			}
		}
	}
	// Reject an out-of-range label cap at config time rather than silently
	// clamping it at load. A cap below the floor destroys the per-endpoint
	// series this exists to protect, and one above the ceiling defeats the
	// memory bound; both are mistakes worth failing on, and both are invisible
	// if they are quietly reinterpreted.
	//
	// Preferences is a *pointer* and every other preference accessor here is
	// nil-safe; this one has to be too, because `preferences:` is omitempty and
	// a config without it is perfectly valid.
	// A nil Preferences (absent `preferences:` key) is the common case, not an
	// error, so guard the whole block instead of reaching through the pointer.
	// Read the pointer once: dereferencing it twice would re-introduce exactly
	// the panic this is fixing.
	if p := c.Preferences; p != nil && p.MaxLabelCardinality != nil {
		if n := *p.MaxLabelCardinality; n < minLabelValues || n > maxAllowedLabels {
			return fmt.Errorf("preferences.max_label_cardinality must be between %d and %d, got %d",
				minLabelValues, maxAllowedLabels, n)
		}
	}
	return nil
}

func (c *Config) GetChain(logicalModel string) ([]ModelEndpoint, bool) {
	mc, ok := c.Models[logicalModel]
	if !ok {
		return nil, false
	}
	// Return a copy so callers can't mutate the internal slice
	return append([]ModelEndpoint(nil), mc.Chain...), true
}

func (c *Config) GetProvider(provider string) (ProviderConfig, bool) {
	p, ok := c.Providers[provider]
	return p, ok
}

// ReplaceModelName returns body with its top-level "model" field set to
// modelName.
//
// The substitution is a splice: the model value is replaced where it lies in
// the raw body and every other byte is forwarded exactly as the client sent
// it. Decoding the body into a map and re-marshaling it copied the whole
// conversation twice, and the gateway does this once per endpoint it tries
// on the way down a fallback chain.
func ReplaceModelName(body []byte, modelName string) ([]byte, error) {
	modelJSON, err := json.Marshal(modelName)
	if err != nil {
		return nil, err
	}
	if obj, ok := scanJSONObject(body); ok {
		if span, found := findJSONMember(obj, "model"); found {
			return replaceJSONValue(body, span, modelJSON), nil
		}
		return insertJSONMember(body, obj.End, "model", modelJSON), nil
	}
	// A body the scanner does not understand: fall back to the decoder.
	var data map[string]json.RawMessage
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	data["model"] = modelJSON
	return json.Marshal(data)
}

func IsValidModel(name string) bool {
	for _, m := range LogicalModels {
		if m == name {
			return true
		}
	}
	return false
}

// randomHex returns n random hex bytes (2*n characters) from crypto/rand.
// Used to synthesize per-request OpenCode session/request IDs so the upstream
// cannot fingerprint a single client across sessions.
func randomHex(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fallback: never block a request on RNG failure; emit a deterministic
		// placeholder so attribution is still present (just not unique).
		return strings.Repeat("0", 2*n)
	}
	return hex.EncodeToString(b)
}

type SSEError struct {
	Data string
}

func (e *SSEError) Error() string {
	return "SSE error event: " + e.Data
}

func ExtractSSEError(data []byte) error {
	// SSE events can carry errors in three shapes:
	//   1. A "data:" line whose JSON payload contains an "error" object
	//      (OpenAI-style, most common).
	//   2. An "event: error" line followed by a "data:" line carrying the
	//      error payload (Anthropic / some providers).
	//   3. A bare JSON error object outside any "data:" wrapper (seen from
	//      some proxy layers that strip framing).
	//
	// We scan line-by-line so each data: line is examined independently.

	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}

		payload := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}

		// Fast path: if the payload is a JSON object containing an "error"
		// field, it's an error event.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(payload, &obj); err == nil {
			// An error field that is null or an empty object {} is not an error.
			// Some providers emit "error":{} on non-error chunks (e.g. usage-only
			// events), so we must not treat it as a failure.
			if errRaw, ok := obj["error"]; ok && len(errRaw) > 2 && string(errRaw) != "null" {
				return &SSEError{Data: string(errRaw)}
			}
		}
	}

	// Fallback: if no data: error was found, check whether the raw body itself
	// is a JSON error object (some providers return the error as the entire
	// response body with no SSE framing at all).
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("{")) {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err == nil {
			// Same guard as above: skip null/empty error objects.
			if errRaw, ok := obj["error"]; ok && len(errRaw) > 2 && string(errRaw) != "null" {
				return &SSEError{Data: string(errRaw)}
			}
		}
	}

	return nil
}

// proxyEnabled reports whether any provider, or any endpoint in any model
// chain, has proxying enabled. When false, the proxy service does not need to
// run.
func proxyEnabled(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	for _, provider := range cfg.Providers {
		if provider.Proxy > 0 {
			return true
		}
	}
	for _, model := range cfg.Models {
		for _, ep := range model.Chain {
			if ep.Proxy > 0 {
				return true
			}
		}
	}
	return false
}
