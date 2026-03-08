package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
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

func TestProcessFileDisableComment(t *testing.T) {
	schema := testSchemaIndex("aws_s3_bucket", map[string]attrSchema{
		"force_destroy": {
			Optional: true,
			Computed: false,
			Default:  json.RawMessage(`false`),
		},
	})

	cases := []struct {
		name    string
		input   string
		want    string
		changed bool
	}{
		{
			name: "ignore_preserves_next_line",
			input: `resource "aws_s3_bucket" "example" {
  # tfsimplify-ignore
  force_destroy = false
}
`,
			want: `resource "aws_s3_bucket" "example" {
  # tfsimplify-ignore
  force_destroy = false
}
`,
			changed: false,
		},
		{
			name: "disable_enable_preserves_range",
			input: `resource "aws_s3_bucket" "example" {
  # tfsimplify-disable
  force_destroy = false
  # tfsimplify-enable
}
`,
			want: `resource "aws_s3_bucket" "example" {
  # tfsimplify-disable
  force_destroy = false
  # tfsimplify-enable
}
`,
			changed: false,
		},
		{
			name: "without_directive_removes",
			input: `resource "aws_s3_bucket" "example" {
  force_destroy = false
}
`,
			want: `resource "aws_s3_bucket" "example" {
}
`,
			changed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.tf")
			if err := os.WriteFile(path, []byte(tc.input), 0o644); err != nil {
				t.Fatal(err)
			}
			changed, _, _, err := processFile(path, schema, false)
			if err != nil {
				t.Fatal(err)
			}
			if changed != tc.changed {
				t.Errorf("changed = %v, want %v", changed, tc.changed)
			}
			if changed {
				// Re-run with write to check output
				_, _, formatted, err := processFile(path, schema, false)
				if err != nil {
					t.Fatal(err)
				}
				if string(formatted) != tc.want {
					t.Errorf("got:\n%s\nwant:\n%s", string(formatted), tc.want)
				}
			}
		})
	}
}

func TestExtractStringLiteral(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", `source = "../module/"`, "../module/"},
		{"dotslash", `source = "./local"`, "./local"},
		{"no_equals", `source`, ""},
		{"no_quotes", `source = noquotes`, ""},
		{"empty_string", `source = ""`, ""},
		{"registry", `source = "hashicorp/consul/aws"`, "hashicorp/consul/aws"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractStringLiteral([]byte(tc.input))
			if got != tc.want {
				t.Errorf("extractStringLiteral(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestUnifiedDiff(t *testing.T) {
	original := []byte("line1\nline2\nline3\n")
	modified := []byte("line1\nline3\n")

	diff, err := unifiedDiff("test.tf", original, modified)
	if err != nil {
		t.Fatal(err)
	}
	if diff == "" {
		t.Error("expected non-empty diff")
	}

	// No diff case
	diff2, err := unifiedDiff("test.tf", original, original)
	if err != nil {
		t.Fatal(err)
	}
	if diff2 != "" {
		t.Errorf("expected empty diff for identical content, got: %s", diff2)
	}
}

func TestZeroDefault(t *testing.T) {
	tests := []struct {
		name    string
		rawType string
		want    string
	}{
		{"bool", `"bool"`, `false`},
		{"number", `"number"`, `0`},
		{"string", `"string"`, `""`},
		{"unknown", `"dynamic"`, ""},
		{"empty", ``, ""},
		{"map_string", `["map","string"]`, `{}`},
		{"list_string", `["list","string"]`, `[]`},
		{"set_string", `["set","string"]`, `[]`},
		{"object", `["object",{"a":"string"}]`, `{}`},
		{"tuple", `["tuple",["string"]]`, `[]`},
		{"unknown_complex", `["unknown","string"]`, ""},
		{"invalid_json", `{bad`, ""},
		{"empty_array", `[]`, ""},
		{"non_string_kind", `[123]`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := zeroDefault(json.RawMessage(tc.rawType))
			gotStr := ""
			if got != nil {
				gotStr = string(got)
			}
			if gotStr != tc.want {
				t.Errorf("zeroDefault(%s) = %q, want %q", tc.rawType, gotStr, tc.want)
			}
		})
	}
}

func TestFindLocalModuleSources(t *testing.T) {
	dir := t.TempDir()
	modDir := filepath.Join(dir, "modules", "mymod")
	_ = os.MkdirAll(modDir, 0o755)
	_ = os.WriteFile(filepath.Join(modDir, "main.tf"), []byte(""), 0o644)

	// Write a .tf file with a local module source
	tfContent := `
module "mymod" {
  source = "./modules/mymod"
}

module "remote" {
  source = "hashicorp/consul/aws"
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tfContent), 0o644)

	dirs, err := findLocalModuleSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 {
		t.Fatalf("expected 1 local module dir, got %d: %v", len(dirs), dirs)
	}

	// Test with non-existent module source
	tfContent2 := `
module "missing" {
  source = "./nonexistent"
}
`
	dir2 := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir2, "main.tf"), []byte(tfContent2), 0o644)
	dirs2, err := findLocalModuleSources(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs2) != 0 {
		t.Errorf("expected 0 dirs for nonexistent module, got %d", len(dirs2))
	}

	// Test with invalid HCL
	dir3 := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir3, "bad.tf"), []byte("{{invalid"), 0o644)
	dirs3, err := findLocalModuleSources(dir3)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs3) != 0 {
		t.Errorf("expected 0 dirs for invalid HCL, got %d", len(dirs3))
	}
}

func TestCollectTerraformFilesWithModules(t *testing.T) {
	dir := t.TempDir()
	modDir := filepath.Join(dir, "modules", "sub")
	_ = os.MkdirAll(modDir, 0o755)
	_ = os.WriteFile(filepath.Join(modDir, "sub.tf"), []byte(""), 0o644)

	tfContent := `
module "sub" {
  source = "./modules/sub"
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tfContent), 0o644)

	files, err := findTerraformFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The walk finds main.tf and modules/sub/sub.tf, and module resolution
	// may also find sub.tf again (appended before visited check in walk).
	if len(files) < 2 {
		t.Errorf("expected at least 2 files, got %d: %v", len(files), files)
	}
}

func TestProcessFileWithDataSource(t *testing.T) {
	schema := &schemaIndex{
		Resource: make(map[string]map[string]attrSchema),
		Data: map[string]map[string]attrSchema{
			"aws_ami": {
				"most_recent": {
					Optional: true,
					Computed: false,
					Default:  json.RawMessage(`false`),
				},
			},
		},
	}

	dir := t.TempDir()
	input := `data "aws_ami" "example" {
  most_recent = false
}
`
	path := filepath.Join(dir, "main.tf")
	_ = os.WriteFile(path, []byte(input), 0o644)

	changed, _, _, err := processFile(path, schema, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected data source attribute to be pruned")
	}
}

func TestProcessFileWrite(t *testing.T) {
	schema := testSchemaIndex("aws_s3_bucket", map[string]attrSchema{
		"force_destroy": {
			Optional: true,
			Computed: false,
			Default:  json.RawMessage(`false`),
		},
	})

	dir := t.TempDir()
	input := `resource "aws_s3_bucket" "example" {
  force_destroy = false
}
`
	path := filepath.Join(dir, "main.tf")
	_ = os.WriteFile(path, []byte(input), 0o644)

	changed, _, _, err := processFile(path, schema, true)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected change")
	}

	// Verify file was written
	content, _ := os.ReadFile(path)
	expected := `resource "aws_s3_bucket" "example" {
}
`
	if string(content) != expected {
		t.Errorf("written file mismatch:\ngot:\n%s\nwant:\n%s", string(content), expected)
	}
}

