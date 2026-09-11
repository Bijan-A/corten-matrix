// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Enforces that contact-based handle merging goes through the guard.

package connector

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// contactPortalIDsAllowlist names the only functions permitted to read a contact
// card's handles directly. Everything else must go through
// mutualContactHandles, which refuses handles shared by two people.
//
// This test exists because the guard was once added and the two most important
// call sites were left unconverted — the patch that was supposed to convert them
// failed halfway and the omission went unnoticed, because both need a live
// bridge and so had no unit test. A shipped bridge then re-merged two people's
// DMs on the first sync. An allowlist is cheap; re-auditing eleven call sites by
// hand is not.
//
// To add an entry, be sure the function only DISPLAYS a card's handles or builds
// the index itself. If it picks a portal, a send target, a member count or an
// "is this message ours" decision, it must use the guard instead.
var contactPortalIDsAllowlist = map[string]string{
	"buildContactPersonIndex": "builds the person index itself — must read cards raw",
	"contactKeyFromContact":   "groups a card's own phones for sync dedup, picks no portal",
	"GetUserInfo":             "emits the ghost's contact-card identifiers for display, not routing",
	"fnMsgDebug":              "debug command; a deliberately broad IDS probe list",
}

func TestContactPortalIDsCallersAreAllowlisted(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var violations []string

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := fn.Name.Name
			if _, allowed := contactPortalIDsAllowlist[name]; allowed {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != "contactPortalIDs" {
					return true
				}
				violations = append(violations, path+":"+
					fset.Position(call.Pos()).String()[len(path)+1:]+" in "+name)
				return true
			})
		}
	}

	for _, v := range violations {
		t.Errorf("contactPortalIDs called outside the guard at %s\n"+
			"\tUse c.mutualContactHandles(handle) instead — reading the card directly merges\n"+
			"\thandles that two different people share. If this call only displays a card's\n"+
			"\thandles or builds the index, add the function to contactPortalIDsAllowlist.", v)
	}
}
