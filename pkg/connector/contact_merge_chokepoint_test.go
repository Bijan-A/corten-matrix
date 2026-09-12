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

// chokepointAllowlist names the only functions permitted to read a contact
// card's handles directly — either by calling contactPortalIDs or by reaching
// into Contact.Phones / Contact.Emails. Everything else must go through
// mutualContactHandles, which refuses handles that two people claim.
//
// Keys are receiver-qualified ("(*IMClient).foo", or a bare name for a package
// function). An earlier version keyed on the bare name, so any method called
// GetUserInfo on any type silently inherited the exemption.
//
// This test exists because the guard was once added with two of its most
// important call sites left unconverted — the patch that should have converted
// them failed halfway and the omission went unnoticed, because both need a live
// bridge to exercise and so had no unit test. A shipped bridge then re-merged
// two people's DMs on the first sync. An allowlist is cheap; re-auditing every
// call site by hand is not.
//
// To add an entry, be sure the function only DISPLAYS a card's handles or builds
// the index itself. If it picks a portal, a send target, a member count, or an
// "is this message ours" decision, it must use the guard instead.
var chokepointAllowlist = map[string]string{
	"buildContactPersonIndex":                   "builds the person index itself — must read cards raw",
	"contactPortalIDs":                          "the accessor being guarded",
	"contactKeyFromContact":                     "groups one card's own phones for sync dedup, picks no portal",
	"(*IMClient).GetUserInfo":                   "emits the ghost's contact-card identifiers for display, not routing",
	"fnMsgDebug":                                "debug command; a deliberately broad IDS probe list",
	"(*cloudContactsClient).SyncContacts":       "builds the phone/email lookup index",
	"(*externalCardDAVClient).SyncContacts":     "builds the phone/email lookup index",
	"(*cloudContactsClient).carryOverAvatars":   "matches cards across syncs to reuse avatars",
	"(*externalCardDAVClient).carryOverAvatars": "matches cards across syncs to reuse avatars",

	// Parsing and display. These populate or render a card; none of them
	// chooses a portal, a send target or a member count.
	"parseVCard": "fills Contact.Phones/Emails from a vCard",
	"(*cloudContactsClient).parseVCardMultistatus": "counts parsed handles for the sync log",
	"parseVCardMultistatusStandalone":              "counts parsed handles for the sync log",
	"makeVCardPreviewContent":                      "renders a shared contact card into a Matrix preview",
	"fnContacts":                                   "prints the address book for the operator",
}

// funcKey renders a receiver-qualified name for a declaration.
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	var buf strings.Builder
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			buf.WriteString("(*" + id.Name + ")")
		}
	case *ast.Ident:
		buf.WriteString("(" + t.Name + ")")
	}
	if buf.Len() == 0 {
		return fn.Name.Name
	}
	return buf.String() + "." + fn.Name.Name
}

// TestContactCardReadsAreAllowlisted fails when a function outside the
// allowlist reads a contact card's handles without going through the guard.
func TestContactCardReadsAreAllowlisted(t *testing.T) {
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
			if _, allowed := chokepointAllowlist[funcKey(fn)]; allowed {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				var what string
				switch e := n.(type) {
				case *ast.CallExpr:
					if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "contactPortalIDs" {
						what = "contactPortalIDs(...)"
					}
				case *ast.SelectorExpr:
					// Catches c.lookupContact(x).Phones and any other direct
					// read of a card's handle slices — the way to rebuild the
					// bug without ever naming contactPortalIDs.
					if e.Sel.Name == "Phones" || e.Sel.Name == "Emails" {
						what = "Contact." + e.Sel.Name
					}
				}
				if what == "" {
					return true
				}
				violations = append(violations,
					fset.Position(n.Pos()).String()+" in "+funcKey(fn)+" reads "+what)
				return true
			})
		}
	}

	for _, v := range violations {
		t.Errorf("contact card read outside the guard at %s\n"+
			"\tUse c.mutualContactHandles(handle) instead — reading the card directly merges\n"+
			"\thandles that two different people share. If this only displays a card's handles\n"+
			"\tor builds the index, add the receiver-qualified name to chokepointAllowlist.", v)
	}
}
