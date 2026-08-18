package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Documentation rots silently. This walks the Config struct and fails when a
// yaml field is not mentioned in the reference, so adding a field without
// documenting it breaks the build rather than shipping a lie.
func TestEveryConfigFieldIsDocumented(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatalf("read the configuration reference: %v", err)
	}
	text := string(doc)

	fields := yamlFields(reflect.TypeOf(Config{}), "")
	if len(fields) == 0 {
		t.Fatal("walked no fields; the reflection is wrong, not the docs")
	}
	for _, name := range fields {
		if !strings.Contains(text, "`"+name+"`") {
			t.Errorf("field %q is not documented in docs/configuration.md", name)
		}
	}
}

// yamlFields returns the dotted yaml path of every field, following nested
// structs. A nested field is documented by its full path ("labels.review"),
// which is also how it is written in a config file.
func yamlFields(t reflect.Type, prefix string) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}
		// Duration is a scalar in YAML despite being a named type.
		if f.Type.Kind() == reflect.Struct && f.Type.Name() != "Duration" {
			out = append(out, yamlFields(f.Type, path)...)
			continue
		}
		out = append(out, path)
	}
	return out
}
