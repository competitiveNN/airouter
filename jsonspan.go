package main

import (
	"bytes"
	"encoding/json"
	"hash"
	"sort"
)

// Top-level JSON object scanning.
//
// The gateway rewrites request bodies constantly: every fallback attempt
// substitutes the endpoint's model name, every request may get a default
// max_tokens, and every mid-stream failover appends the partial assistant
// message. All of that used to be done by decoding the body into
// map[string]json.RawMessage and re-marshaling the map, which copies the
// whole conversation twice per rewrite. On a 500 KB body that is about a
// megabyte of garbage per rewrite, and a long fallback chain pays it once
// per endpoint it tries.
//
// The helpers here locate the byte span of a top-level member inside the raw
// body, so a rewrite can splice the new bytes into place and leave every
// other byte exactly as the client sent it.
//
// The scanner understands exactly one shape -- a JSON object with string keys
// -- and reports failure on anything else, so a caller can fall back to
// encoding/json instead of guessing.

// jsonSpan is the byte range of a top-level member's value inside a body.
type jsonSpan struct {
	Key   string
	Start int // offset of the value's first byte
	End   int // offset just past the value's last byte
}

// jsonObject is the result of scanning a JSON object: the spans of its
// members, in document order, and the offset of its closing brace.
type jsonObject struct {
	Spans []jsonSpan
	End   int // offset of the closing '}'
}

// scanJSONObject scans the JSON object at the start of body. The object must
// be the whole document: a rewrite that spliced into one object of a
// multi-value body would forward something the upstream cannot parse as a
// request.
func scanJSONObject(body []byte) (jsonObject, bool) {
	i := skipJSONSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return jsonObject{}, false
	}
	obj, ok := scanJSONObjectAt(body, i)
	if !ok {
		return jsonObject{}, false
	}
	if skipJSONSpace(body, obj.End+1) != len(body) {
		return jsonObject{}, false
	}
	return obj, true
}

// scanJSONObjectAt scans the JSON object that starts at body[i]. It does not
// check that the object ends the document: callers that need that use
// scanJSONObject, which does.
func scanJSONObjectAt(body []byte, i int) (jsonObject, bool) {
	if i >= len(body) || body[i] != '{' {
		return jsonObject{}, false
	}
	obj := jsonObject{}
	i++
object:
	for {
		i = skipJSONSpace(body, i)
		if i >= len(body) {
			return jsonObject{}, false
		}
		if body[i] == '}' {
			obj.End = i
			i++
			break
		}
		if body[i] != '"' {
			return jsonObject{}, false
		}
		keyEnd, ok := scanJSONString(body, i)
		if !ok {
			return jsonObject{}, false
		}
		key, ok := jsonKeyName(body[i+1 : keyEnd-1])
		if !ok {
			return jsonObject{}, false
		}
		i = skipJSONSpace(body, keyEnd)
		if i >= len(body) || body[i] != ':' {
			return jsonObject{}, false
		}
		i = skipJSONSpace(body, i+1)
		valEnd, ok := scanJSONValue(body, i)
		if !ok {
			return jsonObject{}, false
		}
		obj.Spans = append(obj.Spans, jsonSpan{Key: key, Start: i, End: valEnd})
		i = skipJSONSpace(body, valEnd)
		if i >= len(body) {
			return jsonObject{}, false
		}
		switch body[i] {
		case ',':
			i = skipJSONSpace(body, i+1)
			if i < len(body) && body[i] == '}' {
				// A trailing comma is not JSON.
				return jsonObject{}, false
			}
			continue
		case '}':
			obj.End = i
			break object
		default:
			return jsonObject{}, false
		}
	}
	return obj, true
}

// scanJSONValue returns the offset just past the JSON value that starts at
// body[i].
func scanJSONValue(body []byte, i int) (int, bool) {
	if i >= len(body) {
		return 0, false
	}
	switch body[i] {
	case '"':
		return scanJSONString(body, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(body); {
			switch body[j] {
			case '"':
				end, ok := scanJSONString(body, j)
				if !ok {
					return 0, false
				}
				j = end
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, true
				}
			}
			j++
		}
		return 0, false
	default:
		// A number, true, false or null: it runs to the next structural
		// character.
		if c := body[i]; c != '-' && (c < '0' || c > '9') && c != 't' && c != 'f' && c != 'n' {
			return 0, false
		}
		j := i
		for j < len(body) {
			switch body[j] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return j, true
			}
			j++
		}
		return j, true
	}
}

// scanJSONString returns the offset just past the closing quote of the JSON
// string that starts at body[i].
func scanJSONString(body []byte, i int) (int, bool) {
	if i >= len(body) || body[i] != '"' {
		return 0, false
	}
	for j := i + 1; j < len(body); {
		switch body[j] {
		case '\\':
			j += 2
		case '"':
			return j + 1, true
		default:
			if body[j] < 0x20 {
				return 0, false
			}
			j++
		}
	}
	return 0, false
}

