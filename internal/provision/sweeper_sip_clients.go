package provision

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SIPClientSweeper deletes orphaned `it-*` Clients left over from crashed
// prior runs. Keep at TestMain so the next run never collides with stale
// usernames in the it-<runID>-<role>-<hex> pattern.
type SIPClientSweeper struct{ C *Client }

func (s *SIPClientSweeper) Name() string { return "sip-clients" }

func (s *SIPClientSweeper) Sweep(protectRunID string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clients, err := s.C.ListSIPClients(ctx)
	if err != nil {
		return 0, fmt.Errorf("list: %w", err)
	}
	currentPrefix := fmt.Sprintf("%s%s-", NamePrefix, protectRunID)
	var swept int
	for _, cl := range clients {
		if !IsHarnessResource(cl.Username) || strings.HasPrefix(cl.Username, currentPrefix) {
			continue
		}
		if err := s.C.DeleteSIPClient(ctx, cl.ClientSID); err != nil {
			continue
		}
		swept++
	}
	return swept, nil
}
