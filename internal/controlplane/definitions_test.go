// definitions_test.go — the authoring surface (U5, R1): save-time validation
// that names what is wrong, in-place edits with a generation counter, and the
// same Origin fence every other mutation takes.
package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/StructuPath/jig/internal/protocol"
)

// u5Definition is the authoring fixture: two phases, one gate, no env
// requirement (so claim eligibility never depends on the test host).
const u5Definition = `name: u5-fixture
roster:
  builder:
    model: claude-sonnet
    system_prompt: you build
    user_prompt: build {{prompt}}
phases:
  - name: build
    kind: agent
    owner: builder
    gates:
      - {name: artifacts_exist}
acceptance: [all_phases_passed]
`

// u5EditedDefinition is the same definition after an edit that changes the
// GATE configuration — the snapshot-isolation scenario names gates
// specifically, because a gate is the part of a definition that decides
// whether work is accepted.
const u5EditedDefinition = `name: u5-fixture
roster:
  builder:
    model: claude-opus
    system_prompt: you build carefully
    user_prompt: build {{prompt}}
phases:
  - name: build
    kind: agent
    owner: builder
    gates:
      - {name: verdict_consistent}
acceptance: [all_phases_passed, verdict_consistent]
`

func mustCreateDefinition(t *testing.T, store *Store, source string) protocol.Definition {
	t.Helper()
	definition, err := store.CreateDefinition(context.Background(), DefinitionInput{Source: source})
	if err != nil {
		t.Fatalf("create definition: %v", err)
	}
	return definition
}

func TestSavingADefinitionWhoseGateIsNotInTheRegistryIsRejectedNamingTheGate(t *testing.T) {
	store, _ := newTestStore(t)
	source := strings.Replace(u5Definition, "artifacts_exist", "artifacts_exists", 1)
	_, err := store.CreateDefinition(context.Background(), DefinitionInput{Source: source})
	if err == nil {
		t.Fatal("a definition naming a gate outside the registry must be rejected at save")
	}
	if code := serviceCode(t, err); code != "invalid_definition" {
		t.Fatalf("rejection code = %q, want invalid_definition", code)
	}
	// The operator must be able to fix the YAML from the message alone (R1).
	if !strings.Contains(err.Error(), "artifacts_exists") || !strings.Contains(err.Error(), "build") {
		t.Fatalf("rejection %q names neither the offending gate nor its phase", err)
	}
	definitions, err := store.Definitions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 0 {
		t.Fatalf("stored %d definitions, want an invalid definition to reach no row", len(definitions))
	}
}

func TestSavingADefinitionWhoseOwnerRoleIsUndefinedIsRejectedNamingTheRole(t *testing.T) {
	store, _ := newTestStore(t)
	source := strings.Replace(u5Definition, "owner: builder", "owner: reviewer", 1)
	_, err := store.CreateDefinition(context.Background(), DefinitionInput{Source: source})
	if err == nil {
		t.Fatal("a phase owned by an undefined role must be rejected at save")
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("rejection %q does not name the undefined role", err)
	}
}

func TestEditingADefinitionBumpsItsGenerationInPlaceWithoutKeepingARevision(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	definition := mustCreateDefinition(t, store, u5Definition)
	if definition.Generation != 1 {
		t.Fatalf("created generation = %d, want 1", definition.Generation)
	}

	edited, err := store.UpdateDefinition(ctx, definition.ID, DefinitionInput{Source: u5EditedDefinition})
	if err != nil {
		t.Fatalf("update definition: %v", err)
	}
	if edited.ID != definition.ID || edited.Generation != 2 {
		t.Fatalf("edit produced %s@%d, want the same id at generation 2",
			edited.ID, edited.Generation)
	}
	if edited.Source != u5EditedDefinition {
		t.Fatal("the edit did not replace the stored source in place")
	}

	// Saving identical bytes is not an edit: the counter has to mean
	// something in a trace.
	again, err := store.UpdateDefinition(ctx, definition.ID, DefinitionInput{Source: u5EditedDefinition})
	if err != nil {
		t.Fatalf("re-save definition: %v", err)
	}
	if again.Generation != 2 {
		t.Fatalf("re-saving identical bytes moved the generation to %d, want 2", again.Generation)
	}

	// One row, one definition: there is no revision library to grow.
	definitions, err := store.Definitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || definitions[0].Generation != 2 {
		t.Fatalf("definitions = %+v, want exactly one record at generation 2", definitions)
	}
}

func TestEditingADefinitionThroughAnInvalidSourceLeavesTheStoredOneIntact(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	definition := mustCreateDefinition(t, store, u5Definition)
	broken := strings.Replace(u5Definition, "kind: agent", "kind: wizard", 1)
	if _, err := store.UpdateDefinition(ctx, definition.ID, DefinitionInput{Source: broken}); err == nil {
		t.Fatal("an invalid edit must be rejected")
	}
	stored, err := store.Definition(ctx, definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Source != u5Definition || stored.Generation != 1 {
		t.Fatalf("stored = %s@%d, want the original source at generation 1",
			stored.Source, stored.Generation)
	}
}

func TestCreatingASecondDefinitionUnderATakenNameIsAConflict(t *testing.T) {
	store, _ := newTestStore(t)
	mustCreateDefinition(t, store, u5Definition)
	_, err := store.CreateDefinition(context.Background(), DefinitionInput{Source: u5EditedDefinition})
	if err == nil {
		t.Fatal("a second definition under a taken name must conflict, not silently edit")
	}
	if code := serviceCode(t, err); code != "definition_exists" {
		t.Fatalf("conflict code = %q, want definition_exists", code)
	}
}

func TestAuthoringRoutesRejectAForeignOriginAndAcceptTheSameOrigin(t *testing.T) {
	store, _ := newTestStore(t)
	handler := NewHandler(store, "", nil)

	post := func(origin, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "http://127.0.0.1:8383/api/definitions",
			strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	payload, err := json.Marshal(DefinitionInput{Source: u5Definition})
	if err != nil {
		t.Fatal(err)
	}
	foreign := post("http://evil.example", string(payload))
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("foreign Origin: status %d, want 403", foreign.Code)
	}
	if !strings.Contains(foreign.Body.String(), "cross_origin_request") {
		t.Fatalf("foreign Origin body = %s, want cross_origin_request", foreign.Body.String())
	}
	// Nothing was authored behind the fence.
	definitions, err := store.Definitions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 0 {
		t.Fatalf("a rejected cross-origin POST stored %d definitions", len(definitions))
	}

	created := post("http://127.0.0.1:8383", string(payload))
	if created.Code != http.StatusCreated {
		t.Fatalf("same-origin create: status %d body %s, want 201",
			created.Code, created.Body.String())
	}
	var definition protocol.Definition
	if err := json.Unmarshal(created.Body.Bytes(), &definition); err != nil {
		t.Fatalf("decode created definition: %v", err)
	}
	if definition.Name != "u5-fixture" || definition.Generation != 1 {
		t.Fatalf("created = %+v, want the fixture at generation 1", definition)
	}

	// And a save-time rejection travels as a 400 naming the element.
	invalidSource := strings.Replace(u5Definition, "artifacts_exist", "nope", 1)
	invalidPayload, err := json.Marshal(DefinitionInput{Source: invalidSource})
	if err != nil {
		t.Fatal(err)
	}
	rejected := post("http://127.0.0.1:8383", string(invalidPayload))
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("invalid definition: status %d, want 400", rejected.Code)
	}
	if !strings.Contains(rejected.Body.String(), "nope") {
		t.Fatalf("invalid definition body = %s, want the offending gate named", rejected.Body.String())
	}
}
