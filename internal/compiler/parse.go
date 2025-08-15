package compiler

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sqlc-dev/sqlc/internal/config"
	"github.com/sqlc-dev/sqlc/internal/debug"
	"github.com/sqlc-dev/sqlc/internal/metadata"
	"github.com/sqlc-dev/sqlc/internal/opts"
	"github.com/sqlc-dev/sqlc/internal/source"
	"github.com/sqlc-dev/sqlc/internal/sql/ast"
	"github.com/sqlc-dev/sqlc/internal/sql/astutils"
	"github.com/sqlc-dev/sqlc/internal/sql/rewrite"
	"github.com/sqlc-dev/sqlc/internal/sql/validate"

	"github.com/sqlc-dev/sqlc/internal/codegen/golang"
)

var ErrUnsupportedStatementType = errors.New("parseQuery: unsupported statement type")

func (c *Compiler) parseQuery(stmt ast.Node, src string, o opts.Parser) (*Query, error) {
	if o.Debug.DumpAST {
		debug.Dump(stmt)
	}
	if err := validate.ParamStyle(stmt); err != nil {
		return nil, err
	}
	numbers, dollar, err := validate.ParamRef(stmt)
	if err != nil {
		return nil, err
	}
	raw, ok := stmt.(*ast.RawStmt)
	if !ok {
		return nil, errors.New("node is not a statement")
	}
	var table *ast.TableName
	switch n := raw.Stmt.(type) {
	case *ast.CallStmt:
	case *ast.SelectStmt:
	case *ast.DeleteStmt:
	case *ast.InsertStmt:
		if err := validate.InsertStmt(n); err != nil {
			return nil, err
		}
		var err error
		table, err = ParseTableName(n.Relation)
		if err != nil {
			return nil, err
		}
	case *ast.ListenStmt:
	case *ast.NotifyStmt:
	case *ast.TruncateStmt:
	case *ast.UpdateStmt:
	case *ast.RefreshMatViewStmt:
	default:
		return nil, ErrUnsupportedStatementType
	}

	rawSQL, err := source.Pluck(src, raw.StmtLocation, raw.StmtLen)
	if err != nil {
		return nil, err
	}
	if rawSQL == "" {
		return nil, errors.New("missing semicolon at end of file")
	}
	if err := validate.FuncCall(c.catalog, c.combo, raw); err != nil {
		return nil, err
	}
	if err := validate.In(c.catalog, raw); err != nil {
		return nil, err
	}
	queryConfig, err := metadata.ParseQueryNameAndType(strings.TrimSpace(rawSQL), c.parser.CommentSyntax())
	if err != nil {
		return nil, err
	}
	raw, namedParams, edits := rewrite.NamedParameters(c.conf.Engine, raw, numbers, dollar)
	if err := validate.Cmd(
		raw.Stmt, queryConfig.Name, queryConfig.Cmd, queryConfig.Options); err != nil {
		return nil, err
	}
	err = validateAndSetDefaultOptions(
		raw.Stmt, queryConfig.Name, queryConfig.Cmd, queryConfig.Options)
	if err != nil {
		return nil, err
	}
	rvs := rangeVars(raw.Stmt)
	refs, err := findParameters(raw.Stmt)
	if err != nil {
		return nil, err
	}
	refs = uniqueParamRefs(refs, dollar)
	if c.conf.Engine == config.EngineMySQL || !dollar {
		sort.Slice(refs, func(i, j int) bool { return refs[i].ref.Location < refs[j].ref.Location })
	} else {
		sort.Slice(refs, func(i, j int) bool { return refs[i].ref.Number < refs[j].ref.Number })
	}
	raw, embeds := rewrite.Embeds(raw)
	qc, err := c.buildQueryCatalog(c.catalog, raw.Stmt, embeds)
	if err != nil {
		return nil, err
	}

	params, err := c.resolveCatalogRefs(qc, rvs, refs, namedParams, embeds)
	if err != nil {
		return nil, err
	}
	cols, err := c.outputColumns(qc, raw.Stmt)
	if err != nil {
		return nil, err
	}

	expandEdits, err := c.expand(qc, raw)
	if err != nil {
		return nil, err
	}
	edits = append(edits, expandEdits...)
	expanded, err := source.Mutate(rawSQL, edits)
	if err != nil {
		return nil, err
	}

	// If the query string was edited, make sure the syntax is valid
	if expanded != rawSQL {
		if _, err := c.parser.Parse(strings.NewReader(expanded)); err != nil {
			return nil, fmt.Errorf("edited query syntax is invalid: %w", err)
		}
	}

	trimmed, comments, err := source.StripComments(expanded)
	if err != nil {
		return nil, err
	}

	flags, err := metadata.ParseQueryFlags(comments)
	if err != nil {
		return nil, err
	}

	return &Query{
		RawStmt:         raw,
		Cmd:             queryConfig.Cmd,
		Comments:        comments,
		Name:            queryConfig.Name,
		Flags:           flags,
		Options:         queryConfig.Options,
		Params:          params,
		Columns:         cols,
		SQL:             trimmed,
		InsertIntoTable: table,
	}, nil
}

func rangeVars(root ast.Node) []*ast.RangeVar {
	var vars []*ast.RangeVar
	find := astutils.VisitorFunc(func(node ast.Node) {
		switch n := node.(type) {
		case *ast.RangeVar:
			vars = append(vars, n)
		}
	})
	astutils.Walk(find, root)
	return vars
}

