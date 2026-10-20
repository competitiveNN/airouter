package main

// Extractor for spys.one list pages (anonymous-proxy-list and socks-proxy-list).
//
// The IP is plaintext in each row, but the PORT is not: it is assembled by an
// inline JavaScript that bitwise-XORs named variables, e.g.
//
//	112.169.99.71<script ...>document.write("...:..."+ (j0c3q7^t0p6) + ...)</script>
//
// A script block at the top of the page defines those names, and crucially some
// of the definitions are themselves XOR expressions: `j0c3q7=8^t0p6`. The trap
// is that resolving a name to the value that definition stores and then XORing
// once is WRONG. `(j0c3q7 ^ t0p6)` with the expansions on both sides is
// `(8^t0p6) ^ t0p6`, which cancels `t0p6` and leaves `8` -- one decimal digit
// of the port. So the operands must be expanded recursively (substituting the
// definition of every variable on the way down) before the final XOR. Every
// name in a row resolves to a single digit, and the digits concatenated in
// appearance order are the port.
//
// Nothing here may panic: these pages are third-party and may be reformatted,
// truncated, or replaced with a challenge page at any moment. Any row we cannot
// decode (undefined variable, malformed expression, out-of-range port) is
// skipped so the rest of the page still yields proxies.

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// spysOneScriptRe isolates the contents of each <script> element so the
// variable table can be recognised by shape rather than by position. Matching
// the assignments over the whole body instead would also pick up unrelated
// `name=number;` fragments from inline analytics/iframe scripts (e.g.
// `a.height=1;`), polluting the table with names that are not spys variables.
var spysOneScriptRe = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)

// spysOneAssignRe is one entry of the variable table: either a literal
// (`t0p6=5668`) or a single XOR term (`j0c3q7=8^t0p6`). A block whose body is
// entirely made of these is the table; the analytics/iframe blocks are not.
var spysOneAssignRe = regexp.MustCompile(`^[a-z0-9]+=(?:\d+|\d+\^[a-z0-9]+)$`)

// spysOneVarRefRe splits a table value that references another variable into
// its leading literal and the referenced name.
var spysOneVarRefRe = regexp.MustCompile(`^(\d+)\^([a-z0-9]+)$`)

// spysOneRowRe matches one proxy row: the plaintext IP immediately followed by
// the script that writes its port. The write body is captured non-greedily up
// to the closing `)</script>`, then the `(a^b)` terms are pulled out of it
// separately -- one combined regex to also skip the string literal in the
// write args backtracks badly under RE2.
var spysOneRowRe = regexp.MustCompile(
	`(?s)(\d{1,3}(?:\.\d{1,3}){3})<script type="text/javascript">document\.write\((.*?)\)\s*</script>`)

// spysOneTermRe is a single `(name ^ name)` port term.
var spysOneTermRe = regexp.MustCompile(`\(([a-z0-9]+)\^([a-z0-9]+)\)`)

// spysOneDateRe matches the row's "last check" cell: a dd-Mon-yyyy date in a
// spy14 font followed by HH:MM. The IP cell uses the same font class, so the
// month/year part is what keeps this from matching an address.
var spysOneDateRe = regexp.MustCompile(
	`(?s)<font class=spy14>(\d{1,2})-([a-z]{3})-(\d{4})</font>\s+(\d{1,2}):(\d{2})`)

// spysOneMonths maps the lowercase three-letter month names the page emits.
var spysOneMonths = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

// extractorSpysOne returns the extractor bound to one spys.one list page. The
// registry supplies the protocol (the page's rows do not state it per entry
// reliably), so it is captured rather than read from the body.
func extractorSpysOne(protocol string) func(body []byte) sourceExtract {
	return func(body []byte) sourceExtract {
		vars := parseSpysOneVars(body)

		rows := spysOneRowRe.FindAllSubmatch(body, -1)
		out := make([]*ProxyInfo, 0, len(rows))
		seen := make(map[string]bool, len(rows))
		for _, m := range rows {
			ip := string(m[1])
			port, ok := spysOneDecodePort(vars, string(m[2]))
			if !ok {
				continue
			}
			key := ip + ":" + port
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, &ProxyInfo{
				URL:      proxyURLFor(protocol, ip, port),
				Protocol: protocol,
				IP:       ip,
				Port:     port,
			})
		}

		// AsOf is the newest check time on the page. It is read across the
		// whole body rather than per row because we only need the freshest
		// value, and the date cell is not adjacent to the IP in the markup.
		var asOf time.Time
		for _, m := range spysOneDateRe.FindAllSubmatch(body, -1) {
			if t, ok := spysOneParseDate(m); ok && t.After(asOf) {
				asOf = t
			}
		}

		return sourceExtract{Proxies: out, AsOf: asOf}
	}
}