func TestProcessFileParseError(t *testing.T) {
	schema := testSchemaIndex("aws_s3_bucket", map[string]attrSchema{})
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.tf")
	_ = os.WriteFile(path, []byte("{{invalid"), 0o644)

	_, _, _, err := processFile(path, schema, false)
	if err == nil {
		t.Error("expected parse error")
	}
}

func TestProcessFileNoChanges(t *testing.T) {
	schema := testSchemaIndex("aws_s3_bucket", map[string]attrSchema{
		"force_destroy": {
			Optional: true,
			Computed: false,
			Default:  json.RawMessage(`false`),
		},
	})

	dir := t.TempDir()
	input := `resource "aws_s3_bucket" "example" {
  force_destroy = true
}
`
	path := filepath.Join(dir, "main.tf")
	_ = os.WriteFile(path, []byte(input), 0o644)

	changed, _, _, err := processFile(path, schema, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("expected no change")
	}
}

func TestProcessFileUnknownResource(t *testing.T) {
	schema := testSchemaIndex("aws_s3_bucket", map[string]attrSchema{})

	dir := t.TempDir()
	input := `resource "aws_unknown_resource" "example" {
  some_attr = "value"
}
`
	path := filepath.Join(dir, "main.tf")
	_ = os.WriteFile(path, []byte(input), 0o644)

	changed, _, _, err := processFile(path, schema, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("expected no change for unknown resource")
	}
}

