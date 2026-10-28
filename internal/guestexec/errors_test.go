package guestexec

import (
	"strings"
	"testing"

	"davit/internal/domain"
	"davit/internal/guestproto"
)

// TestProtoErrBoxDiskFull: the guest's own disk out of space (the agent
// reports ENOSPC or EDQUOT as internal) is the user's box disk full, with
// the resize hint; any other internal agent error stays internal.
func TestProtoErrBoxDiskFull(t *testing.T) {
	for _, msg := range []string{"/home/davit/work: no space left on device", "/home/davit/work/x: disk quota exceeded"} {
		err := protoErr(guestproto.Errorf(guestproto.CodeInternal, "%s", msg))
		de := domain.AsError(err)
		if de.Code != domain.CodeResourceExhausted || de.Retryable || !domain.IsBoxDiskFull(err) || domain.IsRoomError(err) ||
			!strings.HasPrefix(de.Message, "guest: "+msg+"; box disk full: `davit box up --disk 40GiB` (resize) or clean the workspace") {
			t.Fatalf("%q: %+v", msg, de)
		}
	}
	if de := domain.AsError(protoErr(guestproto.Errorf(guestproto.CodeInternal, "connect to x: refused"))); de.Code != domain.CodeInternal {
		t.Fatalf("other internal error: %+v", de)
	}
}
