package ai

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// CodeTerm is something to look for in the founder's repo.
type CodeTerm struct {
	Label   string
	Pattern *regexp.Regexp
	Prefer  string // path fragment to rank first, e.g. "migrations" or "src"
}

// CodeSearcher finds code references for a project (the GitHub App).
type CodeSearcher interface {
	Refs(ctx context.Context, projectID int64, terms []CodeTerm) ([]CodeRef, error)
}

func term(label, prefer, pattern string) CodeTerm {
	return CodeTerm{Label: label, Prefer: prefer, Pattern: regexp.MustCompile(pattern)}
}

func supabaseFrom(table string) CodeTerm {
	return term("uses table "+table, "src", `\.from\(\s*['"`+"`"+`]`+regexp.QuoteMeta(table)+`['"`+"`"+`]`)
}

func createTable(table string) CodeTerm {
	return term("creates table "+table, "migrations", `(?i)create\s+table\s+(if\s+not\s+exists\s+)?("?public"?\.)?"?`+regexp.QuoteMeta(table)+`"?\b`)
}

// CodeTerms picks what to search for, from the error and its catalog entry.
func CodeTerms(c *Context, catalogKey string) []CodeTerm {
	var out []CodeTerm
	if n := c.net(); n != nil {
		table := TableFromPath(n.Path)
		switch {
		case strings.HasPrefix(catalogKey, "pg.") && table != "":
			out = append(out, supabaseFrom(table))
			switch catalogKey {
			case "pg.value_too_long", "pg.not_null", "pg.unique_violation", "pg.foreign_key", "pg.rls_denied":
				out = append(out, createTable(table))
			}
		case catalogKey == "auth.weak_password" || catalogKey == "auth.user_exists":
			out = append(out, term("signs up", "src", `auth\.signUp\(`))
		case catalogKey == "auth.invalid_credentials" || catalogKey == "auth.email_not_confirmed":
			out = append(out, term("logs in", "src", `signInWithPassword\(`))
		case catalogKey == "net.local_address" || catalogKey == "net.unreachable":
			if n.Host != "" {
				out = append(out, term("calls "+n.Host, "src", regexp.QuoteMeta(n.Host)))
			}
		default:
			if last := lastSegment(n.Path); last != "" {
				out = append(out, term("calls "+n.Path, "src", regexp.QuoteMeta(last)))
			}
		}
	}
	if j := c.js(); j != nil {
		switch {
		case catalogKey == "js.json_parse" && j.JSONPath != "":
			out = append(out, term("fetches "+j.JSONPath, "src", regexp.QuoteMeta(j.JSONPath)))
		case catalogKey == "js.null_property" && j.Property != "":
			prop := j.Property
			if i := strings.LastIndexByte(prop, '.'); i >= 0 {
				prop = prop[i+1:]
			}
			out = append(out, term("reads ."+prop, "src", `\.`+regexp.QuoteMeta(prop)+`\b`))
		case catalogKey == "js.not_a_function":
			if m := reNotFunction.FindStringSubmatch(j.Message); m != nil {
				ident := sourceIdent(m[1])
				out = append(out, term("calls "+ident, "src", `\.?`+regexp.QuoteMeta(ident)+`\(`))
			}
		case catalogKey == "js.not_defined":
			if m := reNotDefined.FindStringSubmatch(j.Message); m != nil {
				out = append(out, term("uses "+m[1], "src", `\b`+regexp.QuoteMeta(m[1])+`\b`))
			}
		}
		if j.Function != "" && len(out) == 0 {
			out = append(out, term("function "+j.Function, "src", `\b`+regexp.QuoteMeta(j.Function)+`\b`))
		}
	}
	return out
}

func lastSegment(p string) string {
	p = strings.Trim(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	if len(p) < 4 || strings.HasPrefix(p, "<") {
		return ""
	}
	return p
}

// whereToLook is appended to the base prompt when code references exist.
func whereToLook(refs []CodeRef) string {
	if len(refs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nWhere to look in the code:")
	for i, r := range refs {
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "\n- `%s` line %d: `%s`", r.Path, r.Line, strings.ReplaceAll(r.Snippet, "`", "'"))
	}
	return b.String()
}
