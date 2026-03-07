package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// --- Provider schema JSON (minimum fields we use) ----------------------------

// terraform providers schema -json output (partial)
type providersSchemaJSON struct {
	ProviderSchemas map[string]providerSchema `json:"provider_schemas"`
}

type providerSchema struct {
	ResourceSchemas      map[string]blockSchema `json:"resource_schemas"`
	DataSourceSchemas    map[string]blockSchema `json:"data_source_schemas"`
	ProviderConfigSchema any                    `json:"provider"` // ignored
}

type blockSchema struct {
	Block nestedBlock `json:"block"`
}

type nestedBlock struct {
	Attributes map[string]attrSchema `json:"attributes"`
	// block_types omitted in minimal implementation
}

type attrSchema struct {
	Optional bool            `json:"optional"`
	Computed bool            `json:"computed"`
	Default  json.RawMessage `json:"default"`
	Type     json.RawMessage `json:"type"`
}

// --- CLI --------------------------------------------------------------------

type options struct {
	Dir   string
	Write bool
	Check bool
}

func main() {
	var opt options
	flag.StringVar(&opt.Dir, "dir", "", "directory to scan for .tf files (required)")
	flag.BoolVar(&opt.Write, "write", false, "rewrite files in-place")
	flag.BoolVar(&opt.Check, "check", false, "exit 1 if changes are needed (no write)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s -dir <directory> [options]\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "tfsimplify removes attributes from Terraform .tf files that match")
		fmt.Fprintln(os.Stderr, "the provider schema's default values.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Examples:")
		fmt.Fprintln(os.Stderr, "  tfsimplify -dir ./environments/prod")
		fmt.Fprintln(os.Stderr, "  tfsimplify -dir ./environments/prod --write")
		fmt.Fprintln(os.Stderr, "  tfsimplify -dir ./environments/prod --check")
	}

	flag.Parse()

	// Go's flag package stops parsing at the first non-flag argument.
	// Re-parse remaining args so that "tfsimplify . --check" works as expected.
	remaining := flag.Args()
	for len(remaining) > 0 {
		switch remaining[0] {
		case "--check", "-check":
			opt.Check = true
			remaining = remaining[1:]
		case "--write", "-write":
			opt.Write = true
			remaining = remaining[1:]
		default:
			// Treat the first non-flag remaining arg as the target directory.
			if opt.Dir == "" {
				opt.Dir = remaining[0]
			}
			remaining = remaining[1:]
		}
	}

	if opt.Dir == "" {
		fmt.Fprintln(os.Stderr, "error: -dir is required")
		fmt.Fprintln(os.Stderr)
		flag.Usage()
		os.Exit(2)
	}

	if opt.Write && opt.Check {
		fmt.Fprintln(os.Stderr, "error: cannot use --write and --check together")
		os.Exit(2)
	}

	awsSchema, err := loadAWSSchemaFromTerraform(opt.Dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error loading terraform provider schema:", err)
		os.Exit(2)
	}

	tfFiles, err := findTerraformFiles(opt.Dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error scanning directory:", err)
		os.Exit(2)
	}
	if len(tfFiles) == 0 {
		fmt.Println("no .tf files found")
		return
	}

	changedAny := false
	for _, path := range tfFiles {
		changed, err := processFile(path, awsSchema, opt.Write)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error processing %s: %v\n", path, err)
			os.Exit(2)
		}
		if changed {
			changedAny = true
			if !opt.Write {
				fmt.Println("would change:", path)
			} else {
				fmt.Println("updated:", path)
			}
		}
	}

	if opt.Check && changedAny {
		os.Exit(1)
	}
}

// --- Core logic --------------------------------------------------------------

type schemaIndex struct {
	Resource map[string]map[string]attrSchema // resourceType -> attrName -> schema
	Data     map[string]map[string]attrSchema // dataType     -> attrName -> schema
}

