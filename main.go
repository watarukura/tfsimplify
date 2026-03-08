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

	schema, err := loadProviderSchemas(opt.Dir)
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
		changed, original, formatted, err := processFile(path, schema, opt.Write)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error processing %s: %v\n", path, err)
			os.Exit(2)
		}
		if changed {
			changedAny = true
			if opt.Check {
				d, _ := unifiedDiff(path, original, formatted)
				fmt.Print(d)
			} else if opt.Write {
				fmt.Println("updated:", path)
			} else {
				fmt.Println("would change:", path)
			}
		}
	}

	if opt.Check && changedAny {
		os.Exit(1)
	}
}

// unifiedDiff generates a unified diff using the external diff command.
func unifiedDiff(path string, original, modified []byte) (string, error) {
	tmpOrig, err := os.CreateTemp("", "tfsimplify-orig-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmpOrig.Name()) }()

	tmpMod, err := os.CreateTemp("", "tfsimplify-mod-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmpMod.Name()) }()

	if _, err := tmpOrig.Write(original); err != nil {
		return "", err
	}
	_ = tmpOrig.Close()

	if _, err := tmpMod.Write(modified); err != nil {
		return "", err
	}
	_ = tmpMod.Close()

	cmd := exec.Command("diff", "-u",
		"--label", path, "--label", path,
		tmpOrig.Name(), tmpMod.Name())
	out, _ := cmd.CombinedOutput() // diff exits 1 when files differ
	return string(out), nil
}

// --- Core logic --------------------------------------------------------------

type schemaIndex struct {
	Resource map[string]map[string]attrSchema // resourceType -> attrName -> schema
	Data     map[string]map[string]attrSchema // dataType     -> attrName -> schema
}

func loadProviderSchemas(dir string) (*schemaIndex, error) {
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

	if len(root.ProviderSchemas) == 0 {
		return nil, fmt.Errorf("no provider schemas found in %s", dir)
	}

	idx := &schemaIndex{
		Resource: make(map[string]map[string]attrSchema),
		Data:     make(map[string]map[string]attrSchema),
	}
	for _, prov := range root.ProviderSchemas {
		for rType, rs := range prov.ResourceSchemas {
			idx.Resource[rType] = rs.Block.Attributes
		}
		for dType, ds := range prov.DataSourceSchemas {
			idx.Data[dType] = ds.Block.Attributes
		}
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
	visited := make(map[string]bool)
	var files []string
	if err := collectTerraformFiles(dir, visited, &files); err != nil {
		return nil, err
	}
	return files, nil
}

func collectTerraformFiles(dir string, visited map[string]bool, files *[]string) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if visited[absDir] {
		return nil
	}
	visited[absDir] = true

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
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
			*files = append(*files, path)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Find local module sources and collect their files too
	moduleDirs, err := findLocalModuleSources(dir)
	if err != nil {
		return err
	}
	for _, modDir := range moduleDirs {
		if err := collectTerraformFiles(modDir, visited, files); err != nil {
			return err
		}
	}
	return nil
}

// findLocalModuleSources parses .tf files in dir and returns resolved paths
// of local module sources (those starting with "./" or "../").
func findLocalModuleSources(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var dirs []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		f, diags := hclwrite.ParseConfig(src, e.Name(), hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			continue
		}
		for _, block := range f.Body().Blocks() {
			if block.Type() != "module" {
				continue
			}
			sourceAttr := block.Body().GetAttribute("source")
			if sourceAttr == nil {
				continue
			}
			// Extract string value from source attribute tokens
			sourceVal := extractStringLiteral(sourceAttr.BuildTokens(nil).Bytes())
			if sourceVal == "" {
				continue
			}
			if !strings.HasPrefix(sourceVal, "./") && !strings.HasPrefix(sourceVal, "../") {
				continue
			}
			resolved := filepath.Join(dir, sourceVal)
			abs, err := filepath.Abs(resolved)
			if err != nil {
				continue
			}
			if seen[abs] {
				continue
			}
			// Verify it's a directory
			info, err := os.Stat(abs)
			if err != nil || !info.IsDir() {
				continue
			}
			seen[abs] = true
			dirs = append(dirs, abs)
		}
	}
	return dirs, nil
}

// extractStringLiteral extracts a string value from HCL attribute bytes like: source = "../module/"
func extractStringLiteral(b []byte) string {
	s := strings.TrimSpace(string(b))
	// Format: name = "value"\n
	idx := strings.Index(s, "=")
	if idx < 0 {
		return ""
	}
	val := strings.TrimSpace(s[idx+1:])
	if len(val) >= 2 && val[0] == '"' {
		end := strings.LastIndex(val, "\"")
		if end > 0 {
			return val[1:end]
		}
	}
	return ""
}

func processFile(path string, idx *schemaIndex, write bool) (bool, []byte, []byte, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return false, nil, nil, err
	}

	// Use hclwrite to identify removable attributes, then use hclsyntax to get
	// their source ranges so we can remove lines from the original source,
	// preserving the formatting of remaining lines.

	wf, wdiags := hclwrite.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
	if wdiags.HasErrors() {
		return false, nil, nil, fmt.Errorf("parse %s: %s", path, wdiags.Error())
	}

	sf, sdiags := hclsyntax.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
	if sdiags.HasErrors() {
		return false, nil, nil, fmt.Errorf("parse %s: %s", path, sdiags.Error())
	}
	syntaxBody := sf.Body.(*hclsyntax.Body)

	// Collect attribute names to remove per block index.
	type blockKey struct {
		typ    string
		labels string // joined labels
	}
	toRemove := make(map[blockKey]map[string]bool)

	for _, block := range wf.Body().Blocks() {
		var schemaAttrs map[string]attrSchema
		switch block.Type() {
		case "resource":
			labels := block.Labels()
			if len(labels) < 1 {
				continue
			}
			attrs, ok := idx.Resource[labels[0]]
			if !ok {
				continue
			}
			schemaAttrs = attrs
		case "data":
			labels := block.Labels()
			if len(labels) < 1 {
				continue
			}
			attrs, ok := idx.Data[labels[0]]
			if !ok {
				continue
			}
			schemaAttrs = attrs
		default:
			continue
		}
		key := blockKey{typ: block.Type(), labels: strings.Join(block.Labels(), "/")}
		for name, attr := range block.Body().Attributes() {
			if shouldPrune(name, attr, schemaAttrs) {
				if toRemove[key] == nil {
					toRemove[key] = make(map[string]bool)
				}
				toRemove[key][name] = true
			}
		}
	}

	if len(toRemove) == 0 {
		return false, nil, nil, nil
	}

	// Now use hclsyntax to find source line ranges for those attributes.
	removeLines := make(map[int]bool)
	for _, block := range syntaxBody.Blocks {
		key := blockKey{typ: block.Type, labels: strings.Join(block.Labels, "/")}
		names, ok := toRemove[key]
		if !ok {
			continue
		}
		for name, attr := range block.Body.Attributes {
			if !names[name] {
				continue
			}
			for line := attr.SrcRange.Start.Line; line <= attr.SrcRange.End.Line; line++ {
				removeLines[line] = true
			}
		}
	}

	if len(removeLines) == 0 {
		return false, nil, nil, nil
	}

	// Build set of lines protected by ignore/disable directives.
	// - "# tfsimplify-ignore" on line N protects line N+1 from removal.
	// - Lines between "# tfsimplify-disable" and "# tfsimplify-enable" are protected.
	disabledLines := make(map[int]bool)
	srcLines := strings.Split(string(src), "\n")
	inDisableBlock := false
	for i, line := range srcLines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "# tfsimplify-disable") {
			inDisableBlock = true
			continue
		}
		if strings.Contains(trimmed, "# tfsimplify-enable") {
			inDisableBlock = false
			continue
		}
		if inDisableBlock {
			disabledLines[i+1] = true // 1-based
		}
		if strings.Contains(trimmed, "# tfsimplify-ignore") {
			disabledLines[i+2] = true // protect next line (1-based)
		}
	}

	// Exclude disabled lines from removal.
	for line := range disabledLines {
		delete(removeLines, line)
	}

	if len(removeLines) == 0 {
		return false, nil, nil, nil
	}

	// Remove lines from original source.
	lines := strings.Split(string(src), "\n")
	var result []string
	for i, line := range lines {
		lineNum := i + 1 // 1-based
		if !removeLines[lineNum] {
			result = append(result, line)
		}
	}
	formatted := []byte(strings.Join(result, "\n"))

	if bytes.Equal(src, formatted) {
		return false, nil, nil, nil
	}

	if write {
		return true, src, formatted, os.WriteFile(path, formatted, 0o644)
	}
	return true, src, formatted, nil
}

// pruneBodyAttrs removes attributes from the body that match their schema defaults.
func pruneBodyAttrs(b *hclwrite.Body, schemaAttrs map[string]attrSchema) bool {
	changed := false
	for name, attr := range b.Attributes() {
		if shouldPrune(name, attr, schemaAttrs) {
			b.RemoveAttribute(name)
			changed = true
		}
	}
	return changed
}

// shouldPrune returns true if the given attribute should be removed because
// its value matches the provider schema default.
func shouldPrune(name string, attr *hclwrite.Attribute, schemaAttrs map[string]attrSchema) bool {
	s, ok := schemaAttrs[name]
	if !ok {
		return false
	}

	if !s.Optional || s.Computed {
		return false
	}
	if len(bytes.TrimSpace(s.Default)) == 0 {
		if zd := zeroDefault(s.Type); zd != nil {
			s.Default = zd
		} else {
			return false
		}
	}

	v, ok := evalLiteralExprToGo(attr.Expr().BuildTokens(nil).Bytes())
	if !ok {
		return false
	}
	if v == nil {
		return false
	}

	var def any
	if err := json.Unmarshal(s.Default, &def); err != nil {
		return false
	}
	if def == nil {
		return false
	}

	return deepEqualJSONish(v, def)
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
