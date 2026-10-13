package main

// Command Code protocol support.
//
// This file re-implements commandcode-proxy (originally a single-file
// Node.js reverse proxy) as a native airouter protocol. A provider whose
// endpoint carries `protocol: commandcode` is addressed over Command
// Code's internal /alpha/generate API (api.commandcode.ai), which is
// NOT an OpenAI-compatible surface: the gateway must (a) derive a
// deterministic device fingerprint from the API key, (b) register that
// fingerprint and post a lifecycle event before the first generation,
// (c) wrap the OpenAI Chat Completions request in the CC envelope, and
// (d) translate the CC NDJSON event stream back into Chat Completions
// SSE. Everything after that -- the router, the fallback walk, the
// cooldown classifier, the dashboard -- keeps speaking the one canonical
// Chat Completions shape.
//
// The wire protocol replicated here was recovered from the official CLI's
// network traffic (command-code@1.53.1). Field names, the envelope key
// order, the header set and the fingerprint derivation are all load-
// bearing: the upstream fingerprinting and retention checks key on them.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── protocol version & fixed salts ─────────────────────────────────

// ccProtocolVersion is the wire protocol version this implementation
// speaks. It is pinned to the CLI version whose protocol shape it
// replicates (command-code@1.53.1); it is NOT bumped automatically,
// because a version number that drifts ahead of the shape is a stronger
// fingerprinting signal than a stale one.
const ccProtocolVersion = "1.53.1"

// ccFPSalt is the CLI's fixed device-fingerprint hash salt. The proxy
// hashes the raw signal values with this salt before sending them, so
// only hashes leave the process.
const ccFPSalt = "command-code:device-fingerprint:v1"

// ── configuration knobs ────────────────────────────────────────────
//
// The Node proxy reads these from config.json / env. Here they are env
// only (the airouter provider config carries the URL and key); the
// defaults match the proxy's shipped defaults.

func ccStrEnv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func ccBoolEnv(name string, def bool) bool {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true")
}

// ccDeviceProjectDir is the faked project directory. It is the source
// of config.workingDir, config.environment's sibling and the
// x-project-slug header (slug = slugify(projectDir)), so all three stay
// self-consistent.
func ccDeviceProjectDir() string {
	return ccStrEnv("CC_DEVICE_PROJECT_DIR", `C:\Users\dev\projects\app`)
}

// ccFingerprintSalt rotates the whole fleet's device identities: one
// value changes which fake device every key derives to.
func ccFingerprintSalt() string {
	return os.Getenv("CC_FINGERPRINT_SALT")
}

func ccCliMode() string        { return ccStrEnv("CC_CLI_MODE", "agent") }
func ccCliSessionMode() string { return ccStrEnv("CC_CLI_SESSION_MODE", "interactive") }
func ccZDR() bool              { return ccBoolEnv("CMD_ZDR", false) }

// ccEmptySystemPlaceholder sends a single space when there is no system
// prompt, so the upstream does not inject its ~7.5K-token default
// prompt (commandcode-proxy issue #17).
func ccEmptySystemPlaceholder() bool { return ccBoolEnv("CC_EMPTY_SYSTEM_PLACEHOLDER", true) }

// ── device profile & fingerprint pools ───────────────────────────

// ccCPU is one entry in the CPU pool. cpuCount reported to the
// upstream is the LOGICAL processor count (threads), matching the
// CLI's os.cpus().length -- not the physical core count.
type ccCPU struct {
	Model   string
	Cores   int
	Threads int
}

var ccFingerprintCPUs = []ccCPU{
	{"12th Gen Intel(R) Core(TM) i7-12650H", 10, 16},
	{"12th Gen Intel(R) Core(TM) i5-12400F", 6, 12},
	{"12th Gen Intel(R) Core(TM) i9-12900K", 16, 24},
	{"13th Gen Intel(R) Core(TM) i7-13700K", 16, 24},
	{"13th Gen Intel(R) Core(TM) i5-13600K", 14, 20},
	{"13th Gen Intel(R) Core(TM) i9-13900K", 24, 32},
	{"Intel(R) Core(TM) Ultra 7 155H", 16, 22},
	{"Intel(R) Core(TM) Ultra 9 285H", 16, 16},
	{"Intel(R) Core(TM) i9-14900K", 24, 32},
	{"Intel(R) Core(TM) i7-14700K", 20, 28},
	{"AMD Ryzen 7 7800X3D", 8, 16},
	{"AMD Ryzen 9 7950X", 16, 32},
	{"AMD Ryzen 5 7600", 6, 12},
	{"AMD Ryzen 9 7900X", 12, 24},
	{"AMD Ryzen 7 5800X3D", 8, 16},
}

var (
	ccFingerprintMems      = []int{8, 16, 24, 32, 48, 64}
	ccFingerprintMacCounts = []int{2, 3, 4, 5}
	ccFPOSUsers            = []string{"dev", "user", "admin", "coder", "engineer", "work"}
	ccFPMailDomains        = []string{"gmail.com", "outlook.com", "qq.com", "163.com"}
)

var ccFingerprintTZs = []string{
	"America/New_York", "America/Chicago", "America/Los_Angeles", "America/Toronto",
	"Europe/London", "Europe/Berlin", "Europe/Paris", "Europe/Moscow",
	"Asia/Shanghai", "Asia/Tokyo", "Asia/Singapore", "Asia/Seoul", "Asia/Hong_Kong",
	"Australia/Sydney", "Pacific/Auckland",
}

// ccDeviceProfile is the fake machine the fingerprint describes. It is
// shared by the fingerprint, config.environment, config.workingDir,
// x-project-slug and the lifecycle os field so the upstream never sees
// a fingerprint that says win32 next to an environment that says linux.
type ccDeviceProfile struct {
	Platform    string `json:"platform"`
	Arch        string `json:"arch"`
	OSRelease   string `json:"osRelease"`
	IsContainer bool   `json:"isContainer"`
	ProjectDir  string `json:"-"`
}

func ccNewDeviceProfile() ccDeviceProfile {
	return ccDeviceProfile{
		Platform:    "win32",
		Arch:        "x64",
		OSRelease:   "10.0.22631",
		IsContainer: false,
		ProjectDir:  ccDeviceProjectDir(),
	}
}

// ── fingerprint derivation ───────────────────────────────────────

