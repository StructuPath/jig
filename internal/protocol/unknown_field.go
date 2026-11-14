// unknown_field.go — the operator's form of a strict-decode rejection.
// yaml.v3 names an unknown field by the Go type it is missing from ("field
// writs not found in type protocol.RoleSpec"), which is this package's
// vocabulary, not the definition author's. ParseDefinition re-walks the
// document against the schema to name the element the author wrote
// (`role "builder"`) and the yaml field they most likely meant.
package protocol

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// unknownFieldError finds the first mapping key in source that the schema
// does not declare and describes it. It returns nil when the document has
// no such key, which leaves the decoder's own error standing.
func unknownFieldError(source []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil || len(document.Content) == 0 {
		return nil
	}
	return findUnknownField(document.Content[0], reflect.TypeOf(DefinitionSpec{}), "definition")
}

func findUnknownField(node *yaml.Node, schema reflect.Type, where string) error {
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	switch {
	case schema.Kind() == reflect.Struct && node.Kind == yaml.MappingNode:
		fields := yamlFields(schema)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			field, known := fields[key.Value]
			if !known {
				names := make([]string, 0, len(fields))
				for name := range fields {
					names = append(names, name)
				}
				sort.Strings(names)
				message := fmt.Sprintf("line %d: %s: unknown field %q", key.Line, where, key.Value)
				if suggestion := ClosestMatch(key.Value, names); suggestion != "" {
					message += fmt.Sprintf(" (did you mean %q?)", suggestion)
				}
				return fmt.Errorf("%s", message)
			}
			child := key.Value
			if where != "definition" {
				child = where + "." + key.Value
			}
			if err := findUnknownField(value, field, child); err != nil {
				return err
			}
		}
	case schema.Kind() == reflect.Map && node.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			child := fmt.Sprintf("%s[%q]", where, node.Content[i].Value)
			if where == "roster" {
				child = fmt.Sprintf("role %q", node.Content[i].Value)
			}
			if err := findUnknownField(node.Content[i+1], schema.Elem(), child); err != nil {
				return err
			}
		}
	case schema.Kind() == reflect.Slice && node.Kind == yaml.SequenceNode:
		for index, item := range node.Content {
			child := fmt.Sprintf("%s[%d]", where, index)
			if name := mappingValue(item, "name"); name != "" {
				switch {
				case where == "phases":
					child = fmt.Sprintf("phase %q", name)
				case strings.HasSuffix(where, ".gates"):
					child = fmt.Sprintf("%s gate %q", strings.TrimSuffix(where, ".gates"), name)
				}
			}
			if err := findUnknownField(item, schema.Elem(), child); err != nil {
				return err
			}
		}
	}
	return nil
}

// yamlFields maps each yaml key a struct declares to the field's type.
func yamlFields(schema reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, schema.NumField())
	for i := 0; i < schema.NumField(); i++ {
		field := schema.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = field.Type
	}
	return fields
}

func mappingValue(node *yaml.Node, key string) string {
	if node.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1].Value
		}
	}
	return ""
}