// scoreParamRefForTypeInference scores a parameter reference based on how good
// its context is for type inference. Higher scores indicate better contexts.
func scoreParamRefForTypeInference(ref paramRef) int {
	if ref.parent == nil {
		return 0 // No context
	}

	switch parent := ref.parent.(type) {
	case *ast.TypeCast:
		// Explicit type cast - excellent for type inference
		return 100

	case *ast.A_Expr:
		// Expression context - quality depends on the operator
		if parent.Name != nil && len(parent.Name.Items) > 0 {
			if nameStr, ok := parent.Name.Items[0].(*ast.String); ok {
				switch nameStr.Str {
				case "=", "==", "!=", "<>", "<", "<=", ">", ">=":
					// Comparison operations - very good for type inference
					return 100
				case "+", "-", "*", "/", "%":
					// Mathematical operations - good for type inference
					return 90
				case "||":
					// String concatenation - good for type inference
					return 90
				case "~~", "!~~", "~~*", "!~~*":
					// LIKE operations - good for type inference
					return 90
				case "IS", "IS NOT":
					// IS NULL/IS NOT NULL - poor for type inference
					return 0
				default:
					return 50
				}
			}
		}
		return 50 // Default for A_Expr without clear operator

	case *ast.BoolExpr:
		// Boolean expressions
		switch parent.Boolop {
		case ast.BoolExprTypeAnd, ast.BoolExprTypeOr:
			// Logical operations - still useful but lower priority
			return 60
		case ast.BoolExprTypeIsNull, ast.BoolExprTypeIsNotNull:
			// IS NULL/IS NOT NULL - poor for type inference
			return 20
		case ast.BoolExprTypeNot:
			// NOT operations - moderate for type inference
			return 50
		default:
			return 40
		}

	case *ast.BetweenExpr:
		// BETWEEN expressions - good for type inference
		return 75

	case *ast.FuncCall:
		// Function call context - depends on function, generally moderate
		// sqlc.narg() and similar functions have poor type inference context
		if parent.Funcname != nil && len(parent.Funcname.Items) > 0 {
			if nameStr, ok := parent.Funcname.Items[0].(*ast.String); ok {
				if nameStr.Str == "sqlc.narg" || nameStr.Str == "sqlc.arg" {
					// sqlc parameter functions in isolation - poor for type inference
					return 30
				}
			}
		}
		return 40

	case *ast.ResTarget:
		// SELECT target or similar - can be good for type inference
		return 60

	case *ast.In:
		// IN expression - good for type inference
		return 70

	case *limitCount, *limitOffset:
		// LIMIT/OFFSET - known to be integer, good for type inference
		return 90

	default:
		// Unknown context - assign low score
		return 10
	}
}

func uniqueParamRefs(in []paramRef, dollar bool) []paramRef {
	// Group parameter references by their number
	paramGroups := make(map[int][]paramRef)
	for _, v := range in {
		if v.ref.Number != 0 {
			paramGroups[v.ref.Number] = append(paramGroups[v.ref.Number], v)
		}
	}

	// For each parameter number, select the reference with the best type inference context
	o := make([]paramRef, 0, len(paramGroups))
	for _, refs := range paramGroups {
		if len(refs) == 1 {
			// Only one reference, use it
			o = append(o, refs[0])
		} else {
			// Multiple references, select the one with the highest score
			bestRef := refs[0]
			bestScore := scoreParamRefForTypeInference(refs[0])

			for _, ref := range refs[1:] {
				score := scoreParamRefForTypeInference(ref)
				if score > bestScore {
					bestScore = score
					bestRef = ref
				}
			}
			o = append(o, bestRef)
		}
	}

	// Handle unnamed parameters (number == 0) for non-dollar parameter styles
	if !dollar {
		start := 1
		usedNumbers := make(map[int]bool)
		for _, v := range o {
			usedNumbers[v.ref.Number] = true
		}

		for _, v := range in {
			if v.ref.Number == 0 {
				for usedNumbers[start] {
					start++
				}
				v.ref.Number = start
				usedNumbers[start] = true
				o = append(o, v)
			}
		}
	}

	return o
}

// wicked-sqlc specific function
func validateAndSetDefaultOptions(n ast.Node, name, cmd string, options map[string]string) error {
	// looksLikeQuery := (cmd == metadata.CmdMany || cmd == metadata.CmdOne)
	_, isSelect := n.(*ast.SelectStmt)

	// count_intent option validation and default value setting
	err := validateOrDefaultSelectOnlyBoolOption(isSelect, name, golang.WpgxOptionKeyCountIntent, options)
	if err != nil {
		return err
	}

	// allow replica option validation and default value setting
	err = validateOrDefaultSelectOnlyBoolOption(isSelect, name, golang.WpgxOptionKeyAllowReplica, options)
	if err != nil {
		return err
	}

	// mutex option validation
	_, hasInvalidate := options[golang.WPgxOptionKeyInvalidate]
	if isSelect && hasInvalidate {
		return fmt.Errorf("query %q uses invalidate option but is a SELECT", name)
	}

	return nil
}

func validateOrDefaultSelectOnlyBoolOption(isSelect bool, queryName string, optionKey string, options map[string]string) error {
	v, ok := options[optionKey]
	if !ok {
		if isSelect {
			options[optionKey] = "true"
		} else {
			options[optionKey] = "false"
		}
	} else {
		if v != "true" && v != "false" {
			return fmt.Errorf("query %q has invalid %s value: %s", queryName, optionKey, v)
		}
		if !isSelect && v == "true" {
			return fmt.Errorf("query %q uses %s option but is not a SELECT", optionKey, queryName)
		}
	}
	return nil
}