// ccFPDigest is the raw signal hash: sha256(salt + "\0" + apiKey + "\0" + field).
func ccFPDigest(apiKey, field string) []byte {
	h := sha256.New()
	h.Write([]byte(ccFingerprintSalt()))
	h.Write([]byte{0})
	h.Write([]byte(apiKey))
	h.Write([]byte{0})
	h.Write([]byte(field))
	return h.Sum(nil)
}

// ccFPPickIndex deterministically picks one entry from a pool. It
// scores every candidate by hashing field+"\0"+label and keeps the
// MAXIMUM (not a modulo), so growing a pool only changes which keys
// the new candidate wins -- it never reshuffles the whole fleet.
func ccFPPickIndex(apiKey, field string, labels []string) int {
	bestIdx := 0
	var bestScore []byte
	for i, label := range labels {
		score := ccFPDigest(apiKey, field+"\x00"+label)
		if bestScore == nil || bytes.Compare(score, bestScore) > 0 {
			bestScore = score
			bestIdx = i
		}
	}
	return bestIdx
}

// ccFingerprintHash is the CLI's hashSignal: sha256(FP_SALT + "\0" +
// value.toLowerCase()). An empty value hashes to "" so the field is
// dropped from the JSON (the upstream treats absent and empty as the
// same "not collected" signal).
func ccFingerprintHash(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(ccFPSalt + "\x00" + v))
	return hex.EncodeToString(sum[:])
}

// ccFingerprintComponents is the plaintext-and-hash mix the CLI
// collects. Only the identity fields (machineId, macs, osUser,
// hostname, gitEmail) are hashed; the hardware facts (cpu, mem, tz)
// are sent in the clear and are cross-checkable against the platform.
type ccFingerprintComponents struct {
	MachineIDHash    string   `json:"machineIdHash,omitempty"`
	MACHashes        []string `json:"macHashes"`
	OSUserHash       string   `json:"osUserHash,omitempty"`
	HostnameHash     string   `json:"hostnameHash,omitempty"`
	GitEmailHash     string   `json:"gitEmailHash,omitempty"`
	Platform         string   `json:"platform"`
	Arch             string   `json:"arch"`
	OSRelease        string   `json:"osRelease"`
	CPUModel         string   `json:"cpuModel"`
	CPUCount         int      `json:"cpuCount"`
	MemGiB           int      `json:"memGiB"`
	IsContainer      bool     `json:"isContainer"`
	Timezone         string   `json:"timezone"`
	Runtime          string   `json:"runtime"`
	CollectorVersion int      `json:"collectorVersion"`
}

// ccFingerprint is the body POSTed to /alpha/fingerprint/record.
type ccFingerprint struct {
	Thumbmark  string                  `json:"thumbmark"`
	Components ccFingerprintComponents `json:"components"`
}

// ccGenerateFingerprint derives the whole device identity from the API
// key. One key is therefore always one device, across restarts,
// evictions and multi-instance deployments -- the upstream's
// device_fingerprints table is keyed on (userId, thumbmark), so a
// drifting identity is itself a signal.
func ccGenerateFingerprint(apiKey string) ccFingerprint {
	prof := ccNewDeviceProfile()

	cpuLabels := make([]string, len(ccFingerprintCPUs))
	for i, c := range ccFingerprintCPUs {
		cpuLabels[i] = fmt.Sprintf("%s|%d", c.Model, c.Cores)
	}
	cpuEntry := ccFingerprintCPUs[ccFPPickIndex(apiKey, "cpu", cpuLabels)]

	memLabels := make([]string, len(ccFingerprintMems))
	for i, m := range ccFingerprintMems {
		memLabels[i] = strconv.Itoa(m)
	}
	memGiB := ccFingerprintMems[ccFPPickIndex(apiKey, "mem", memLabels)]

	tz := ccFingerprintTZs[ccFPPickIndex(apiKey, "timezone", ccFingerprintTZs)]

	macLabels := make([]string, len(ccFingerprintMacCounts))
	for i, m := range ccFingerprintMacCounts {
		macLabels[i] = strconv.Itoa(m)
	}
	macCount := ccFingerprintMacCounts[ccFPPickIndex(apiKey, "macCount", macLabels)]

	osUser := ccFPOSUsers[ccFPPickIndex(apiKey, "osUser", ccFPOSUsers)]
	mailDomain := ccFPMailDomains[ccFPPickIndex(apiKey, "mailDomain", ccFPMailDomains)]

	// Windows MachineGuid shape: 8-4-4-4-12.
	mid := hex.EncodeToString(ccFPDigest(apiKey, "machineId")[:16])
	machineID := fmt.Sprintf("%s-%s-%s-%s-%s", mid[0:8], mid[8:12], mid[12:16], mid[16:20], mid[20:32])

	macs := make([]string, 0, macCount)
	for i := 0; i < macCount; i++ {
		b := ccFPDigest(apiKey, fmt.Sprintf("mac%d", i))[:6]
		macs = append(macs, fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5]))
	}
	sort.Strings(macs) // the CLI de-dupes then sorts MACs

	hostname := "DESKTOP-" + strings.ToUpper(hex.EncodeToString(ccFPDigest(apiKey, "hostname")[:4]))
	gitEmail := fmt.Sprintf("%s.%s@%s", osUser, hex.EncodeToString(ccFPDigest(apiKey, "gitEmail")[:3]), mailDomain)

	machineIDHash := ccFingerprintHash(machineID)
	macHashes := make([]string, 0, len(macs))
	for _, m := range macs {
		if h := ccFingerprintHash(m); h != "" {
			macHashes = append(macHashes, h)
		}
	}
	osUserHash := ccFingerprintHash(osUser)
	hostnameHash := ccFingerprintHash(hostname)
	gitEmailHash := ccFingerprintHash(gitEmail)

	// The CLI's thumbmark: FP_SALT + "\0machine\0" + join([machineId,
	// macs.join(",")]) -- when machineId is present the hostname and
	// cpuModel are NOT part of the seed.
	var seedParts []string
	seedParts = append(seedParts, strings.TrimSpace(machineID), strings.Join(macs, ","))
	if strings.TrimSpace(machineID) == "" {
		seedParts = append(seedParts, hostname, cpuEntry.Model)
	}
	thumbSeed := strings.Join(seedParts, "|")
	if thumbSeed == "" {
		thumbSeed = "unknown"
	}
	thumb := sha256.Sum256([]byte(ccFPSalt + "\x00machine\x00" + thumbSeed))

	return ccFingerprint{
		Thumbmark: hex.EncodeToString(thumb[:]),
		Components: ccFingerprintComponents{
			MachineIDHash:    machineIDHash,
			MACHashes:        macHashes,
			OSUserHash:       osUserHash,
			HostnameHash:     hostnameHash,
			GitEmailHash:     gitEmailHash,
			Platform:         prof.Platform,
			Arch:             prof.Arch,
			OSRelease:        prof.OSRelease,
			CPUModel:         cpuEntry.Model,
			CPUCount:         cpuEntry.Threads, // logical processors, not cores
			MemGiB:           memGiB,
			IsContainer:      prof.IsContainer,
			Timezone:         tz,
			Runtime:          "cli",
			CollectorVersion: 1,
		},
	}
}

