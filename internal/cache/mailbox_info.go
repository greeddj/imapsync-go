// Mailbox summary helpers expose cached data for presentation layers.
package cache

import (
	"sort"
)

// MailboxInfoForShow summarizes folder metadata for display in the show command.
type MailboxInfoForShow struct {
	Name     string
	Messages uint32
	Size     uint64
}

// GetSourceMailboxes returns cached source folders sorted by name.
func (cm *CacheManager) GetSourceMailboxes() []*MailboxInfoForShow {
	var result []*MailboxInfoForShow
	for _, mbox := range cm.SourceCache.Mailboxes {
		result = append(result, &MailboxInfoForShow{
			Name:     mbox.Mailbox,
			Messages: mbox.MessageCount,
			Size:     mbox.TotalSize,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// GetDestMailboxes returns cached destination folders sorted by name.
func (cm *CacheManager) GetDestMailboxes() []*MailboxInfoForShow {
	var result []*MailboxInfoForShow
	for _, mbox := range cm.DestCache.Mailboxes {
		result = append(result, &MailboxInfoForShow{
			Name:     mbox.Mailbox,
			Messages: mbox.MessageCount,
			Size:     mbox.TotalSize,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}
