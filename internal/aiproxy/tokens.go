package aiproxy

import (
	"context"
	"sync"
	"time"
)

// Tokens lists the space credentials currently bound to this proxy endpoint.
type Tokens func(context.Context) ([]string, error)

// A short cache avoids reading the runtime for every request while bounding
// revocation to 30 seconds. An unsuccessful refresh never accepts stale tokens.
type proxyTokens struct {
	mu      sync.Mutex
	until   time.Time
	tokens  map[string]bool
	refresh func() (map[string]bool, error)
}

func newTokenCache(valid Tokens) *proxyTokens {
	return &proxyTokens{refresh: func() (map[string]bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		values, err := valid(ctx)
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, value := range values {
			if value != "" {
				set[value] = true
			}
		}
		return set, nil
	}}
}

func (t *proxyTokens) valid(token string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Now().After(t.until) || t.tokens == nil {
		fresh, err := t.refresh()
		if err != nil {
			return false, err
		}
		t.tokens = fresh
		t.until = time.Now().Add(30 * time.Second)
	}
	return token != "" && t.tokens[token], nil
}