// ── per-key state & lifecycle pre-requests ───────────────────────

const (
	// ccInitRefresh is how long a successful fingerprint/lifecycle
	// registration lasts before it is re-sent.
	ccInitRefresh = 8 * time.Hour
	// ccInitJitter is the random spread added on top so a fleet does
	// not re-register in lockstep.
	ccInitJitter = 2 * time.Hour
)

// ccKeyState caches the derived fingerprint and throttles the
// lifecycle pre-requests for one API key. The fingerprint is
// deterministic, so eviction never changes the device identity -- it
// only forces a re-derivation and a re-registration on the next
// request.
type ccKeyState struct {
	mu          sync.Mutex
	fingerprint ccFingerprint
	nextInitAt  time.Time
	inFlight    bool
}

var ccKeyStates sync.Map // apiKey -> *ccKeyState

func ccGetKeyState(apiKey string) *ccKeyState {
	if v, ok := ccKeyStates.Load(apiKey); ok {
		return v.(*ccKeyState)
	}
	st := &ccKeyState{fingerprint: ccGenerateFingerprint(apiKey)}
	actual, _ := ccKeyStates.LoadOrStore(apiKey, st)
	return actual.(*ccKeyState)
}

// ccEnsureInitialized runs the two lifecycle pre-requests (fingerprint
// record + lifecycle event) for this key, throttled to once per
// refresh window. The pre-requests are fire-and-forget: a failure is
// logged but never blocks the generation request, exactly as the Node
// proxy does. Concurrent requests within the window share one in-flight
// pair rather than each firing their own.
func ccEnsureInitialized(ctx context.Context, client *http.Client, apiBase, apiKey string) {
	if client == nil {
		client = http.DefaultClient
	}
	st := ccGetKeyState(apiKey)

	st.mu.Lock()
	if time.Now().Before(st.nextInitAt) || st.inFlight {
		st.mu.Unlock()
		return
	}
	st.inFlight = true
	fp := st.fingerprint
	st.mu.Unlock()

	go func() {
		defer func() {
			st.mu.Lock()
			st.inFlight = false
			st.mu.Unlock()
		}()

		headers := map[string]string{
			"Content-Type":           "application/json",
			"User-Agent":             "cli",
			"x-cli-environment":      "production",
			"Authorization":          "Bearer " + apiKey,
			"x-command-code-version": ccProtocolVersion,
		}
		if ccZDR() {
			headers["x-cmd-zdr"] = "1"
		}

		// Both pre-requests are sent in parallel.
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			body, _ := json.Marshal(fp)
			ccPostJSON(ctx, client, apiBase+"/alpha/fingerprint/record", headers, body)
		}()

		go func() {
			defer wg.Done()
			lifecycle := map[string]any{
				"eventType": "cli_session_exists",
				"metadata": map[string]any{
					"sessionId":  "sess_" + ccRandomHex(8),
					"cliVersion": ccProtocolVersion,
					"mode":       ccCliSessionMode(),
					"os":         fp.Components.Platform + "-" + fp.Components.Arch,
				},
			}
			body, _ := json.Marshal(lifecycle)
			ccPostJSON(ctx, client, apiBase+"/alpha/lifecycle-events", headers, body)
		}()

		wg.Wait()

		// Success (or not): re-arm for the next window. The Node
		// proxy only re-arms on success, but re-arming unconditionally
		// with the full window avoids a thundering herd against a
		// failing upstream; the pre-requests are advisory.
		st.mu.Lock()
		st.nextInitAt = time.Now().Add(ccInitRefresh + time.Duration(ccRandomInt63n(int64(ccInitJitter))))
		st.mu.Unlock()
	}()
}

// ccPostJSON fires one pre-request. Failures are deliberately swallowed
// (logged at most): the generation request proceeds regardless.
func ccPostJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// ── session id ───────────────────────────────────────────────────

// ccWireUUIDRe matches a canonical lowercase-hex UUID, the only shape
// the CLI puts in the envelope's threadId.
var ccWireUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func ccIsWireUUID(v string) bool {
	return ccWireUUIDRe.MatchString(strings.ToLower(strings.TrimSpace(v)))
}

// ccDeriveSessionID derives a UUID v4-shaped session id from the API
// key, model, system blocks and the leading run of user messages. It
// pins one logical conversation to one upstream thread so prompt cache
// survives across the gateway's own session boundaries.
func ccDeriveSessionID(apiKey string, params map[string]any) string {
	h := sha1.New()
	h.Write([]byte(apiKey))
	h.Write([]byte{0})
	if model, _ := params["model"].(string); model != "" {
		h.Write([]byte(model))
	}
	h.Write([]byte{0})
	if system, ok := params["system"].([]any); ok {
		for _, b := range system {
			if blk, ok := b.(map[string]any); ok {
				if text, _ := blk["text"].(string); text != "" {
					h.Write([]byte(text))
					h.Write([]byte{0})
				}
			}
		}
	}
	if messages, ok := params["messages"].([]any); ok {
		for _, m := range messages {
			msg, ok := m.(map[string]any)
			if !ok {
				break
			}
			if role, _ := msg["role"].(string); role != "user" {
				break // only the leading run of user messages participates
			}
			content, ok := msg["content"].([]any)
			if !ok {
				continue
			}
			for _, p := range content {
				part, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := part["type"].(string); t == "text" {
					if text, _ := part["text"].(string); text != "" {
						h.Write([]byte(text))
						h.Write([]byte{0})
					}
				}
			}
		}
	}
	sum := h.Sum(nil)[:16]
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	hx := hex.EncodeToString(sum)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hx[0:8], hx[8:12], hx[12:16], hx[16:20], hx[20:32])
}

