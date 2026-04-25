package provision

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type LcrSweeper struct{ C *Client }

func (s *LcrSweeper) Name() string { return "lcrs" }

func (s *LcrSweeper) Sweep(protectRunID string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lcrs, err := s.C.ListLcrs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list: %w", err)
	}
	currentPrefix := fmt.Sprintf("%s%s-", NamePrefix, protectRunID)
	var swept int
	for _, l := range lcrs {
		if !IsHarnessResource(l.Name) || strings.HasPrefix(l.Name, currentPrefix) {
			continue
		}
		if err := s.C.DeleteLcr(ctx, l.LcrSID); err != nil {
			continue
		}
		swept++
	}
	return swept, nil
}
