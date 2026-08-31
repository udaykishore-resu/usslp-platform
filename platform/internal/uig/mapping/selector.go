package mapping

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Selector is a compiled field selector: the JSON-path-ish expression a mapping
// document uses to point at a value inside an inbound payload.
//
// It is deliberately a small subset of JSONPath rather than the whole thing.
// The grammar is:
//
//	$            the current record (the element the root selector produced)
//	$^           the enclosing group, when the document declares one — the
//	             site, plant or store header a run of price rows sits under
//	$$           the document root, for values that live outside the repeated
//	             element — a currency or a batch id in the header of a payload
//	             whose price rows are nested three levels down
//	.name        object member
//	["name"]     object member whose name contains punctuation
//	[3]          array element by index
//	[*]          every element of an array (root selectors only)
//
// Everything an integration engineer has actually needed to describe a POS
// payload is in that list. What is left out is left out on purpose: recursive
// descent, filters and script expressions would turn a mapping document into a
// program, and a program supplied as configuration by a partner is a remote
// code execution surface on the pricing path. A mapping document must be
// readable in ten seconds by the on-call engineer who has to decide whether it
// caused the incident.
type Selector struct {
	// raw is the original text, kept for error messages that an integration
	// engineer can act on without a debugger.
	raw string
	// scope says which node the selector starts from.
	scope scopeKind
	steps []step
	// wildcard records whether any step fans out, which is what makes a
	// selector legal as a root and illegal as a field.
	wildcard bool
}

type step struct {
	// name is set for object member access.
	name string
	// index is set for array element access when wildcard is false.
	index int
	kind  stepKind
}

// scopeKind is the node a selector is resolved against.
type scopeKind uint8

const (
	// scopeRecord is the repeated element the root selector produced.
	scopeRecord scopeKind = iota
	// scopeGroup is the enclosing header element, for the header-and-lines
	// shape that every ERP price feed eventually turns out to have: a site or
	// plant carrying a currency and an effective date, with a run of article
	// rows underneath it. Without it, a currency declared once per site would
	// have to be repeated on every row or hoisted to the document, and real
	// feeds do neither.
	scopeGroup
	// scopeDocument is the whole payload.
	scopeDocument
)

type stepKind uint8

const (
	stepMember stepKind = iota
	stepIndex
	stepWildcard
)

// String returns the selector's original text.
func (s Selector) String() string { return s.raw }

// FromRoot reports whether the selector escapes the current record and reads
// from the document root.
func (s Selector) FromRoot() bool { return s.scope == scopeDocument }

// FromGroup reports whether the selector reads from the enclosing group.
func (s Selector) FromGroup() bool { return s.scope == scopeGroup }

// HasWildcard reports whether the selector fans out to many values.
func (s Selector) HasWildcard() bool { return s.wildcard }

// ParseSelector compiles a selector, rejecting anything outside the grammar at
// configuration-load time rather than at 3am on the price path.
func ParseSelector(raw string) (Selector, error) {
	s := Selector{raw: raw}
	rest := strings.TrimSpace(raw)
	if rest == "" {
		return Selector{}, fmt.Errorf("%w: empty selector", ErrDocument)
	}
	switch {
	case strings.HasPrefix(rest, "$$"):
		s.scope = scopeDocument
		rest = rest[2:]
	case strings.HasPrefix(rest, "$^"):
		s.scope = scopeGroup
		rest = rest[2:]
	case strings.HasPrefix(rest, "$"):
		s.scope = scopeRecord
		rest = rest[1:]
	default:
		return Selector{}, fmt.Errorf("%w: selector %q must start with $, $^ or $$", ErrDocument, raw)
	}
	for rest != "" {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			if end == -1 {
				end = len(rest)
			}
			name := rest[:end]
			if name == "" {
				return Selector{}, fmt.Errorf("%w: selector %q has an empty member name", ErrDocument, raw)
			}
			s.steps = append(s.steps, step{kind: stepMember, name: name})
			rest = rest[end:]
		case '[':
			end := strings.IndexByte(rest, ']')
			if end == -1 {
				return Selector{}, fmt.Errorf("%w: selector %q has an unclosed [", ErrDocument, raw)
			}
			inner := rest[1:end]
			rest = rest[end+1:]
			switch {
			case inner == "*":
				s.steps = append(s.steps, step{kind: stepWildcard})
				s.wildcard = true
			case len(inner) >= 2 && (inner[0] == '"' || inner[0] == '\'') && inner[len(inner)-1] == inner[0]:
				s.steps = append(s.steps, step{kind: stepMember, name: inner[1 : len(inner)-1]})
			default:
				n, err := strconv.Atoi(inner)
				if err != nil || n < 0 {
					return Selector{}, fmt.Errorf("%w: selector %q has a bad subscript %q", ErrDocument, raw, inner)
				}
				s.steps = append(s.steps, step{kind: stepIndex, index: n})
			}
		default:
			return Selector{}, fmt.Errorf("%w: selector %q has trailing text %q", ErrDocument, raw, rest)
		}
	}
	return s, nil
}

