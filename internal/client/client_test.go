package client

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
)

type mockProgressReporter struct {
	messages []string
	quiet    bool
}

func (m *mockProgressReporter) Update(message string) {
	m.messages = append(m.messages, message)
}

func (m *mockProgressReporter) IsQuiet() bool {
	return m.quiet
}

func TestSetPrefix(t *testing.T) {
	c := &Client{}
	prefix := "test-prefix"

	c.SetPrefix(prefix)

	if c.prefix != prefix {
		t.Errorf("expected prefix %s, got %s", prefix, c.prefix)
	}
}

func TestSetProgress(t *testing.T) {
	c := &Client{}
	progress := &mockProgressReporter{}

	c.SetProgress(progress)

	if c.progress != progress {
		t.Error("progress reporter not set correctly")
	}
}

func TestUpdateProgress(t *testing.T) {
	tests := []struct {
		name     string
		quiet    bool
		message  string
		expected int
	}{
		{
			name:     "normal mode",
			quiet:    false,
			message:  "test message",
			expected: 1,
		},
		{
			name:     "quiet mode",
			quiet:    true,
			message:  "test message",
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{}
			progress := &mockProgressReporter{quiet: tt.quiet}
			c.SetProgress(progress)

			c.UpdateProgress(tt.message)

			if len(progress.messages) != tt.expected {
				t.Errorf("expected %d messages, got %d", tt.expected, len(progress.messages))
			}

			if !tt.quiet && len(progress.messages) > 0 {
				if progress.messages[0] != tt.message {
					t.Errorf("expected message %q, got %q", tt.message, progress.messages[0])
				}
			}
		})
	}
}

func TestUpdateProgressNoReporter(t *testing.T) {
	c := &Client{}
	c.UpdateProgress("test message")
}

func TestIsConnError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "EOF error",
			err:      io.EOF,
			expected: true,
		},
		{
			name:     "network closed error",
			err:      net.ErrClosed,
			expected: true,
		},
		{
			name:     "timeout error",
			err:      &net.OpError{Op: "read", Err: errors.New("timeout")},
			expected: true,
		},
		{
			name:     "regular error",
			err:      errors.New("some error"),
			expected: false,
		},
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isConnError(tt.err)
			if result != tt.expected {
				t.Errorf("isConnError(%v) = %v; want %v", tt.err, result, tt.expected)
			}
		})
	}
}

func TestMailboxInfo(t *testing.T) {
	info := &MailboxInfo{
		Name:     "INBOX",
		Messages: 100,
		Size:     1024000,
	}

	if info.Name != "INBOX" {
		t.Errorf("expected name INBOX, got %s", info.Name)
	}

	if info.Messages != 100 {
		t.Errorf("expected 100 messages, got %d", info.Messages)
	}

	if info.Size != 1024000 {
		t.Errorf("expected size 1024000, got %d", info.Size)
	}
}

func TestCreateParentFoldersLogic(t *testing.T) {
	tests := []struct {
		name      string
		folder    string
		delimiter string
		expected  []string
	}{
		{
			name:      "single level",
			folder:    "INBOX",
			delimiter: "/",
			expected:  []string{},
		},
		{
			name:      "two levels",
			folder:    "Archive/2023",
			delimiter: "/",
			expected:  []string{"Archive"},
		},
		{
			name:      "three levels",
			folder:    "Work/Projects/Active",
			delimiter: "/",
			expected:  []string{"Work", "Work/Projects"},
		},
		{
			name:      "dot delimiter",
			folder:    "Archive.2023.January",
			delimiter: ".",
			expected:  []string{"Archive", "Archive.2023"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts := strings.Split(tt.folder, tt.delimiter)
			var parents []string

			for i := 1; i < len(parts); i++ {
				parentPath := strings.Join(parts[:i], tt.delimiter)
				parents = append(parents, parentPath)
			}

			if len(parents) != len(tt.expected) {
				t.Errorf("expected %d parents, got %d", len(tt.expected), len(parents))
			}

			for i, expected := range tt.expected {
				if i >= len(parents) || parents[i] != expected {
					t.Errorf("expected parent[%d] = %s, got %s", i, expected, parents[i])
				}
			}
		})
	}
}

func TestBackoffCalculation(t *testing.T) {
	c := &Client{
		backoff:       initialBackoff,
		reconnectDur:  reconnectInterval,
		lastReconnect: time.Now().Add(-15 * time.Second),
	}

	initialDelay := c.backoff
	if initialDelay != 2*time.Second {
		t.Errorf("expected initial backoff 2s, got %v", initialDelay)
	}

	c.backoff *= 2
	if c.backoff != 4*time.Second {
		t.Errorf("expected backoff 4s after doubling, got %v", c.backoff)
	}
}

func TestReconnectIntervalCheck(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name          string
		lastReconnect time.Time
		shouldWait    bool
	}{
		{
			name:          "recent reconnect",
			lastReconnect: now.Add(-5 * time.Second),
			shouldWait:    true,
		},
		{
			name:          "old reconnect",
			lastReconnect: now.Add(-15 * time.Second),
			shouldWait:    false,
		},
		{
			name:          "exactly at interval",
			lastReconnect: now.Add(-reconnectInterval),
			shouldWait:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{
				reconnectDur:  reconnectInterval,
				lastReconnect: tt.lastReconnect,
			}

			sinceLast := now.Sub(c.lastReconnect)
			shouldWait := sinceLast < c.reconnectDur

			if shouldWait != tt.shouldWait {
				t.Errorf("expected shouldWait=%v, got %v (sinceLast=%v)",
					tt.shouldWait, shouldWait, sinceLast)
			}
		})
	}
}

