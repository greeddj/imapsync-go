package client

import (
	"testing"
)

// TestSelectIfNeeded_shortCircuitsWhenSameFolder asserts the invariant that
// callers (FetchMessageMap, StreamMessagesByUIDs, getFolderSize) all rely on:
// a second call for the same folder on the same connection generation does
// not touch the underlying imapclient.Client. The test passes a nil cli on
// purpose — if short-circuit breaks, the call will panic on nil deref.
func TestSelectIfNeeded_shortCircuitsWhenSameFolder(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	f := "INBOX"
	c.selectedFolder.Store(&f)

	mbox, err := c.selectIfNeeded(nil, "INBOX")
	if err != nil {
		t.Fatalf("selectIfNeeded short-circuit returned err=%v", err)
	}
	if mbox != nil {
		t.Errorf("short-circuit returned mbox=%v, want nil", mbox)
	}
}

// TestSelectIfNeeded_differentFolderRequiresFreshSelect asserts that the
// short-circuit only fires for the cached folder; a different name must
// flow through to cli.Select. A nil cli triggers a panic, which the test
// recovers as proof that the short-circuit was *not* taken.
func TestSelectIfNeeded_differentFolderRequiresFreshSelect(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	f := "INBOX"
	c.selectedFolder.Store(&f)

	defer func() {
		if r := recover(); r == nil {
			t.Error("selectIfNeeded for a different folder did not call cli.Select")
		}
	}()
	_, _ = c.selectIfNeeded(nil, "Archive")
}