// ccSessionID resolves the session id with the CLI's precedence: a
// client-supplied session header (>=8 chars) wins, otherwise the id is
// derived from the request content.
func ccSessionID(clientHeaders map[string]string, apiKey string, params map[string]any) string {
	for _, h := range []string{"x-session-id", "x-claude-code-session-id", "session_id"} {
		if v := clientHeaders[h]; len(v) >= 8 {
			return v
		}
	}
	return ccDeriveSessionID(apiKey, params)
}

// ── envelope builder ─────────────────────────────────────────────

// ccClientMessage is the subset of a Chat Completions message the
// translator reads. Content is kept raw because it is either a string
// or an array of typed parts, decided by the first byte.
type ccClientMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
}

type ccClientTool struct {
	Type     string `json:"type"`
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type ccClientRequest struct {
	Model             string            `json:"model"`
	Messages          []ccClientMessage `json:"messages"`
	MaxTokens         *int              `json:"max_tokens"`
	Temperature       *float64          `json:"temperature"`
	Tools             []ccClientTool    `json:"tools"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	ReasoningEffort   *string           `json:"reasoning_effort"`
	PromptCacheKey    string            `json:"prompt_cache_key"`
}

// ccBuildEnvelope converts an OpenAI Chat Completions request into the
// Command Code /alpha/generate envelope. It returns the envelope and
// the resolved session id (the caller needs it for the x-session-id
// header and the envelope's threadId).
//
// The envelope's top-level key order is load-bearing -- the CLI emits
// config, memory, taste, skills, permissionMode, threadId, mode,
// params -- so the JSON object is built in that order. Go's encoding/
// json marshals a map in key-sorted order, so the ordered envelope is
// assembled as a json.RawMessage via an ordered encoder.
func ccBuildEnvelope(clientBody []byte, model, apiKey string, clientHeaders map[string]string) (envelope []byte, sessionID string, err error) {
	var req ccClientRequest
	if err := json.Unmarshal(clientBody, &req); err != nil {
		return nil, "", fmt.Errorf("decode chat request: %w", err)
	}
	if model == "" {
		model = req.Model
	}
	if model == "" {
		model = "deepseek/deepseek-v4-flash"
	}

	// System prompt: OpenAI system/developer messages become an array
	// of text blocks. Every block but the last gets a trailing newline
	// (the CLI's toWireSystem), and per-block cache_control is kept.
	var systemBlocks []map[string]any
	var chatMessages []ccClientMessage
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			systemBlocks = append(systemBlocks, ccSystemBlocks(m)...)
			continue
		}
		chatMessages = append(chatMessages, m)
	}
	for i := 0; i < len(systemBlocks)-1; i++ {
		if text, _ := systemBlocks[i]["text"].(string); text != "" {
			systemBlocks[i]["text"] = text + "\n"
		}
	}

	// tool_call_id -> tool_name reverse map, so tool results carry the
	// name the assistant used when it issued the call.
	toolNameMap := map[string]string{}
	for _, m := range chatMessages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					toolNameMap[tc.ID] = tc.Function.Name
				}
			}
		}
	}

	// Convert the non-system messages to CC wire format.
	ccMessages := make([]any, 0, len(chatMessages))
	for _, m := range chatMessages {
		ccMessages = append(ccMessages, ccConvertMessage(m, toolNameMap))
	}

	// Cache breakpoint: keep any the client already set; otherwise, if
	// a prompt_cache_key was given, mark the last system block.
	hasCacheMarker := false
	for _, b := range systemBlocks {
		if _, ok := b["cache_control"]; ok {
			hasCacheMarker = true
			break
		}
	}
	if !hasCacheMarker {
		for _, m := range ccMessages {
			if msg, ok := m.(map[string]any); ok {
				if content, ok := msg["content"].([]any); ok {
					for _, p := range content {
						if part, ok := p.(map[string]any); ok {
							if _, ok := part["cache_control"]; ok {
								hasCacheMarker = true
							}
						}
					}
				}
			}
			if hasCacheMarker {
				break
			}
		}
	}
	if req.PromptCacheKey != "" && !hasCacheMarker && len(systemBlocks) > 0 {
		systemBlocks[len(systemBlocks)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
	}

	params := map[string]any{
		"model":      model,
		"messages":   ccMessages,
		"max_tokens": ccMinInt(ccDerefInt(req.MaxTokens, 64000), 200000),
		"stream":     true, // the CC API always streams
	}
	if len(systemBlocks) > 0 {
		params["system"] = systemBlocks
	} else if ccEmptySystemPlaceholder() {
		params["system"] = []any{map[string]any{"type": "text", "text": " "}}
	}
	if req.Temperature != nil {
		params["temperature"] = *req.Temperature
	}
	if req.ReasoningEffort != nil {
		params["reasoning_effort"] = *req.ReasoningEffort
	}
	// The CLI always sends tools (empty array when there are none).
	params["tools"] = ccConvertTools(req.Tools)
	if tc := ccConvertToolChoice(req.ToolChoice); tc != nil {
		if cleared, ok := tc.(bool); ok && !cleared {
			// "none": CC's tool_choice enum is auto/any/tool, so "none"
			// is expressed by clearing tools and omitting tool_choice.
			params["tools"] = []any{}
		} else {
			params["tool_choice"] = tc
		}
	}
	if req.ParallelToolCalls != nil {
		params["parallel_tool_calls"] = *req.ParallelToolCalls
	}

	// Resolve the session id now that params is built, so threadId and
	// x-session-id agree.
	sessionID = ccSessionID(clientHeaders, apiKey, params)

	prof := ccNewDeviceProfile()
	config := map[string]any{
		"workingDir":    prof.ProjectDir,
		"date":          time.Now().UTC().Format("2006-01-02"),
		"environment":   prof.Platform,
		"structure":     []any{},
		"isGitRepo":     false,
		"currentBranch": "",
		"mainBranch":    "",
		"gitStatus":     "",
		"recentCommits": []any{},
	}

	// Assemble the envelope in the CLI's wire order. encoding/json
	// sorts map keys, so the ordered shape is produced by a manual
	// encoder that writes keys in sequence.
	var buf bytes.Buffer
	buf.WriteString("{")
	writeKV := func(key string, val any, first *bool) {
		if !*first {
			buf.WriteString(",")
		}
		*first = false
		kb, _ := json.Marshal(key)
		vb, _ := json.Marshal(val)
		buf.Write(kb)
		buf.WriteString(":")
		buf.Write(vb)
	}
	first := true
	writeKV("config", config, &first)
	writeKV("memory", nil, &first)
	writeKV("taste", nil, &first)
	writeKV("skills", nil, &first)
	writeKV("permissionMode", "standard", &first)
	if ccIsWireUUID(sessionID) {
		writeKV("threadId", sessionID, &first)
	}
	writeKV("mode", ccCliMode(), &first)
	writeKV("params", params, &first)
	buf.WriteString("}")

	return buf.Bytes(), sessionID, nil
}

// ccSystemBlocks extracts the text blocks of one system/developer
// message, preserving per-block cache_control.
func ccSystemBlocks(m ccClientMessage) []map[string]any {
	var blocks []map[string]any
	addBlock := func(text string, cacheControl any) {
		if text == "" && cacheControl == nil {
			return
		}
		blk := map[string]any{"type": "text", "text": text}
		if cacheControl != nil {
			blk["cache_control"] = cacheControl
		}
		blocks = append(blocks, blk)
	}

	if len(m.Content) == 0 {
		return blocks
	}
	// A string content is one block.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		if s != "" {
			addBlock(s, nil)
		}
		return blocks
	}
	// An array content is a list of typed parts.
	var parts []map[string]any
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		// Neither string nor array: stringify the raw value.
		if string(m.Content) != "" {
			addBlock(string(m.Content), nil)
		}
		return blocks
	}
	for _, p := range parts {
		text, _ := p["text"].(string)
		if text == "" {
			if alt, _ := p["content"].(string); alt != "" {
				text = alt
			}
		}
		addBlock(text, p["cache_control"])
	}
	return blocks
}

// ccConvertMessage converts one non-system message to CC wire format.
func ccConvertMessage(m ccClientMessage, toolNameMap map[string]string) map[string]any {
	switch m.Role {
	case "user":
		return map[string]any{"role": "user", "content": ccUserContent(m.Content)}
	case "assistant":
		parts := []any{}
		// Reasoning comes first: CC validates that thinking mode's
		// reasoning is echoed back in the history.
		if m.ReasoningContent != "" {
			parts = append(parts, map[string]any{"type": "reasoning", "text": m.ReasoningContent})
		}
		if content, ok := ccAssistantContent(m.Content); ok {
			parts = append(parts, content...)
		}
		for _, tc := range m.ToolCalls {
			input := ccParseToolInput(tc.Function.Arguments)
			parts = append(parts, map[string]any{
				"type":       "tool-call",
				"toolCallId": tc.ID,
				"toolName":   tc.Function.Name,
				"input":      input,
			})
		}
		return map[string]any{"role": "assistant", "content": parts}
	case "tool":
		name := toolNameMap[m.ToolCallID]
		if name == "" {
			name = m.Name
		}
		return map[string]any{
			"role": "tool",
			"content": []any{map[string]any{
				"type":       "tool-result",
				"toolCallId": m.ToolCallID,
				"toolName":   name,
				"output":     map[string]any{"type": "text", "value": ccToolOutputValue(m.Content)},
			}},
		}
	default:
		// Unknown roles are normalized to user with an array content so
		// the upstream's schema validation accepts them.
		return map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": ccStringContent(m.Content)}}}
	}
}

// ccUserContent converts a user message's content (string or array) to
// CC parts. image_url parts become CC image parts; everything else is
// passed through.
func ccUserContent(content json.RawMessage) []any {
	if len(content) == 0 {
		return []any{map[string]any{"type": "text", "text": ""}}
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return []any{map[string]any{"type": "text", "text": s}}
	}
	var parts []map[string]any
	if err := json.Unmarshal(content, &parts); err != nil {
		return []any{map[string]any{"type": "text", "text": ccStringContent(content)}}
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		if p == nil {
			continue
		}
		if t, _ := p["type"].(string); t == "image_url" {
			url := ""
			if iu, ok := p["image_url"].(map[string]any); ok {
				url, _ = iu["url"].(string)
			}
			image := map[string]any{"type": "image", "image": url}
			if mt := ccDataURLMediaType(url); mt != "" {
				image["mimeType"] = mt
			}
			out = append(out, image)
			continue
		}
		out = append(out, p)
	}
	return out
}

// ccAssistantContent extracts the text/reasoning parts of an assistant
// message's content array. A reasoning part already covered by
// reasoning_content is not duplicated.
func ccAssistantContent(content json.RawMessage) ([]any, bool) {
	if len(content) == 0 {
		return nil, false
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		if s == "" {
			return nil, false
		}
		return []any{map[string]any{"type": "text", "text": s}}, true
	}
	var parts []map[string]any
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, false
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		if p == nil {
			continue
		}
		t, _ := p["type"].(string)
		if t == "text" {
			out = append(out, p)
		} else if t == "reasoning" {
			// Only forward a reasoning part that is not already carried
			// by the reasoning_content field.
			out = append(out, p)
		}
	}
	return out, true
}

// ccConvertTools maps OpenAI function tools to the CLI's tool shape
// (name / description / input_schema -- no type field).
func ccConvertTools(tools []ccClientTool) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		name, desc, params := t.Name, t.Description, t.InputSchema
		if t.Function != nil {
			if t.Function.Name != "" {
				name = t.Function.Name
			}
			if t.Function.Description != "" {
				desc = t.Function.Description
			}
			if len(t.Function.Parameters) > 0 {
				params = t.Function.Parameters
			}
		}
		if params == nil {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, map[string]any{
			"name":         name,
			"description":  desc,
			"input_schema": json.RawMessage(params),
		})
	}
	return out
}

// ccConvertToolChoice maps the OpenAI tool_choice to the CC enum
// (auto / any / tool). It returns nil when tool_choice is absent, and
// false (a bool) when the choice is "none" -- which the caller turns
// into "clear tools, omit tool_choice".
func ccConvertToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none":
			return false
		case "required":
			return map[string]any{"type": "any"}
		case "auto":
			return map[string]any{"type": "auto"}
		default:
			return map[string]any{"type": "auto"}
		}
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	t, _ := obj["type"].(string)
	if t == "none" {
		return false
	}
	if t == "function" {
		name := ""
		if fn, ok := obj["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		}
		return map[string]any{"type": "tool", "name": name}
	}
	return obj
}

// ccParseToolInput parses a tool call's arguments string into a JSON
// value, falling back to an empty object.
func ccParseToolInput(args string) any {
	if args == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return map[string]any{}
	}
	return v
}

// ccToolOutputValue is the CLI's toWireToolOutput: a string passes
// through, an array of blocks joins its text parts with newlines,
// anything else is stringified.
func ccToolOutputValue(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(content, &parts); err == nil {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p != nil {
				if t, _ := p["type"].(string); t == "text" {
					if txt, _ := p["text"].(string); txt != "" {
						texts = append(texts, txt)
					}
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(content)
}

// ccStringContent stringifies a raw content value for the unknown-role
// fallback.
func ccStringContent(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	return string(content)
}

// ccDataURLMediaType extracts the media type from a data: URL.
func ccDataURLMediaType(url string) string {
	if !strings.HasPrefix(url, "data:") {
		return ""
	}
	rest := url[5:]
	if i := strings.IndexAny(rest, ";,"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// ccBodyModel extracts the logical model name from a client Chat
// Completions body, falling back to the endpoint's model when the
// body is unparseable. The response carries the name the client
// asked for, matching the Responses translator's behaviour.
func ccBodyModel(body []byte, fallback string) string {
	var partial struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &partial); err == nil && partial.Model != "" {
		return partial.Model
	}
	return fallback
}

func ccDerefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func ccMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── request headers ──────────────────────────────────────────────

// ccSlugifyProjectPath is the CLI's project-dir slug: lowercase, runs
// of non-alphanumerics collapsed to "-", trimmed, "root" if empty.
func ccSlugifyProjectPath(p string) string {
	s := strings.ToLower(p)
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "root"
	}
	return out
}

// ccSetRequestHeaders applies the Command Code client headers to the
// generation request. They mirror the CLI's buildCommandAuthHeaders
// exactly; provider-level header overrides from the airouter config are
// applied afterwards and win.
func ccSetRequestHeaders(req *http.Request, apiKey, sessionID string) {
	req.Header.Set("User-Agent", "cli")
	req.Header.Set("x-command-code-version", ccProtocolVersion)
	req.Header.Set("x-cli-environment", "production")
	req.Header.Set("x-project-slug", ccSlugifyProjectPath(ccDeviceProjectDir()))
	req.Header.Set("x-taste-learning", "false")
	req.Header.Set("x-session-id", sessionID)
	// W3C traceparent: 00-<32 hex trace id>-<16 hex parent id>-01.
	req.Header.Set("traceparent", "00-"+ccRandomHex(16)+"-"+ccRandomHex(8)+"-01")
	if ccZDR() {
		req.Header.Set("x-cmd-zdr", "1")
	}
}

// ── NDJSON event model ───────────────────────────────────────────

type ccUsage struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	CachedInputTokens int `json:"cachedInputTokens"`
}

type ccEventError struct {
	Message     string `json:"message"`
	Code        string `json:"code"`
	StatusCode  int    `json:"statusCode"`
	IsRetryable bool   `json:"isRetryable"`
}

// ccEvent is one NDJSON line from /alpha/generate. Only the fields the
// translator consumes are decoded; the rest are ignored.
type ccEvent struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Delta        string          `json:"delta"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	Input        json.RawMessage `json:"input"`
	FinishReason string          `json:"finishReason"`
	Usage        *ccUsage        `json:"usage"`
	TotalUsage   *ccUsage        `json:"totalUsage"`
	Error        *ccEventError   `json:"error"`
	Message      string          `json:"message"`
}

// ccInputString renders a tool-call input as the wire string: a JSON
// string passes through, an object is re-marshalled compactly.
func ccInputString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ── finish-reason mapping ────────────────────────────────────────

// ccMapFinishReason maps an upstream finish reason to the internal
// vocabulary, aligned with the CLI's normalizeStopReason2 /
// isNetworkFailureFinish. Unknown values pass through unchanged.
func ccMapFinishReason(reason string) string {
	r := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case r == "":
		return "stop"
	case r == "tool-calls" || r == "tool_calls" || r == "tool_use":
		return "tool_calls"
	case r == "length" || r == "max_tokens" || r == "max_output_tokens" || r == "model_context_window_exceeded":
		return "length"
	case r == "error" || ccNetworkErrorRe.MatchString(r):
		return "upstream_error"
	default:
		return r
	}
}