// skipJSONSpace returns the offset of the first byte at or after i that is
// not JSON whitespace.
func skipJSONSpace(body []byte, i int) int {
	for i < len(body) {
		switch body[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// jsonKeyName decodes an object key. The fast path is a key with no escape
// sequence in it; one with escapes is rare enough to decode properly.
func jsonKeyName(raw []byte) (string, bool) {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw), true
	}
	var key string
	if err := json.Unmarshal(raw, &key); err != nil {
		return "", false
	}
	return key, true
}

// findJSONMember returns the span of the top-level member named key. A
// duplicated key resolves to the last one, which is what decoding the body
// into a map did.
func findJSONMember(obj jsonObject, key string) (jsonSpan, bool) {
	var found jsonSpan
	ok := false
	for _, s := range obj.Spans {
		if s.Key == key {
			found = s
			ok = true
		}
	}
	return found, ok
}

// replaceJSONValue returns body with the value at span replaced by value.
func replaceJSONValue(body []byte, span jsonSpan, value []byte) []byte {
	out := make([]byte, 0, len(body)-(span.End-span.Start)+len(value))
	out = append(out, body[:span.Start]...)
	out = append(out, value...)
	out = append(out, body[span.End:]...)
	return out
}

// insertJSONMember adds a top-level member to the JSON object in body. The
// member lands after the last existing one and every existing byte stays
// where the client put it.
func insertJSONMember(body []byte, objEnd int, key string, value []byte) []byte {
	member := make([]byte, 0, len(key)+len(value)+4)
	member = append(member, '"')
	member = append(member, key...)
	member = append(member, '"', ':')
	member = append(member, value...)

	// Drop the whitespace that separated the last member from the closing
	// brace, so the new member does not inherit it.
	cut := objEnd
	for cut > 0 {
		switch body[cut-1] {
		case ' ', '\t', '\n', '\r':
			cut--
			continue
		}
		break
	}
	out := make([]byte, 0, len(body)+len(member)+1)
	out = append(out, body[:cut]...)
	if cut > 0 && body[cut-1] != '{' {
		out = append(out, ',')
	}
	out = append(out, member...)
	out = append(out, body[cut:]...)
	return out
}

// insertJSONMemberReusing adds a top-level member without allocating a copy of
// the body when the buffer has spare capacity. Request bodies read by the
// gateway are sized from Content-Length plus a small headroom, so the one
// member the gateway injects (max_tokens) lands in that headroom instead of
// forcing every request to be copied.
func insertJSONMemberReusing(body []byte, objEnd int, key string, value []byte) []byte {
	member := make([]byte, 0, len(key)+len(value)+4)
	member = append(member, '"')
	member = append(member, key...)
	member = append(member, '"', ':')
	member = append(member, value...)

	cut := objEnd
	for cut > 0 {
		switch body[cut-1] {
		case ' ', '\t', '\n', '\r':
			cut--
			continue
		}
		break
	}
	sep := 0
	if cut > 0 && body[cut-1] != '{' {
		sep = 1
	}
	need := sep + len(member)
	if cap(body)-len(body) < need {
		return insertJSONMember(body, objEnd, key, value)
	}
	// Shift the closing brace (and any trailing whitespace) right by need, then
	// write the separator and the member into the gap. copy handles the
	// overlapping move.
	tail := body[cut:]
	oldLen := len(body)
	body = body[:oldLen+need]
	copy(body[cut+need:], tail)
	if sep > 0 {
		body[cut] = ','
	}
	copy(body[cut+sep:], member)
	return body
}

// jsonStringValue decodes the string at span.
func jsonStringValue(body []byte, span jsonSpan, found bool) (string, bool) {
	if !found || span.Start >= len(body) || body[span.Start] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(body[span.Start:span.End], &s); err != nil {
		return "", false
	}
	return s, true
}

// hashJSONValue writes the JSON value to h with insignificant whitespace
// removed. Whitespace inside a string is content, not formatting, so it is
// kept.
func hashJSONValue(h hash.Hash, raw []byte) {
	start := 0
	inString := false
	escaped := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\r':
			h.Write(raw[start:i])
			start = i + 1
		}
	}
	h.Write(raw[start:])
}

// Editing spans.

// jsonEdit replaces body[Start:End] with Replacement. An empty range
// (Start == End) inserts instead.
type jsonEdit struct {
	Start       int
	End         int
	Replacement []byte
}

