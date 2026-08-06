// envelope.go — typed JSON envelopes, the only inter-phase contract (R8),
// with tolerant extraction and the live-session parse-correction loop (R7).
// Extraction accepts what agents actually emit: bare JSON, fenced ```json
// blocks, or JSON buried in prose (first `{` to last `}`). Validation is
// typed against the envelope base shape — status is mandatory and must be
// literal success/fail; the well-known fields must carry their declared
// types when present. A failed parse re-prompts the SAME live session,
// naming the error and the required fields, under a per-emission budget;
// every invalid attempt persists to the trace, size-capped (R7, R15).
package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// parsedEnvelope is one valid envelope: the canonical JSON, the full field
// map (extension fields like `approved` ride here for predicates and
// `if:` guards), and the typed base.
type parsedEnvelope struct {
	Raw    json.RawMessage
	Fields map[string]any
	Base   protocol.Envelope
}

// envelopeFieldNames is what a parse correction tells the agent to emit —
// the base contract every phase relies on (R8).
const envelopeFieldNames = "status, summary, artifacts, notes_for_next_agent"

// extractJSON finds the JSON object in a response: fenced blocks first
// (odd-indexed segments of a ``` split, `json` tag stripped), then the
// first-{/last-} span of whatever candidate held.
func extractJSON(text string) (map[string]any, json.RawMessage, error) {
	candidate := text
	if strings.Contains(text, "```") {
		segments := strings.Split(text, "```")
		for i := 1; i < len(segments); i += 2 {
			block := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(segments[i]), "json"))
			if strings.HasPrefix(block, "{") {
				candidate = block
				break
			}
		}
	}
	start := strings.Index(candidate, "{")
	end := strings.LastIndex(candidate, "}")
	if start == -1 || end <= start {
		return nil, nil, fmt.Errorf("no JSON object found in the response")
	}
	raw := candidate[start : end+1]
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return fields, json.RawMessage(raw), nil
}

// validateEnvelope checks the extracted object against the base contract:
// status mandatory and literal, well-known fields typed when present.
// Extension fields pass through untouched — predicates and guards read them.
func validateEnvelope(fields map[string]any, raw json.RawMessage) (parsedEnvelope, error) {
	status, present := fields["status"].(string)
	if !present {
		return parsedEnvelope{}, fmt.Errorf("field \"status\" is missing or not a string")
	}
	if status != protocol.EnvelopeSuccess && status != protocol.EnvelopeFail {
		return parsedEnvelope{}, fmt.Errorf(
			"field \"status\" must be %q or %q, got %q",
			protocol.EnvelopeSuccess, protocol.EnvelopeFail, status)
	}
	envelope := protocol.Envelope{Status: status}
	if value, exists := fields["summary"]; exists {
		text, ok := value.(string)
		if !ok {
			return parsedEnvelope{}, fmt.Errorf("field \"summary\" must be a string")
		}
		envelope.Summary = text
	}
	if value, exists := fields["notes_for_next_agent"]; exists {
		text, ok := value.(string)
		if !ok {
			return parsedEnvelope{}, fmt.Errorf("field \"notes_for_next_agent\" must be a string")
		}
		envelope.NotesForNextAgent = text
	}
	if value, exists := fields["artifacts"]; exists {
		artifacts, err := stringList(value)
		if err != nil {
			return parsedEnvelope{}, fmt.Errorf("field \"artifacts\" %w", err)
		}
		envelope.Artifacts = artifacts
	}
	return parsedEnvelope{Raw: raw, Fields: fields, Base: envelope}, nil
}

func stringList(value any) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("must be a list of strings")
	}
	list := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("must be a list of strings")
		}
		list = append(list, text)
	}
	return list, nil
}

// parseEnvelopeText runs extraction then validation — one emission's parse.
func parseEnvelopeText(text string) (parsedEnvelope, error) {
	fields, raw, err := extractJSON(text)
	if err != nil {
		return parsedEnvelope{}, err
	}
	return validateEnvelope(fields, raw)
}

// parseCorrection is the message a parse failure sends into the same live
// session: it names the error and the required fields (R7).
func parseCorrection(err error) string {
	return fmt.Sprintf(
		"Your response was not valid JSON for the required structure (%v). "+
			"Respond again with ONLY a JSON object with these fields: %s. "+
			"No prose, no code fences.", err, envelopeFieldNames)
}

// gateCorrection is the message gate violations send into the same live
// session (R9). The corrected emission re-enters parsing with a FRESH
// per-emission budget — the nesting the execution diagram makes normative.
func gateCorrection(violations []string) string {
	return "Your previous response failed validation:\n- " +
		strings.Join(violations, "\n- ") +
		"\n\nFix these problems, then re-emit ONLY your Report JSON."
}

// invalidEnvelopePayload is the trace record of one invalid emission,
// size-capped server-side; the full form survives in attempt-local JSONL
// only insofar as this payload carries it — the cap keeps a runaway
// emission from bloating anything downstream (R7).
func invalidEnvelopePayload(role string, parseAttempt int, raw string, parseErr error) map[string]any {
	return map[string]any{
		"role":          role,
		"parse_attempt": parseAttempt,
		"error":         parseErr.Error(),
		"raw":           truncateText(raw, protocol.MaxInvalidEnvelopeBytes),
	}
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
