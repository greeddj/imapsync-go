package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/greeddj/imapsync-go/internal/config"
)

func TestNewCacheManager(t *testing.T) {
	src := config.Credentials{
		Label:  "test-src",
		Server: "imap.source.com:993",
		User:   "user@source.com",
		Pass:   "srcpass",
	}

	dst := config.Credentials{
		Label:  "test-dst",
		Server: "imap.dest.com:993",
		User:   "user@dest.com",
		Pass:   "dstpass",
	}

	cm, err := NewCacheManager(src, dst)
	if err != nil {
		t.Fatalf("NewCacheManager failed: %v", err)
	}

	if cm.SourceCache.Server != src.Server {
		t.Errorf("expected source server %s, got %s", src.Server, cm.SourceCache.Server)
	}

	if cm.DestCache.User != dst.User {
		t.Errorf("expected dest user %s, got %s", dst.User, cm.DestCache.User)
	}

	if cm.SourceCache.Mailboxes == nil {
		t.Error("source mailboxes map is nil")
	}

	if cm.DestCache.Mailboxes == nil {
		t.Error("dest mailboxes map is nil")
	}
}

func TestUpdateMailbox(t *testing.T) {
	sc := &ServerCache{
		Server:    "imap.test.com:993",
		User:      "test@test.com",
		Mailboxes: make(map[string]*MailboxCache),
	}

	messages := []*imap.Message{
		{
			Uid:  1,
			Size: 1024,
			Envelope: &imap.Envelope{
				MessageId: "<msg1@test.com>",
				Subject:   "Test Message 1",
				Date:      time.Now(),
				From: []*imap.Address{
					{
						PersonalName: "Test Sender",
						MailboxName:  "sender",
						HostName:     "test.com",
					},
				},
			},
		},
		{
			Uid:  2,
			Size: 2048,
			Envelope: &imap.Envelope{
				MessageId: "<msg2@test.com>",
				Subject:   "Test Message 2",
				Date:      time.Now(),
				From: []*imap.Address{
					{
						MailboxName: "sender2",
						HostName:    "test.com",
					},
				},
			},
		},
	}

	sc.UpdateMailbox("INBOX", messages)

	mbox := sc.Mailboxes["INBOX"]
	if mbox == nil {
		t.Fatal("INBOX not created")
	}

	if mbox.MessageCount != 2 {
		t.Errorf("expected 2 messages, got %d", mbox.MessageCount)
	}

	if mbox.TotalSize != 3072 {
		t.Errorf("expected total size 3072, got %d", mbox.TotalSize)
	}

	if mbox.UIDNext != 3 {
		t.Errorf("expected UIDNext 3, got %d", mbox.UIDNext)
	}

	msg1 := mbox.Messages["msg1@test.com"]
	if msg1 == nil {
		t.Fatal("message 1 not found")
	}

	if msg1.Subject != "Test Message 1" {
		t.Errorf("expected subject 'Test Message 1', got %s", msg1.Subject)
	}

	if msg1.From != "Test Sender" {
		t.Errorf("expected from 'Test Sender', got %s", msg1.From)
	}
}

func TestGetMailbox(t *testing.T) {
	sc := &ServerCache{
		Mailboxes: map[string]*MailboxCache{
			"INBOX": {
				Mailbox:      "INBOX",
				MessageCount: 5,
			},
		},
	}

	mbox := sc.GetMailbox("INBOX")
	if mbox == nil {
		t.Error("expected to find INBOX")
	}

	mbox = sc.GetMailbox("NonExistent")
	if mbox != nil {
		t.Error("expected nil for non-existent mailbox")
	}
}

func TestHasMessage(t *testing.T) {
	sc := &ServerCache{
		Mailboxes: map[string]*MailboxCache{
			"INBOX": {
				Mailbox: "INBOX",
				Messages: map[string]*MessageInfo{
					"msg1@test.com": {
						MessageID: "msg1@test.com",
					},
				},
			},
		},
	}

	if !sc.HasMessage("INBOX", "msg1@test.com") {
		t.Error("expected to find message")
	}

	if sc.HasMessage("INBOX", "nonexistent@test.com") {
		t.Error("expected not to find message")
	}

	if sc.HasMessage("NonExistent", "msg1@test.com") {
		t.Error("expected false for non-existent mailbox")
	}
}

func TestGetMessageIDs(t *testing.T) {
	sc := &ServerCache{
		Mailboxes: map[string]*MailboxCache{
			"INBOX": {
				Mailbox: "INBOX",
				Messages: map[string]*MessageInfo{
					"msg1@test.com": {},
					"msg2@test.com": {},
					"msg3@test.com": {},
				},
			},
		},
	}

	ids := sc.GetMessageIDs("INBOX")
	if len(ids) != 3 {
		t.Errorf("expected 3 message IDs, got %d", len(ids))
	}

	ids = sc.GetMessageIDs("NonExistent")
	if ids != nil {
		t.Error("expected nil for non-existent mailbox")
	}
}

func TestClear(t *testing.T) {
	tmpDir := t.TempDir()

	src := config.Credentials{
		Server: "imap.source.com:993",
		User:   "user@source.com",
		Pass:   "srcpass",
	}

	dst := config.Credentials{
		Server: "imap.dest.com:993",
		User:   "user@dest.com",
		Pass:   "dstpass",
	}

	cacheFile := generateCacheFileName(src.Server, src.User, dst.Server, dst.User)
	cacheFilePath := filepath.Join(tmpDir, cacheFile)

	cm := &CacheManager{
		SourceCache: &ServerCache{
			Server:    src.Server,
			User:      src.User,
			Mailboxes: map[string]*MailboxCache{"INBOX": {}},
		},
		DestCache: &ServerCache{
			Server:    dst.Server,
			User:      dst.User,
			Mailboxes: map[string]*MailboxCache{"INBOX": {}},
		},
		cacheFile: cacheFilePath,
	}

	if err := os.WriteFile(cacheFilePath, []byte("test"), 0600); err != nil {
		t.Fatalf("failed to create test cache file: %v", err)
	}

	if err := cm.Clear(); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}

	if _, err := os.Stat(cacheFilePath); !os.IsNotExist(err) {
		t.Error("cache file should be deleted")
	}

	if len(cm.SourceCache.Mailboxes) != 0 {
		t.Error("source mailboxes should be empty")
	}

	if len(cm.DestCache.Mailboxes) != 0 {
		t.Error("dest mailboxes should be empty")
	}
}