func loadAWSSchemaFromTerraform(dir string) (*schemaIndex, error) {
	if err := ensureTerraformInitialized(dir); err != nil {
		return nil, err
	}

	cmd := exec.Command("terraform", "providers", "schema", "-json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf(
			"failed to load provider schema in %s: %v\n%s",
			dir, err, string(out),
		)
	}

	var root providersSchemaJSON
	if err := json.Unmarshal(out, &root); err != nil {
		return nil, err
	}

	awsProv, ok := root.ProviderSchemas["registry.terraform.io/hashicorp/aws"]
	if !ok {
		for k, v := range root.ProviderSchemas {
			if strings.HasSuffix(k, "/aws") {
				awsProv = v
				ok = true
				break
			}
		}
	}
	if !ok {
		return nil, fmt.Errorf("aws provider schema not found in %s", dir)
	}

	idx := &schemaIndex{
		Resource: make(map[string]map[string]attrSchema),
		Data:     make(map[string]map[string]attrSchema),
	}
	for rType, rs := range awsProv.ResourceSchemas {
		idx.Resource[rType] = rs.Block.Attributes
	}
	for dType, ds := range awsProv.DataSourceSchemas {
		idx.Data[dType] = ds.Block.Attributes
	}

	return idx, nil
}

func ensureTerraformInitialized(dir string) error {
	tfDir := filepath.Join(dir, ".terraform")
	st, err := os.Stat(tfDir)
	if err != nil || !st.IsDir() {
		return fmt.Errorf(
			"terraform is not initialized in %s: .terraform directory not found; run `terraform init` first",
			dir,
		)
	}
	return nil
}

func findTerraformFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// skip .terraform, vendor-like dirs
		if d.IsDir() {
			base := filepath.Base(path)
			if base == ".terraform" || base == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".tf") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func processFile(path string, idx *schemaIndex, write bool) (bool, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	f, diags := hclwrite.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return false, fmt.Errorf("parse %s: %s", path, diags.Error())
	}
	body := f.Body()

	changed := false

	// We only touch top-level resource/data blocks in this minimal version.
	for _, block := range body.Blocks() {
		switch block.Type() {
		case "resource":
			labels := block.Labels()
			if len(labels) < 1 {
				continue
			}
			rType := labels[0]
			attrs, ok := idx.Resource[rType]
			if !ok {
				continue
			}
			if pruneBodyAttrs(block.Body(), attrs) {
				changed = true
			}
		case "data":
			labels := block.Labels()
			if len(labels) < 1 {
				continue
			}
			dType := labels[0]
			attrs, ok := idx.Data[dType]
			if !ok {
				continue
			}
			if pruneBodyAttrs(block.Body(), attrs) {
				changed = true
			}
		default:
			// ignore
		}
	}

	if !changed {
		return false, nil
	}

	formatted := f.BuildTokens(nil).Bytes()

	// If you prefer: formatted := f.Bytes()
	// BuildTokens tends to preserve formatting decisions; either is ok.

	// Avoid rewriting if unchanged bytes (rare, but safe)
	if bytes.Equal(src, formatted) {
		return false, nil
	}

	if write {
		return true, os.WriteFile(path, formatted, 0o644)
	}
	return true, nil
}

func pruneBodyAttrs(b *hclwrite.Body, schemaAttrs map[string]attrSchema) bool {
	changed := false

	for name, attr := range b.Attributes() {
		s, ok := schemaAttrs[name]
		if !ok {
			continue
		}

		// Safety gates
		if !s.Optional || s.Computed {
			continue
		}
		// If no explicit default, infer zero value from type for optional, non-computed attrs
		if len(bytes.TrimSpace(s.Default)) == 0 {
			if zd := zeroDefault(s.Type); zd != nil {
				s.Default = zd
			} else {
				continue // default unknown/missing
			}
		}

		// Only literal values (no references/functions/templating)
		v, ok := evalLiteralExprToGo(attr.Expr().BuildTokens(nil).Bytes())
		if !ok {
			continue
		}
		// Do not delete null (often changes semantics)
		if v == nil {
			continue
		}

		// Parse schema default to Go value
		var def any
		if err := json.Unmarshal(s.Default, &def); err != nil {
			continue
		}
		// Also avoid null defaults for now
		if def == nil {
			continue
		}

		if deepEqualJSONish(v, def) {
			b.RemoveAttribute(name)
			changed = true
		}
	}
	return changed
}

