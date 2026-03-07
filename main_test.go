package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// helper: build a schemaIndex with a single resource type for testing
func testSchemaIndex(resourceType string, attrs map[string]attrSchema) *schemaIndex {
	return &schemaIndex{
		Resource: map[string]map[string]attrSchema{
			resourceType: attrs,
		},
		Data: make(map[string]map[string]attrSchema),
	}
}

func TestGolden(t *testing.T) {
	// force_destroy: optional=true, computed=false, default=false
	schema := testSchemaIndex("aws_s3_bucket", map[string]attrSchema{
		"force_destroy": {
			Optional: true,
			Computed: false,
			Default:  json.RawMessage(`false`),
		},
		"arn": {
			Optional: true,
			Computed: true,
			Default:  json.RawMessage(`""`),
		},
	})

	cases := []struct {
		name string
		dir  string
	}{
		{"default_removal", "testdata/default_removal"},
		{"default_mismatch", "testdata/default_mismatch"},
		{"null_value", "testdata/null_value"},
		{"computed_attribute", "testdata/computed_attribute"},
		{"expression_attribute", "testdata/expression_attribute"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inputPath := filepath.Join(tc.dir, "input.tf")
			expectedPath := filepath.Join(tc.dir, "expected.tf")

			inputSrc, err := os.ReadFile(inputPath)
			if err != nil {
				t.Fatal(err)
			}
			expectedSrc, err := os.ReadFile(expectedPath)
			if err != nil {
				t.Fatal(err)
			}

			f, diags := hclwrite.ParseConfig(inputSrc, inputPath, hcl.Pos{Line: 1, Column: 1})
			if diags.HasErrors() {
				t.Fatalf("parse error: %s", diags.Error())
			}

			for _, block := range f.Body().Blocks() {
				if block.Type() == "resource" {
					labels := block.Labels()
					if len(labels) < 1 {
						continue
					}
					attrs, ok := schema.Resource[labels[0]]
					if !ok {
						continue
					}
					pruneBodyAttrs(block.Body(), attrs)
				}
			}

			got := f.BuildTokens(nil).Bytes()
			if string(got) != string(expectedSrc) {
				t.Errorf("mismatch for %s\ngot:\n%s\nexpected:\n%s", tc.name, string(got), string(expectedSrc))
			}
		})
	}
}

func TestEvalLiteralExprToGo(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantVal any
		wantOK  bool
	}{
		{"bool_true", "true", true, true},
		{"bool_false", "false", false, true},
		{"number", "42", float64(42), true},
		{"string", `"hello"`, "hello", true},
		{"variable_ref", "var.foo", nil, false},
		{"function_call", `upper("a")`, nil, false},
		{"local_ref", "local.x", nil, false},
		{"empty_object", "{}", map[string]any{}, true},
		{"empty_list", "[]", []any{}, true},
		{"non_empty_object", `{a = 1}`, nil, false},
		{"non_empty_list", `[1]`, nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			val, ok := evalLiteralExprToGo([]byte(tc.input))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !deepEqualJSONish(val, tc.wantVal) {
				t.Errorf("val = %v (%T), want %v (%T)", val, val, tc.wantVal, tc.wantVal)
			}
		})
	}
}

func TestDeepEqualJSONish(t *testing.T) {
	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{"bool_eq", true, true, true},
		{"bool_neq", true, false, false},
		{"string_eq", "a", "a", true},
		{"string_neq", "a", "b", false},
		{"number_eq", float64(1), float64(1), true},
		{"number_neq", float64(1), float64(2), false},
		{"nil_eq", nil, nil, true},
		{"nil_neq", nil, false, false},
		{"map_eq", map[string]any{"k": "v"}, map[string]any{"k": "v"}, true},
		{"map_neq", map[string]any{"k": "v"}, map[string]any{"k": "x"}, false},
		{"slice_eq", []any{"a"}, []any{"a"}, true},
		{"slice_neq", []any{"a"}, []any{"b"}, false},
		{"type_mismatch", "1", float64(1), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := deepEqualJSONish(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("deepEqualJSONish(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestPruneBodyAttrs(t *testing.T) {
	tests := []struct {
		name        string
		hcl         string
		attrs       map[string]attrSchema
		wantChanged bool
	}{
		{
			name: "no_default_in_schema",
			hcl:  `force_destroy = false`,
			attrs: map[string]attrSchema{
				"force_destroy": {Optional: true, Computed: false, Default: nil},
			},
			wantChanged: false,
		},
		{
			name: "computed_skip",
			hcl:  `arn = ""`,
			attrs: map[string]attrSchema{
				"arn": {Optional: true, Computed: true, Default: json.RawMessage(`""`)},
			},
			wantChanged: false,
		},
		{
			name: "not_optional_skip",
			hcl:  `force_destroy = false`,
			attrs: map[string]attrSchema{
				"force_destroy": {Optional: false, Computed: false, Default: json.RawMessage(`false`)},
			},
			wantChanged: false,
		},
		{
			name: "match_removes",
			hcl:  `force_destroy = false`,
			attrs: map[string]attrSchema{
				"force_destroy": {Optional: true, Computed: false, Default: json.RawMessage(`false`)},
			},
			wantChanged: true,
		},
		{
			name: "empty_map_removes",
			hcl:  `tags = {}`,
			attrs: map[string]attrSchema{
				"tags": {Optional: true, Computed: false, Type: json.RawMessage(`["map","string"]`)},
			},
			wantChanged: true,
		},
		{
			name: "empty_list_removes",
			hcl:  `items = []`,
			attrs: map[string]attrSchema{
				"items": {Optional: true, Computed: false, Type: json.RawMessage(`["list","string"]`)},
			},
			wantChanged: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, diags := hclwrite.ParseConfig([]byte(tc.hcl), "test.hcl", hcl.Pos{Line: 1, Column: 1})
			if diags.HasErrors() {
				t.Fatal(diags.Error())
			}
			got := pruneBodyAttrs(f.Body(), tc.attrs)
			if got != tc.wantChanged {
				t.Errorf("pruneBodyAttrs changed = %v, want %v", got, tc.wantChanged)
			}
		})
	}
}

func TestFindTerraformFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "readme.md"), []byte(""), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, ".terraform"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".terraform", "skip.tf"), []byte(""), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".git", "skip.tf"), []byte(""), 0o644)

	files, err := findTerraformFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	if filepath.Base(files[0]) != "main.tf" {
		t.Errorf("expected main.tf, got %s", files[0])
	}
}

func TestEnsureTerraformInitialized(t *testing.T) {
	t.Run("not_initialized", func(t *testing.T) {
		dir := t.TempDir()
		err := ensureTerraformInitialized(dir)
		if err == nil {
			t.Error("expected error for uninitialized dir")
		}
	})

	t.Run("initialized", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(dir, ".terraform"), 0o755)
		err := ensureTerraformInitialized(dir)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
