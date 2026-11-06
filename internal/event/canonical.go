package event

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// canonical writes the RFC 8785 (JSON Canonicalization Scheme) serialization of
// v, restricted to the no-floating-point subset EVENTS.md §1.1 permits.
//
// The float ban is what makes this tractable: full JCS requires ECMAScript
// number-to-string conversion, which is a research project on its own. With
// integers only, canonical numbers are just their shortest decimal form.
//
// Deviation from strict JCS, harmless here: keys are sorted by UTF-8 byte order
// (equivalently, code point order) rather than by UTF-16 code unit. The two
// differ only for supplementary-plane characters, and every key in this schema
// is ASCII. EVENTS.md §2.1 specifies code point order, so this matches the spec
// it implements.
func canonical(w io.Writer, v any) error {
	switch t := v.(type) {
	case nil:
		_, err := io.WriteString(w, "null")
		return err
	case bool:
		if t {
			_, err := io.WriteString(w, "true")
			return err
		}
		_, err := io.WriteString(w, "false")
		return err
	case string:
		return canonicalString(w, t)
	case json.Number:
		return canonicalNumber(w, t)
	case int:
		return canonicalNumber(w, json.Number(fmt.Sprintf("%d", t)))
	case int64:
		return canonicalNumber(w, json.Number(fmt.Sprintf("%d", t)))
	case float64:
		// Reachable only if a caller bypasses Parse's UseNumber decoding.
		return fmt.Errorf("floating-point values are not permitted (EVENTS.md §1.1)")
	case map[string]any:
		return canonicalObject(w, t)
	case Event:
		return canonicalObject(w, map[string]any(t))
	case []any:
		return canonicalArray(w, t)
	default:
		return fmt.Errorf("cannot canonicalize %T", v)
	}
}

func canonicalObject(w io.Writer, m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}
	for i, k := range keys {
		if i > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if err := canonicalString(w, k); err != nil {
			return err
		}
		if _, err := io.WriteString(w, ":"); err != nil {
			return err
		}
		if err := canonical(w, m[k]); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "}")
	return err
}

func canonicalArray(w io.Writer, a []any) error {
	if _, err := io.WriteString(w, "["); err != nil {
		return err
	}
	for i, v := range a {
		if i > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if err := canonical(w, v); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "]")
	return err
}

// canonicalNumber writes an integer in shortest form. Anything with a fraction
// or exponent is rejected rather than rounded (EVENTS.md §1.1).
//
// The digits are written verbatim rather than round-tripped through Int64.
// §1.1 bounds numeric fields to integers, not to 64 bits, and §1.3 requires
// readers to retain fields they do not recognize — so narrowing here made
// Marshal, and therefore Hash, reject an event that Parse had just accepted.
// An id that cannot be recomputed is one that cannot be verified, and a future
// re-serializing path would have failed at runtime on a log another tool wrote,
// which is the seam the whole format exists to keep open. Found by fuzzing.
//
// Verbatim is safe for identity because JSON already forbids leading zeros and
// a leading plus, so a parsed integer's text is its shortest form. Every value
// weft itself writes is small and encodes byte-identically to what Int64 gave,
// so no existing id changes.
func canonicalNumber(w io.Writer, n json.Number) error {
	s := n.String()
	if strings.ContainsAny(s, ".eE") {
		return fmt.Errorf("floating-point value %q is not permitted (EVENTS.md §1.1)", s)
	}
	if !isCanonicalInteger(s) {
		return fmt.Errorf("invalid integer %q", s)
	}
	// Negative zero is the one integer with two spellings. Canonical printing
	// requires picking one (SPEC.md §9.5), and Int64 used to pick this one.
	if s == "-0" {
		s = "0"
	}
	_, err := io.WriteString(w, s)
	return err
}

// checkNumbers rejects any number the canonical encoder could not write, at any
// depth. Parse calls it so that its domain stays inside Marshal's: an event that
// decodes but cannot be re-encoded has an id nobody can recompute, and §1.3's
// "retain fields you do not recognize" is about *unknown* fields, not about
// values that break §1.1's byte rules — a newer writer is still bound by those.
//
// Deliberately a walk rather than a trial encode, which would double the cost of
// every fold to answer a question about a handful of bytes (BUILD.md §1.4). It
// lives next to canonicalNumber so the two rules are read together.
func checkNumbers(v any) error {
	switch t := v.(type) {
	case json.Number:
		if strings.ContainsAny(t.String(), ".eE") {
			return fmt.Errorf("floating-point value %q is not permitted (EVENTS.md §1.1)", t.String())
		}
		if !isCanonicalInteger(t.String()) {
			return fmt.Errorf("invalid integer %q", t.String())
		}
	case map[string]any:
		for _, e := range t {
			if err := checkNumbers(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range t {
			if err := checkNumbers(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// isCanonicalInteger reports whether s is a JSON integer already in shortest
// form: an optional minus, then either "0" or a digit run with no leading zero.
func isCanonicalInteger(s string) bool {
	s = strings.TrimPrefix(s, "-")
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// canonicalString writes a JSON string with the shortest legal escaping.
//
// Go's encoding/json escapes <, >, and & for HTML safety, which JCS does not,
// so this cannot delegate to the standard marshaller.
func canonicalString(w io.Writer, s string) error {
	var b bytes.Buffer
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	_, err := w.Write(b.Bytes())
	return err
}
