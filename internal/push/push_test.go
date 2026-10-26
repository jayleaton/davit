package push

import (
	"context"
	"io/fs"
	"testing"

	"davit/internal/domain"
)

func TestValidateSelection(t *testing.T) {
	ok := Static{{Path: "go.mod", Mode: 0o644}, {Path: "sub/lib/x.go", Mode: 0o644}, {Path: "link", Mode: fs.ModeSymlink, LinkTarget: "go.mod"}}
	if es, err := ok.Select(context.Background(), "/src"); err != nil || len(es) != 3 {
		t.Fatalf("valid selection: %v", err)
	}
	for _, bad := range []Entry{
		{Path: "/etc/passwd"}, {Path: "../x"}, {Path: "a/../b"}, {Path: ""},
		{Path: ".git/config"}, {Path: "sub/.git/HEAD"}, {Path: "go.mod"}, // duplicate of ok[0]
		{Path: "f", LinkTarget: "x"},
	} {
		if _, err := Validate(append(Static{{Path: "go.mod"}}, bad)); !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Errorf("%+v accepted: %v", bad, err)
		}
	}
}
