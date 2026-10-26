//go:build linux

package userspace

import (
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// TestFilteredRelaysOnlyQueries: in filtered mode the stack relays plain
// queries to the host's resolver and drops anything else (an UPDATE here)
// without contacting it (SEC-10).
func TestFilteredRelaysOnlyQueries(t *testing.T) {
	st := testStack(t, ModeFiltered, nil)
	calls := 0
	exchange := func(q []byte) ([]byte, error) { calls++; return q, nil }
	if st.answer(query(t, "example.com."), exchange) == nil || calls != 1 {
		t.Fatalf("plain query not relayed (calls %d)", calls)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 3, OpCode: 5})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET})
	update, _ := b.Finish()
	if st.answer(update, exchange) != nil || calls != 1 {
		t.Fatalf("UPDATE relayed (calls %d)", calls)
	}
}