// zeroDefault returns the JSON-encoded zero value for a primitive type schema.
// For "bool" -> false, for "number" -> 0, for "string" -> "".
// Returns nil if the type is not a simple primitive.
func zeroDefault(rawType json.RawMessage) json.RawMessage {
	if len(rawType) == 0 {
		return nil
	}
	// Try simple string type first (e.g. "bool", "number", "string")
	var t string
	if err := json.Unmarshal(rawType, &t); err == nil {
		switch t {
		case "bool":
			return json.RawMessage(`false`)
		case "number":
			return json.RawMessage(`0`)
		case "string":
			return json.RawMessage(`""`)
		default:
			return nil
		}
	}
	// Try complex type (e.g. ["map", "string"], ["list", "string"], ["set", "string"])
	var arr []json.RawMessage
	if err := json.Unmarshal(rawType, &arr); err != nil || len(arr) == 0 {
		return nil
	}
	var kind string
	if err := json.Unmarshal(arr[0], &kind); err != nil {
		return nil
	}
	switch kind {
	case "map", "object":
		return json.RawMessage(`{}`)
	case "list", "set", "tuple":
		return json.RawMessage(`[]`)
	default:
		return nil
	}
}

// evalLiteralExprToGo returns a Go representation of a literal HCL expression
// from its raw token bytes (as produced by hclwrite).
// - string, float64, bool, nil, []any, map[string]any
// If expression contains variables, traversals, function calls, etc -> false.
func evalLiteralExprToGo(exprBytes []byte) (any, bool) {
	// Parse the expression bytes using hclsyntax to get a real hcl.Expression.
	expr, diags := hclsyntax.ParseExpression(exprBytes, "expr.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, false
	}
	switch e := expr.(type) {
	case *hclsyntax.LiteralValueExpr:
		return ctyValueToGo(e.Val)
	case *hclsyntax.TemplateExpr:
		// A quoted string like "hello" is parsed as a TemplateExpr with a single LiteralValueExpr part.
		if len(e.Parts) == 1 {
			if lit, ok := e.Parts[0].(*hclsyntax.LiteralValueExpr); ok {
				return ctyValueToGo(lit.Val)
			}
		}
		return nil, false
	case *hclsyntax.ObjectConsExpr:
		// Empty object literal {}
		if len(e.Items) == 0 {
			return map[string]any{}, true
		}
		return nil, false
	case *hclsyntax.TupleConsExpr:
		// Empty list/tuple literal []
		if len(e.Exprs) == 0 {
			return []any{}, true
		}
		return nil, false
	default:
		// Not a pure literal. Reject in minimal version.
		return nil, false
	}
}

// ctyValueToGo converts a cty.Value to a Go native type.
func ctyValueToGo(v cty.Value) (any, bool) {
	if v.IsNull() {
		return nil, true
	}

	ty := v.Type()
	switch ty {
	case cty.Bool:
		return v.True(), true
	case cty.Number:
		bf := v.AsBigFloat()
		f, _ := bf.Float64()
		return f, true
	case cty.String:
		return v.AsString(), true
	default:
		return nil, false
	}
}

// deepEqualJSONish compares decoded JSON-ish values (maps/slices/bool/float64/string/nil).
// We normalize numbers: JSON unmarshal makes them float64; HCL literal roundtrip also float64.
func deepEqualJSONish(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !deepEqualJSONish(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqualJSONish(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		// primitives (string/bool/float64) and nil
		return fmt.Sprintf("%v|%T", a, a) == fmt.Sprintf("%v|%T", b, b)
	}
}