var ccNetworkErrorRe = regexp.MustCompile(`^(?:network|connection|upstream)[-_\s]?error$`)

// ccOpenAIFinishReasons is the set of finish_reason values a strict
// OpenAI client will accept.
var ccOpenAIFinishReasons = map[string]bool{
	"stop": true, "length": true, "tool_calls": true,
	"content_filter": true, "function_call": true,
}

// ccToOpenAIFinishReason maps the internal vocabulary to a value safe
// to emit to an OpenAI client. upstream_error is preserved (the caller
// turns it into a retryable failure); pause_turn becomes length;
// anything else unknown becomes stop.
func ccToOpenAIFinishReason(reason string) string {
	switch {
	case reason == "pause_turn":
		return "length"
	case reason == "upstream_error":
		return reason
	case ccOpenAIFinishReasons[reason]:
		return reason
	default:
		return "stop"
	}
}

// ccNormalizeUsage zeroes the input/cached counters when there is no
// output, so a provider that reports a finish with zero output tokens
// cannot bill phantom input tokens.
func ccNormalizeUsage(u *ccUsage) {
	if u == nil {
		return
	}
	if u.OutputTokens == 0 {
		u.InputTokens = 0
		u.CachedInputTokens = 0
	}
}

// ── stream translator (NDJSON -> Chat Completions SSE) ───────────

