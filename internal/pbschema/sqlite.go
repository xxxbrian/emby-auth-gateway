package pbschema

import (
	"fmt"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// validateTable compares the complete physical shape PocketBase exposes. SQLite
// reports only the declared type through table_info, so type spellings are
// normalized by upper-casing and collapsing whitespace.
func validateTable(app core.App, collection *core.Collection) error {
	actual, err := app.TableInfo(collection.Name)
	if err != nil {
		return fmt.Errorf("inspect table %q: %w", collection.Name, err)
	}
	expected := map[string]columnSpec{}
	for _, field := range collection.Fields {
		expected[field.GetName()] = columnFromDeclaration(field.ColumnType(app), field.GetName() == "id")
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("table %q has %d columns, want %d: %w", collection.Name, len(actual), len(expected), errPhysicalSchemaMismatch)
	}
	for _, column := range actual {
		want, ok := expected[column.Name]
		if !ok {
			return fmt.Errorf("table %q has unexpected column %q: %w", collection.Name, column.Name, errPhysicalSchemaMismatch)
		}
		got := columnSpec{typ: normalizeType(column.Type), notNull: column.NotNull, def: normalizeDefault(column.DefaultValue.String), pk: column.PK}
		if !columnSpecsEqual(got, want) {
			return fmt.Errorf("table %q column %q differs: got %#v want %#v: %w", collection.Name, column.Name, got, want, errPhysicalSchemaMismatch)
		}
	}
	indexes, err := app.TableIndexes(collection.Name)
	if err != nil {
		return fmt.Errorf("inspect indexes %q: %w", collection.Name, err)
	}
	wantIndexes := map[string]string{}
	for _, index := range collection.Indexes {
		name := indexName(index)
		wantIndexes[name] = normalizeIndex(index)
	}
	if len(indexes) != len(wantIndexes) {
		return fmt.Errorf("table %q has %d explicit indexes, want %d: %w", collection.Name, len(indexes), len(wantIndexes), errPhysicalSchemaMismatch)
	}
	for name, sql := range indexes {
		want, ok := wantIndexes[name]
		if !ok || normalizeIndex(sql) != want {
			return fmt.Errorf("table %q index %q differs: got %q want %q: %w", collection.Name, name, normalizeIndex(sql), want, errPhysicalSchemaMismatch)
		}
	}
	return nil
}

func columnSpecsEqual(got, want columnSpec) bool { return got == want }

type columnSpec struct {
	typ     string
	notNull bool
	def     string
	pk      int
}

func columnFromDeclaration(declaration string, id bool) columnSpec {
	words := strings.Fields(strings.ToUpper(declaration))
	spec := columnSpec{typ: words[0], notNull: strings.Contains(strings.ToUpper(declaration), "NOT NULL")}
	if id {
		spec.pk = 1
	}
	if at := strings.Index(strings.ToUpper(declaration), " DEFAULT "); at >= 0 {
		value := declaration[at+len(" DEFAULT "):]
		if end := strings.Index(strings.ToUpper(value), " NOT NULL"); end >= 0 {
			value = value[:end]
		}
		if end := strings.Index(strings.ToUpper(value), " PRIMARY KEY"); end >= 0 {
			value = value[:end]
		}
		spec.def = normalizeDefault(value)
	}
	return spec
}
func normalizeType(value string) string {
	return strings.Join(strings.Fields(strings.ToUpper(value)), " ")
}
func normalizeDefault(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(strings.ToUpper(value))), " ")
	for strings.HasPrefix(value, "(") && strings.HasSuffix(value, ")") {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}
func indexName(sql string) string {
	parts := strings.Split(sql, "`")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}
func normalizeIndex(sql string) string {
	sql = strings.ReplaceAll(sql, "`", "\"")
	sql = strings.ReplaceAll(sql, "[", "\"")
	sql = strings.ReplaceAll(sql, "]", "\"")
	sql = strings.ReplaceAll(sql, "\"", "")
	sql = strings.Join(strings.Fields(strings.ToUpper(sql)), " ")
	sql = strings.ReplaceAll(sql, "( ", "(")
	sql = strings.ReplaceAll(sql, " )", ")")
	sql = strings.ReplaceAll(sql, " ,", ",")
	sql = strings.ReplaceAll(sql, "( ", "(")
	return sql
}