// Eval returns every value the selector matches. record is the current element,
// group its enclosing header (nil when the document declares none) and root the
// whole document.
//
// A missing member yields no values rather than an error, because "the field is
// absent" is the normal shape of an optional field and must be distinguishable
// from "the field is present and unusable".
func (s Selector) Eval(record, group, root any) []any {
	var start any
	switch s.scope {
	case scopeDocument:
		start = root
	case scopeGroup:
		start = group
		if start == nil {
			// A mapping written for a grouped feed and applied to an ungrouped
			// one resolves group references against the document rather than
			// failing: the document *is* the only enclosing scope there is.
			start = root
		}
	default:
		start = record
	}
	return evalSteps(s.steps, start)
}

// One returns the single value a selector matches, reporting whether there was
// exactly one. Field selectors are compiled with wildcards forbidden, so more
// than one match means the payload itself has an unexpected shape.
func (s Selector) One(record, group, root any) (any, bool) {
	vals := s.Eval(record, group, root)
	if len(vals) != 1 {
		return nil, false
	}
	return vals[0], vals[0] != nil
}

func evalSteps(steps []step, node any) []any {
	if len(steps) == 0 {
		if node == nil {
			return nil
		}
		return []any{node}
	}
	st := steps[0]
	switch st.kind {
	case stepMember:
		obj, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		child, ok := obj[st.name]
		if !ok {
			return nil
		}
		return evalSteps(steps[1:], child)
	case stepIndex:
		arr, ok := node.([]any)
		if !ok || st.index >= len(arr) {
			return nil
		}
		return evalSteps(steps[1:], arr[st.index])
	default: // stepWildcard
		switch v := node.(type) {
		case []any:
			out := make([]any, 0, len(v))
			for _, el := range v {
				out = append(out, evalSteps(steps[1:], el)...)
			}
			return out
		case map[string]any:
			// Fanning out over an object's values is what makes Clover-shaped
			// payloads — {"merchants": {"<id>": [ ... ]}} — expressible without
			// knowing the merchant id in advance.
			out := make([]any, 0, len(v))
			for _, key := range sortedKeys(v) {
				out = append(out, evalSteps(steps[1:], v[key])...)
			}
			return out
		default:
			return nil
		}
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Sorted so that a payload with several keys produces price changes in a
	// stable order; an unstable order would make the emitted event sequence
	// differ between replays of the same delivery.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// decodeJSON parses a payload into generic values with numbers preserved as
// json.Number.
//
// UseNumber is not a stylistic choice. json.Unmarshal into an interface decodes
// every number as float64, and a float64 on the price path is exactly the defect
// this platform forbids: 12345678901234567 minor units and 0.1 + 0.2 are both
// wrong in ways that reach a shelf. Keeping numbers as their original text lets
// the decimal package do the conversion in integer arithmetic.
func decodeJSON(body []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPayload, err)
	}
	return doc, nil
}