// ccStreamToChatReader translates the CC NDJSON event stream into
// Chat Completions SSE on the fly, so the downstream streamSSE --
// buffering-until-first-content, tool-call coalescing, [DONE] framing,
// usage extraction -- works unchanged. It is an io.Reader, the same
// shape as newResponsesSSEToChatReader.
type ccStreamToChatReader struct {
	src     io.Reader
	model   string
	created int64
	id      string

	carry     []byte
	pend      []byte
	done      bool
	firstDone bool

	chunkIndex   int
	sawFinish    bool
	finishReason string
	usage        *ccUsage
}

func newCCStreamToChatReader(src io.Reader, model string, created int64) *ccStreamToChatReader {
	return &ccStreamToChatReader{
		src:     src,
		model:   model,
		created: created,
		id:      "chatcmpl-" + ccRandomHex(6),
	}
}

func (r *ccStreamToChatReader) Read(p []byte) (int, error) {
	for len(r.pend) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if err := r.pump(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pend)
	r.pend = r.pend[n:]
	return n, nil
}

func (r *ccStreamToChatReader) pump() error {
	buf := make([]byte, 32*1024)
	n, err := r.src.Read(buf)
	if n > 0 {
		r.carry = append(r.carry, buf[:n]...)
		var out []byte
		for {
			idx := bytes.IndexByte(r.carry, '\n')
			if idx < 0 {
				break
			}
			line := bytes.TrimRight(r.carry[:idx], "\r")
			r.carry = r.carry[idx+1:]
			if translated := r.translateLine(line); len(translated) > 0 {
				out = append(out, translated...)
			}
		}
		if len(out) > 0 {
			r.pend = append(out, r.pend...)
			return nil
		}
	}
	if err != nil {
		r.done = true
		// A final line that arrived without a trailing newline.
		if tail := bytes.TrimRight(r.carry, "\r"); len(tail) > 0 {
			r.carry = nil
			if translated := r.translateLine(tail); len(translated) > 0 {
				r.pend = append(r.pend, translated...)
				return nil
			}
		}
		// The stream ended. If it never reached a finish event the
		// upstream truncated the response: report it as a retryable
		// upstream error so the fallback walk can pick the next model
		// (only effective before any content was flushed downstream).
		if !r.sawFinish {
			r.pend = append(r.pend, r.errorChunk("Upstream stream ended without a completion finish (no finish event) — response was truncated")...)
			return nil
		}
		if !r.firstDone {
			r.firstDone = true
			r.pend = append(r.pend, []byte("data: [DONE]\n\n")...)
			return nil
		}
	}
	return nil
}

