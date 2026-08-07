// definitions.go — authoring and validating Job Definitions (U5, R1).
//
// Definitions edit IN PLACE with a generation counter. There is no revision
// library and no version history table: factory's target design carried one
// and its own verdict was that it earned nothing, because the artifact that
// matters after the fact is not "what did the definition say in March" but
// "what exactly executed in this run" — and that is the run's frozen
// snapshot (R2), which no edit can reach. The generation counter exists so
// an operator can tell two edits apart in a trace, not so anything can be
// restored from it.
//
// Validation is save-time and total (R1): protocol.Validate is the one
// contract, nothing downstream re-checks it, and a rejection names the
// offending element so the operator fixes the YAML from the message alone.
package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// DefinitionInput is the create/update body: the definition YAML itself.
// Nothing else is settable — name and generation are derived, never sent, so
// the stored record can never disagree with the source it holds.
type DefinitionInput struct {
	Source string `json:"source"`
}

// CreateDefinition validates and stores a new definition at generation 1.
// A name already in use is a conflict, not an implicit edit: an operator who
// meant to edit says so with the id (R1).
func (s *Store) CreateDefinition(ctx context.Context, input DefinitionInput) (protocol.Definition, error) {
	spec, err := parseDefinitionInput(input)
	if err != nil {
		return protocol.Definition{}, err
	}
	now := s.now().UnixMilli()
	id, err := newID()
	if err != nil {
		return protocol.Definition{}, unavailable(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO definitions(id, name, generation, source, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?, ?)
	`, id, spec.Name, input.Source, now, now); err != nil {
		if isConstraintViolation(err) {
			return protocol.Definition{}, conflict("definition_exists",
				"a definition named "+quote(spec.Name)+" already exists — update it by id")
		}
		return protocol.Definition{}, unavailable(err)
	}
	return s.Definition(ctx, id)
}

// UpdateDefinition validates and replaces a definition's source in place,
// bumping its generation (R1). An identical source is a no-op that returns
// the stored record unchanged — saving the same bytes twice is not an edit,
// and inflating the counter for it would make the number meaningless in a
// trace. The name follows the source, so an edit may rename a definition;
// colliding with another definition's name is a conflict.
func (s *Store) UpdateDefinition(ctx context.Context, id string, input DefinitionInput) (protocol.Definition, error) {
	spec, err := parseDefinitionInput(input)
	if err != nil {
		return protocol.Definition{}, err
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Definition{}, unavailable(err)
	}
	defer tx.Rollback()
	var storedSource string
	var generation int
	err = tx.QueryRowContext(ctx,
		`SELECT source, generation FROM definitions WHERE id = ?`, id).Scan(&storedSource, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Definition{}, ErrNotFound
	}
	if err != nil {
		return protocol.Definition{}, unavailable(err)
	}
	if storedSource != input.Source {
		if _, err := tx.ExecContext(ctx, `
			UPDATE definitions SET name = ?, generation = generation + 1, source = ?, updated_at = ?
			WHERE id = ?
		`, spec.Name, input.Source, now, id); err != nil {
			if isConstraintViolation(err) {
				return protocol.Definition{}, conflict("definition_exists",
					"another definition is already named "+quote(spec.Name))
			}
			return protocol.Definition{}, unavailable(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return protocol.Definition{}, unavailable(err)
	}
	return s.Definition(ctx, id)
}

// saveDefinition is create-or-edit keyed on the definition's name: the
// direct-run path (U11) has a file, not an id, and running the same file
// twice must not accumulate definitions. It shares UpdateDefinition's
// semantics — same source, same generation.
func (s *Store) saveDefinition(ctx context.Context, source string) (protocol.Definition, error) {
	spec, err := parseDefinitionInput(DefinitionInput{Source: source})
	if err != nil {
		return protocol.Definition{}, err
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Definition{}, unavailable(err)
	}
	defer tx.Rollback()
	var id, storedSource string
	err = tx.QueryRowContext(ctx,
		`SELECT id, source FROM definitions WHERE name = ?`, spec.Name).Scan(&id, &storedSource)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id, err = newID()
		if err != nil {
			return protocol.Definition{}, unavailable(err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO definitions(id, name, generation, source, created_at, updated_at)
			VALUES (?, ?, 1, ?, ?, ?)
		`, id, spec.Name, source, now, now); err != nil {
			return protocol.Definition{}, unavailable(err)
		}
	case err != nil:
		return protocol.Definition{}, unavailable(err)
	case storedSource != source:
		if _, err := tx.ExecContext(ctx, `
			UPDATE definitions SET generation = generation + 1, source = ?, updated_at = ? WHERE id = ?
		`, source, now, id); err != nil {
			return protocol.Definition{}, unavailable(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return protocol.Definition{}, unavailable(err)
	}
	return s.Definition(ctx, id)
}

// Definition reads one definition record.
func (s *Store) Definition(ctx context.Context, id string) (protocol.Definition, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, generation, source, created_at, updated_at FROM definitions WHERE id = ?
	`, id)
	value, err := scanDefinition(row)
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, unavailable(err)
	}
	return value, nil
}

// Definitions lists every definition, newest edit first.
func (s *Store) Definitions(ctx context.Context) ([]protocol.Definition, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, generation, source, created_at, updated_at
		FROM definitions ORDER BY updated_at DESC, id
	`)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	values := []protocol.Definition{}
	for rows.Next() {
		value, err := scanDefinition(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return values, nil
}

func scanDefinition(row rowScanner) (protocol.Definition, error) {
	var value protocol.Definition
	var createdAt, updatedAt int64
	if err := row.Scan(&value.ID, &value.Name, &value.Generation, &value.Source,
		&createdAt, &updatedAt); err != nil {
		return value, err
	}
	value.CreatedAt = fromMillis(createdAt)
	value.UpdatedAt = fromMillis(updatedAt)
	return value, nil
}

// parseDefinitionInput runs the save-time contract (R1). The validation
// message travels verbatim to the operator: protocol.Validate names the
// offending phase, role, gate, or edge, and re-wording it here would only
// blur it.
func parseDefinitionInput(input DefinitionInput) (*protocol.DefinitionSpec, error) {
	if strings.TrimSpace(input.Source) == "" {
		return nil, invalid("invalid_definition", "definition source is required")
	}
	if len(input.Source) > protocol.MaxRequestBodyBytes {
		return nil, invalid("invalid_definition", "definition source exceeds its storage limit")
	}
	spec, err := protocol.ParseDefinition([]byte(input.Source))
	if err != nil {
		return nil, invalid("invalid_definition", err.Error())
	}
	return spec, nil
}

func quote(value string) string { return "\"" + value + "\"" }