// parseSpysOneVars returns the page's variable table. A block is accepted only
// when every one of its `;`-separated entries is a plain literal or a single
// `N^name` term; anything else (analytics, the Cloudflare/iframe script) is
// left out. A body with no such block yields an empty table, and every row then
// skips as undecodable.
func parseSpysOneVars(body []byte) map[string]string {
	vars := make(map[string]string)
	for _, m := range spysOneScriptRe.FindAllSubmatch(body, -1) {
		var parts []string
		for _, p := range strings.Split(string(m[1]), ";") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) < 2 {
			continue
		}
		ok := true
		for _, p := range parts {
			if !spysOneAssignRe.MatchString(p) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		for _, p := range parts {
			name, expr, _ := strings.Cut(p, "=")
			vars[name] = expr
		}
	}
	return vars
}

// spysOneDecodePort turns a row's port expression into the port string, or
// ok=false if any operand is missing or the result is not a usable port. The
// expression is a run of `(a^b)` terms; each term is resolved by expanding both
// operands recursively (see the file comment) and XORing the expanded values.
// The term results are decimal-concatenated, then validated as a port.
func spysOneDecodePort(vars map[string]string, expr string) (string, bool) {
	terms := spysOneTermRe.FindAllStringSubmatch(expr, -1)
	if len(terms) == 0 {
		return "", false
	}
	var digits strings.Builder
	for _, t := range terms {
		a, ok := spysOneResolve(vars, t[1], map[string]bool{})
		if !ok {
			return "", false
		}
		b, ok := spysOneResolve(vars, t[2], map[string]bool{})
		if !ok {
			return "", false
		}
		digits.WriteString(strconv.Itoa(a ^ b))
	}

	port := digits.String()
	if len(port) < 1 || len(port) > 5 {
		return "", false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n > 65535 {
		return "", false
	}
	return port, true
}

// spysOneResolve expands one variable to its final integer. A name defined as a
// literal is that integer; a name defined as `N^other` is `N` XOR the recursive
// expansion of `other`. `seen` guards against a self-referential or cyclic
// table, which would otherwise recurse forever.
func spysOneResolve(vars map[string]string, name string, seen map[string]bool) (int, bool) {
	if seen[name] {
		return 0, false
	}
	expr, ok := vars[name]
	if !ok {
		return 0, false
	}
	if n, err := strconv.Atoi(expr); err == nil {
		return n, true
	}
	m := spysOneVarRefRe.FindStringSubmatch(expr)
	if m == nil {
		return 0, false
	}
	base, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	seen[name] = true
	rhs, ok := spysOneResolve(vars, m[2], seen)
	if !ok {
		return 0, false
	}
	return base ^ rhs, true
}

// spysOneParseDate builds a UTC time from one spysOneDateRe match. It validates
// each field so an out-of-range value (which time.Date would otherwise roll
// over into a neighbouring month/day) is rejected rather than yielding a
// plausible-looking wrong instant.
func spysOneParseDate(m [][]byte) (time.Time, bool) {
	day, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return time.Time{}, false
	}
	month, ok := spysOneMonths[strings.ToLower(string(m[2]))]
	if !ok {
		return time.Time{}, false
	}
	year, err := strconv.Atoi(string(m[3]))
	if err != nil {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(string(m[4]))
	if err != nil {
		return time.Time{}, false
	}
	minute, err := strconv.Atoi(string(m[5]))
	if err != nil {
		return time.Time{}, false
	}
	t := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	if t.Day() != day || t.Month() != month || t.Hour() != hour || t.Minute() != minute {
		return time.Time{}, false
	}
	return t, true
}