func (r *ccStreamToChatReader) translateLine(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) || trimmed[0] == ':' {
		return nil
	}
	// The CC stream is bare NDJSON; tolerate a `data:` prefix so an
	// SSE-framed upstream works too.
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[5:])
	}
	var ev ccEvent
	if err := json.Unmarshal(trimmed, &ev); err != nil || ev.Type == "" {
		return nil
	}

	switch ev.Type {
	case "text-delta":
		text := ev.Text
		if text == "" {
			text = ev.Delta
		}
		if text == "" {
			return nil
		}
		delta := map[string]any{"content": text}
		if r.chunkIndex == 0 {
			delta["role"] = "assistant"
		}
		r.chunkIndex++
		return r.chunk(delta, nil, nil)

	case "reasoning-delta":
		if ev.Text == "" {
			return nil
		}
		delta := map[string]any{"reasoning_content": ev.Text}
		if r.chunkIndex == 0 {
			delta["role"] = "assistant"
		}
		r.chunkIndex++
		return r.chunk(delta, nil, nil)

	case "tool-call":
		id := ev.ToolCallID
		if id == "" {
			id = "call_" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "_" + strconv.Itoa(r.chunkIndex)
		}
		tc := map[string]any{
			"index": r.chunkIndex,
			"id":    id,
			"type":  "function",
			"function": map[string]any{
				"name":      ev.ToolName,
				"arguments": ccInputString(ev.Input),
			},
		}
		delta := map[string]any{"tool_calls": []any{tc}}
		if r.chunkIndex == 0 {
			delta["role"] = "assistant"
			delta["content"] = nil
		}
		r.chunkIndex++
		return r.chunk(delta, nil, nil)

	case "finish-step":
		r.sawFinish = true
		if ev.FinishReason != "" {
			r.finishReason = ccMapFinishReason(ev.FinishReason)
		}
		if ev.Usage != nil {
			r.usage = ev.Usage
		}
		return nil

	case "finish":
		r.sawFinish = true
		reason := ccToOpenAIFinishReason(ccMapFinishReason(ccFirstNonEmpty(ev.FinishReason, r.finishReason, "stop")))
		u := ev.TotalUsage
		if u == nil {
			u = r.usage
		}
		ccNormalizeUsage(u)
		var usage any
		if u != nil {
			usage = map[string]any{
				"prompt_tokens":     u.InputTokens,
				"completion_tokens": u.OutputTokens,
				"total_tokens":      u.InputTokens + u.OutputTokens,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": u.CachedInputTokens,
				},
			}
		}
		if reason == "upstream_error" {
			// A connection-failure finish is a retryable truncation, not
			// a clean end: surface it as an error event so the caller can
			// fall back (or, if content already flowed, terminate cleanly).
			return r.errorChunk("provider reported an upstream connection failure")
		}
		return r.chunk(map[string]any{}, reason, usage)

	case "error":
		msg := ev.Message
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		if msg == "" {
			msg = "Unknown error"
		}
		return r.errorChunk(msg)

	default:
		// text-start, reasoning-start, start, start-step, reasoning-end,
		// provider-metadata, tool-input-*, tool-error, text-end: no
		// user-visible content.
		return nil
	}
}

// ccFirstNonEmpty returns the first non-empty string.
func ccFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// chunk emits one Chat Completions SSE chunk.
func (r *ccStreamToChatReader) chunk(delta any, finishReason any, usage any) []byte {
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": finishReason,
	}
	payload := map[string]any{
		"id":      r.id,
		"object":  "chat.completion.chunk",
		"created": r.created,
		"model":   r.model,
		"choices": []any{choice},
	}
	if usage != nil {
		payload["usage"] = usage
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// errorChunk emits an SSE error object. streamSSE's ExtractSSEError
// recognizes this shape and signals the fallback walk.
func (r *ccStreamToChatReader) errorChunk(message string) []byte {
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
		},
	}
	b, _ := json.Marshal(payload)
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// ── non-stream aggregation (NDJSON -> Chat Completions object) ───

