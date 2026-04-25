package provision

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type AccountSweeper struct{ C *Client }

func (s *AccountSweeper) Name() string { return "accounts" }

func (s *AccountSweeper) Sweep(protectRunID string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	accts, err := s.C.ListAccounts(ctx)
	if err != nil {
		return 0, fmt.Errorf("list: %w", err)
	}
	currentPrefix := fmt.Sprintf("%s%s-", NamePrefix, protectRunID)
	var swept int
	for _, a := range accts {
		if !IsHarnessResource(a.Name) || strings.HasPrefix(a.Name, currentPrefix) {
			continue
		}
		if err := s.C.DeleteAccount(ctx, a.AccountSID); err != nil {
			continue // likely 422 because resources attached — leave for manual cleanup
		}
		swept++
	}
	return swept, nil
}
