// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package chatgptauth

import (
	"errors"
	"time"
)

// FactoryFreshPlanToken reads only the active account and never refreshes or falls back.
func FactoryFreshPlanToken(s Store) (string, error) {
	a, err := s.Selected("")
	if err != nil {
		return "", err
	}
	now := time.Now()
	if a.TokenType != "Bearer" || a.AccessToken == "" || !a.PlanEnabled() || a.PendingRefresh != nil ||
		!a.ExpiresAt.After(now) || a.refreshDue(now) {
		return "", errors.New("fresh ChatGPT plan sign-in required")
	}
	return a.AccessToken, nil
}
