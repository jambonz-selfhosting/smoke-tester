package provision

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type ApplicationSweeper struct{ C *Client }

func (s *ApplicationSweeper) Name() string { return "applications" }

func (s *ApplicationSweeper) Sweep(protectRunID string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	apps, err := s.C.ListApplications(ctx)
	if err != nil {
		return 0, fmt.Errorf("list: %w", err)
	}
	currentPrefix := fmt.Sprintf("%s%s-", NamePrefix, protectRunID)
	var swept int
	for _, a := range apps {
		if !IsHarnessResource(a.Name) || strings.HasPrefix(a.Name, currentPrefix) {
			continue
		}
		if err := s.C.DeleteApplication(ctx, a.ApplicationSID); err != nil {
			continue
		}
		swept++
	}
	return swept, nil
}