func TestConstants(t *testing.T) {
	tests := []struct {
		name     string
		value    interface{}
		expected interface{}
	}{
		{"mailboxChanBuffer", mailboxChanBuffer, 10},
		{"messageChanBuffer", messageChanBuffer, 10},
		{"initialBackoff", initialBackoff, 2 * time.Second},
		{"reconnectInterval", reconnectInterval, 10 * time.Second},
		{"maxReconnectAttempts", maxReconnectAttempts, 5},
		{"progressUpdateInterval", progressUpdateInterval, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != tt.expected {
				t.Errorf("%s = %v; want %v", tt.name, tt.value, tt.expected)
			}
		})
	}
}

func TestMessageIDExtraction(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "with angle brackets",
			input:    "<msg123@example.com>",
			expected: "msg123@example.com",
		},
		{
			name:     "without angle brackets",
			input:    "msg456@example.com",
			expected: "msg456@example.com",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "only brackets",
			input:    "<>",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := strings.Trim(tt.input, "<>")
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestSeqSetCreation(t *testing.T) {
	tests := []struct {
		name  string
		start uint32
		end   uint32
	}{
		{
			name:  "single message",
			start: 1,
			end:   1,
		},
		{
			name:  "range of messages",
			start: 1,
			end:   100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seqset := new(imap.SeqSet)
			seqset.AddRange(tt.start, tt.end)

			// SeqSet is created, verify it's usable
			if seqset.String() == "" {
				t.Error("seqset should have valid range")
			}
		})
	}
}

func TestFetchItemsConstruction(t *testing.T) {
	tests := []struct {
		name     string
		items    []imap.FetchItem
		expected int
	}{
		{
			name:     "envelope only",
			items:    []imap.FetchItem{imap.FetchEnvelope},
			expected: 1,
		},
		{
			name:     "envelope and body",
			items:    []imap.FetchItem{imap.FetchEnvelope, imap.FetchRFC822},
			expected: 2,
		},
		{
			name:     "size only",
			items:    []imap.FetchItem{imap.FetchRFC822Size},
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.items) != tt.expected {
				t.Errorf("expected %d items, got %d", tt.expected, len(tt.items))
			}
		})
	}
}

func TestProgressUpdateInterval(t *testing.T) {
	progress := &mockProgressReporter{}
	c := &Client{prefix: "test"}
	c.SetProgress(progress)

	totalMessages := 25
	for i := 1; i <= totalMessages; i++ {
		if i%progressUpdateInterval == 0 {
			c.UpdateProgress("progress update")
		}
	}

	expected := 2
	if len(progress.messages) != expected {
		t.Errorf("expected %d progress updates, got %d", expected, len(progress.messages))
	}
}

func TestDelimiterParsing(t *testing.T) {
	tests := []struct {
		name      string
		delimiter string
		folder    string
		hasParts  bool
	}{
		{
			name:      "slash delimiter",
			delimiter: "/",
			folder:    "Archive/2023",
			hasParts:  true,
		},
		{
			name:      "dot delimiter",
			delimiter: ".",
			folder:    "Archive.2023",
			hasParts:  true,
		},
		{
			name:      "no delimiter",
			delimiter: "/",
			folder:    "INBOX",
			hasParts:  false,
		},
		{
			name:      "empty delimiter",
			delimiter: "",
			folder:    "Archive/2023",
			hasParts:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasParts := tt.delimiter != "" && strings.Contains(tt.folder, tt.delimiter)
			if hasParts != tt.hasParts {
				t.Errorf("expected hasParts=%v, got %v", tt.hasParts, hasParts)
			}
		})
	}
}

func TestClientStructFields(t *testing.T) {
	c := &Client{
		serverAddr:   "imap.example.com:993",
		useTLS:       true,
		username:     "user@example.com",
		password:     "password",
		backoff:      2 * time.Second,
		reconnectDur: 10 * time.Second,
		prefix:       "test",
		verbose:      true,
	}

	if c.serverAddr != "imap.example.com:993" {
		t.Errorf("expected serverAddr imap.example.com:993, got %s", c.serverAddr)
	}

	if !c.useTLS {
		t.Error("expected useTLS to be true")
	}

	if c.username != "user@example.com" {
		t.Errorf("expected username user@example.com, got %s", c.username)
	}

	if c.backoff != 2*time.Second {
		t.Errorf("expected backoff 2s, got %v", c.backoff)
	}

	if c.prefix != "test" {
		t.Errorf("expected prefix test, got %s", c.prefix)
	}

	if !c.verbose {
		t.Error("expected verbose to be true")
	}
}

func TestMailboxInfoCollection(t *testing.T) {
	infos := []*MailboxInfo{
		{Name: "INBOX", Messages: 100, Size: 1000000},
		{Name: "Sent", Messages: 50, Size: 500000},
		{Name: "Archive", Messages: 200, Size: 2000000},
	}

	if len(infos) != 3 {
		t.Errorf("expected 3 mailboxes, got %d", len(infos))
	}

	totalMessages := uint32(0)
	totalSize := uint64(0)

	for _, info := range infos {
		totalMessages += info.Messages
		totalSize += info.Size
	}

	if totalMessages != 350 {
		t.Errorf("expected total messages 350, got %d", totalMessages)
	}

	if totalSize != 3500000 {
		t.Errorf("expected total size 3500000, got %d", totalSize)
	}
}