// applyJSONEdits returns body with every edit applied. Edits may be given in
// any order; they must not overlap. Anything out of range or overlapping is
// rejected wholesale and the original body is returned, so a malformed edit
// can never produce malformed JSON.
func applyJSONEdits(body []byte, edits []jsonEdit) []byte {
	if len(edits) == 0 {
		return body
	}
	sorted := make([]jsonEdit, len(edits))
	copy(sorted, edits)
	for _, e := range sorted {
		if e.Start < 0 || e.End < e.Start || e.End > len(body) {
			return body
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	size := len(body)
	for i, e := range sorted {
		if i > 0 && e.Start < sorted[i-1].End {
			return body
		}
		size += len(e.Replacement) - (e.End - e.Start)
	}
	if size < 0 {
		return body
	}
	out := make([]byte, 0, size)
	at := 0
	for _, e := range sorted {
		out = append(out, body[at:e.Start]...)
		out = append(out, e.Replacement...)
		at = e.End
	}
	return append(out, body[at:]...)
}

// jsonLiteralValue returns the raw value at span with surrounding whitespace
// removed. It is the byte form of a json.RawMessage member.
func jsonLiteralValue(body []byte, span jsonSpan, found bool) []byte {
	if !found || span.Start < 0 || span.End > len(body) || span.Start > span.End {
		return nil
	}
	return bytes.TrimSpace(body[span.Start:span.End])
}

// jsonObjectInsertion returns the offset at which another member can be
// written into obj and the separator that must precede it ("," unless the
// object is empty). The whitespace between the last member and the closing
// brace is not disturbed.
func jsonObjectInsertion(body []byte, obj jsonObject) (int, string) {
	cut := obj.End
	for cut > 0 {
		switch body[cut-1] {
		case ' ', '\t', '\n', '\r':
			cut--
			continue
		}
		break
	}
	if len(obj.Spans) == 0 {
		return cut, ""
	}
	return cut, ","
}

// jsonArrayElements returns the spans of the elements of the array at span.
// It fails when the value is not an array.
func jsonArrayElements(body []byte, span jsonSpan) ([]jsonSpan, bool) {
	if span.Start < 0 || span.End > len(body) || span.Start >= span.End || body[span.Start] != '[' {
		return nil, false
	}
	end := span.End - 1
	if body[end] != ']' {
		return nil, false
	}
	i := skipJSONSpace(body, span.Start+1)
	var elems []jsonSpan
	if i == end {
		return elems, true
	}
	for {
		stop, ok := scanJSONValue(body, i)
		if !ok || stop > end {
			return nil, false
		}
		elems = append(elems, jsonSpan{Start: i, End: stop})
		i = skipJSONSpace(body, stop)
		if i == end {
			return elems, true
		}
		if i > end || body[i] != ',' {
			return nil, false
		}
		i = skipJSONSpace(body, i+1)
	}
}

// jsonArrayInsertion returns the offset at which another element can be
// written into the array at span and the separator that must precede it.
func jsonArrayInsertion(body []byte, span jsonSpan, elems []jsonSpan) (int, string, bool) {
	if span.Start < 0 || span.End > len(body) || span.Start >= span.End || body[span.Start] != '[' {
		return 0, "", false
	}
	end := span.End - 1
	if body[end] != ']' {
		return 0, "", false
	}
	for end > span.Start+1 {
		switch body[end-1] {
		case ' ', '\t', '\n', '\r':
			end--
			continue
		}
		break
	}
	if len(elems) == 0 {
		return end, "", true
	}
	return end, ",", true
}

// encodeJSONArray joins already-encoded values into a JSON array.
func encodeJSONArray(values [][]byte) []byte {
	size := 2
	for _, v := range values {
		size += len(v) + 1
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for i, v := range values {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, v...)
	}
	return append(out, ']')
}

// jsonObjectAt returns the object that occupies the whole of span. It fails
// when the value is not exactly one object (a scalar, or an object followed by
// something else).
func jsonObjectAt(body []byte, span jsonSpan) (jsonObject, bool) {
	obj, ok := scanJSONObjectAt(body, span.Start)
	if !ok || obj.End+1 != span.End {
		return jsonObject{}, false
	}
	return obj, true
}

// jsonToolName returns the name a Chat Completions tool object declares under
// function.name, or the flat name a Responses-style tool carries, or "".
func jsonToolName(body []byte, span jsonSpan) string {
	obj, ok := jsonObjectAt(body, span)
	if !ok {
		return ""
	}
	if fnSpan, found := findJSONMember(obj, "function"); found {
		if fn, ok := jsonObjectAt(body, fnSpan); ok {
			if nameSpan, found := findJSONMember(fn, "name"); found {
				if name, ok := jsonStringValue(body, nameSpan, found); ok {
					return name
				}
			}
		}
	}
	if nameSpan, found := findJSONMember(obj, "name"); found {
		if name, ok := jsonStringValue(body, nameSpan, found); ok {
			return name
		}
	}
	return ""
}