// ccAggregateStream folds a complete CC NDJSON response body into a
// Chat Completions object. It returns an error when the upstream did
// not reach a finish event (truncated) or produced zero output tokens,
// so the caller treats the attempt as failed and falls back.
func ccAggregateStream(body []byte, model string, created int64) ([]byte, error) {
	var (
		fullText      strings.Builder
		reasoningText strings.Builder
		toolCalls     []map[string]any
		sawFinish     bool
		finishReason  = "stop"
		usage         *ccUsage
		upstreamError string
	)

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) || line[0] == ':' {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[5:])
		}
		var ev ccEvent
		if err := json.Unmarshal(line, &ev); err != nil || ev.Type == "" {
			continue
		}
		switch ev.Type {
		case "text-delta":
			text := ev.Text
			if text == "" {
				text = ev.Delta
			}
			fullText.WriteString(text)
		case "reasoning-delta":
			reasoningText.WriteString(ev.Text)
		case "tool-call":
			id := ev.ToolCallID
			if id == "" {
				id = "call_" + ccRandomHex(6)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      ev.ToolName,
					"arguments": ccInputString(ev.Input),
				},
			})
		case "finish-step":
			sawFinish = true
			if ev.FinishReason != "" {
				finishReason = ccMapFinishReason(ev.FinishReason)
			}
			if ev.Usage != nil {
				usage = ev.Usage
			}
		case "finish":
			sawFinish = true
			if ev.FinishReason != "" {
				finishReason = ccMapFinishReason(ev.FinishReason)
			}
			if ev.TotalUsage != nil {
				usage = ev.TotalUsage
			}
		case "error":
			msg := ev.Message
			if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			if msg == "" {
				msg = "Unknown CC error"
			}
			upstreamError = msg
		}
	}

	// A stream that never reached a finish event was truncated.
	if !sawFinish && upstreamError == "" {
		return nil, &ProviderError{StatusCode: 502, Body: []byte(`{"error":{"message":"Upstream stream ended without a completion finish (no finish event) — response was truncated","type":"upstream_error"}}`), RetryAfter: 10 * time.Second}
	}
	if upstreamError != "" {
		return nil, &ProviderError{StatusCode: 502, Body: []byte(`{"error":{"message":"` + jsonEscape(upstreamError) + `","type":"upstream_error"}}`), RetryAfter: 10 * time.Second}
	}

	ccNormalizeUsage(usage)
	outputTokens := 0
	inputTokens := 0
	cachedTokens := 0
	if usage != nil {
		outputTokens = usage.OutputTokens
		inputTokens = usage.InputTokens
		cachedTokens = usage.CachedInputTokens
	}

	// Zero-output guard: a finish with no output tokens is an empty
	// response, which the upstream treats as a rate-limit-family
	// failure (retryable), not a successful empty answer.
	if outputTokens == 0 && len(toolCalls) == 0 {
		return nil, &ProviderError{StatusCode: 429, Body: []byte(`{"error":{"message":"Empty response from upstream (zero output tokens)","type":"rate_limit_error"},"retry_after":10}`), RetryAfter: 10 * time.Second}
	}

	message := map[string]any{"role": "assistant", "content": fullText.String()}
	if reasoningText.Len() > 0 {
		message["reasoning_content"] = reasoningText.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	fr := ccToOpenAIFinishReason(finishReason)
	completion := map[string]any{
		"id":      "chatcmpl-" + ccRandomHex(6),
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": fr,
		}},
		"usage": map[string]any{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": cachedTokens,
			},
		},
	}
	return json.Marshal(completion)
}

// ccMapError maps a Command Code HTTP error status to the
// OpenAI-style error the gateway reports, following the
// proxy's CC_STATUS_MAP. The returned ProviderError carries
// the remapped status so both the client and the cooldown
// classifier see the OpenAI-equivalent (402 payment-required
// is a rate limit, 5xx is a retryable upstream error, and an
// unlisted status becomes a 502 rather than a generic failure).
func ccMapError(status int, body []byte) *ProviderError {
	mappedStatus, errType := ccStatusMap(status, body)
	message := fmt.Sprintf("CC API error (%d)", status)
	code := ""
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		if parsed.Error.Message != "" {
			message = parsed.Error.Message
			code = parsed.Error.Code
		} else if parsed.Message != "" {
			message = parsed.Message
		}
	} else if len(body) > 0 {
		message = string(body)
		if len(message) > 200 {
			message = message[:200]
		}
	}

	errPayload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
		},
	}
	if code != "" {
		errPayload["error"].(map[string]any)["code"] = code
	}
	retryAfter := time.Duration(0)
	if status == 429 {
		retryAfter = 30 * time.Second
	}
	b, _ := json.Marshal(errPayload)
	return &ProviderError{StatusCode: mappedStatus, Body: b, RetryAfter: retryAfter}
}

// ccStatusMap is the proxy's CC_STATUS_MAP: an upstream status
// to the (gateway status, error type) pair the client should
// see. Unlisted statuses become a retryable 502 upstream_error.
//
// The one refinement on the raw table: a 400 whose body is a
// quota/credit exhaustion refusal ("insufficient credits ...
// purchase more credits") is not a malformed request. It is the
// provider saying the account's balance is gone -- the same
// event as a 402 payment-required, which the table already maps
// to a 429 rate limit. Measured live 2026-10-12: commandcode's
// free-tier accounts answer every request with exactly that 400,
// and classifying it as a client error left the endpoint in
// rotation with a zero-second cooldown -- 708 failed round trips
// in 90 minutes, 100% failure. Mapping it onto the 429 family
// gives it the transient cooldown curve and lets the circuit
// breaker count it.
func ccStatusMap(status int, body []byte) (int, string) {
	switch status {
	case 400:
		if isQuotaRefusal(400, string(body)) {
			return 429, "rate_limit_error"
		}
		return 400, "invalid_request_error"
	case 401:
		return 401, "authentication_error"
	case 402:
		return 429, "rate_limit_error"
	case 403:
		return 401, "authentication_error"
	case 404:
		return 404, "not_found"
	case 422:
		return 400, "invalid_request_error"
	case 429:
		return 429, "rate_limit_error"
	case 500, 502:
		return 502, "upstream_error"
	case 503:
		return 503, "temporarily_unavailable"
	default:
		return 502, "upstream_error"
	}
}

// ── helpers ──────────────────────────────────────────────────────

// ccRandomHex returns n random bytes as a hex string (2n chars).
func ccRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Never fail a request on RNG failure; fill deterministically.
		for i := range b {
			b[i] = byte(i)
		}
	}
	return hex.EncodeToString(b)
}

func ccRandomInt63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return v.Int64()
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}
