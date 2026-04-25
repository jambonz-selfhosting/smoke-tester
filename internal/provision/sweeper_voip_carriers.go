package provision

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type VoipCarrierSweeper struct{ C *Client }

func (s *VoipCarrierSweeper) Name() string { return "voip_carriers" }

func (s *VoipCarrierSweeper) Sweep(protectRunID string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	carriers, err := s.C.ListVoipCarriers(ctx)
	if err != nil {
		return 0, fmt.Errorf("list: %w", err)
	}
	currentPrefix := fmt.Sprintf("%s%s-", NamePrefix, protectRunID)
	var swept int
	for _, c := range carriers {
		if !IsHarnessResource(c.Name) || strings.HasPrefix(c.Name, currentPrefix) {
			continue
		}
		// Best-effort: carriers may have gateways attached; jambonz deletes them cascade-style.
		if err := s.C.DeleteVoipCarrier(ctx, c.VoipCarrierSID); err != nil {
			continue
		}
		swept++
	}
	return swept, nil
}