func TestFindLocalModuleSourcesDuplicateAndParent(t *testing.T) {
	dir := t.TempDir()
	modDir := filepath.Join(dir, "mod")
	_ = os.MkdirAll(modDir, 0o755)
	_ = os.WriteFile(filepath.Join(modDir, "main.tf"), []byte(""), 0o644)

	// Two modules pointing to the same source
	tfContent := `
module "a" {
  source = "./mod"
}

module "b" {
  source = "./mod"
}

module "no_source" {
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tfContent), 0o644)

	dirs, err := findLocalModuleSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 {
		t.Errorf("expected 1 unique dir, got %d: %v", len(dirs), dirs)
	}
}

func TestCtyValueToGo(t *testing.T) {
	// Test null value
	val, ok := ctyValueToGo(cty.NullVal(cty.String))
	if !ok {
		t.Error("expected ok for null")
	}
	if val != nil {
		t.Errorf("expected nil for null, got %v", val)
	}

	// Test unsupported type
	_, ok = ctyValueToGo(cty.ListValEmpty(cty.String))
	if ok {
		t.Error("expected not ok for list type")
	}
}

func TestDeepEqualJSONishMapLenMismatch(t *testing.T) {
	a := map[string]any{"k": "v", "k2": "v2"}
	b := map[string]any{"k": "v"}
	if deepEqualJSONish(a, b) {
		t.Error("expected false for maps with different lengths")
	}
}

func TestDeepEqualJSONishSliceLenMismatch(t *testing.T) {
	a := []any{"a", "b"}
	b := []any{"a"}
	if deepEqualJSONish(a, b) {
		t.Error("expected false for slices with different lengths")
	}
}

func TestParseProviderSchemas(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		data := []byte(`{
			"provider_schemas": {
				"registry.terraform.io/hashicorp/aws": {
					"resource_schemas": {
						"aws_s3_bucket": {
							"block": {
								"attributes": {
									"force_destroy": {
										"optional": true,
										"computed": false,
										"default": false
									}
								}
							}
						}
					},
					"data_source_schemas": {
						"aws_ami": {
							"block": {
								"attributes": {
									"most_recent": {
										"optional": true,
										"computed": false
									}
								}
							}
						}
					}
				}
			}
		}`)
		idx, err := parseProviderSchemas(data, "/tmp")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := idx.Resource["aws_s3_bucket"]; !ok {
			t.Error("expected aws_s3_bucket in resource schemas")
		}
		if _, ok := idx.Data["aws_ami"]; !ok {
			t.Error("expected aws_ami in data source schemas")
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		_, err := parseProviderSchemas([]byte("{bad"), "/tmp")
		if err == nil {
			t.Error("expected error for invalid JSON")
		}
	})

	t.Run("empty_providers", func(t *testing.T) {
		_, err := parseProviderSchemas([]byte(`{"provider_schemas":{}}`), "/tmp")
		if err == nil {
			t.Error("expected error for empty provider schemas")
		}
	})
}

func TestRunNoTerraformInit(t *testing.T) {
	dir := t.TempDir()
	code := run(options{Dir: dir}, loadProviderSchemas)
	if code != 2 {
		t.Errorf("expected exit code 2, got %d", code)
	}
}

func TestRunWithMockSchema(t *testing.T) {
	mockLoader := func(dir string) (*schemaIndex, error) {
		return testSchemaIndex("aws_s3_bucket", map[string]attrSchema{
			"force_destroy": {
				Optional: true,
				Computed: false,
				Default:  json.RawMessage(`false`),
			},
		}), nil
	}

	t.Run("no_tf_files", func(t *testing.T) {
		dir := t.TempDir()
		code := run(options{Dir: dir}, mockLoader)
		if code != 0 {
			t.Errorf("expected exit code 0, got %d", code)
		}
	})

	t.Run("no_changes", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "aws_s3_bucket" "b" {
  force_destroy = true
}
`), 0o644)
		code := run(options{Dir: dir}, mockLoader)
		if code != 0 {
			t.Errorf("expected exit code 0, got %d", code)
		}
	})

	t.Run("dry_run_changes", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "aws_s3_bucket" "b" {
  force_destroy = false
}
`), 0o644)
		code := run(options{Dir: dir}, mockLoader)
		if code != 0 {
			t.Errorf("expected exit code 0, got %d", code)
		}
	})

	t.Run("write_mode", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "aws_s3_bucket" "b" {
  force_destroy = false
}
`), 0o644)
		code := run(options{Dir: dir, Write: true}, mockLoader)
		if code != 0 {
			t.Errorf("expected exit code 0, got %d", code)
		}
	})

	t.Run("check_mode_with_changes", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "aws_s3_bucket" "b" {
  force_destroy = false
}
`), 0o644)
		code := run(options{Dir: dir, Check: true}, mockLoader)
		if code != 1 {
			t.Errorf("expected exit code 1, got %d", code)
		}
	})

	t.Run("check_mode_no_changes", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "aws_s3_bucket" "b" {
  force_destroy = true
}
`), 0o644)
		code := run(options{Dir: dir, Check: true}, mockLoader)
		if code != 0 {
			t.Errorf("expected exit code 0, got %d", code)
		}
	})

	t.Run("schema_loader_error", func(t *testing.T) {
		errLoader := func(dir string) (*schemaIndex, error) {
			return nil, fmt.Errorf("mock error")
		}
		dir := t.TempDir()
		code := run(options{Dir: dir}, errLoader)
		if code != 2 {
			t.Errorf("expected exit code 2, got %d", code)
		}
	})

	t.Run("invalid_dir", func(t *testing.T) {
		code := run(options{Dir: "/nonexistent/path"}, mockLoader)
		if code != 2 {
			t.Errorf("expected exit code 2, got %d", code)
		}
	})

	t.Run("process_error", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "bad.tf"), []byte("{{invalid"), 0o644)
		code := run(options{Dir: dir}, mockLoader)
		if code != 2 {
			t.Errorf("expected exit code 2, got %d", code)
		}
	})
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
