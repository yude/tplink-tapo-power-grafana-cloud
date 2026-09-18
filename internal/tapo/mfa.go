package tapo

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
)

type FileMFACodeProvider struct {
	Immediate    string
	Path         string
	PollInterval time.Duration
	Timeout      time.Duration
}

func (p FileMFACodeProvider) WaitCode(ctx context.Context) (string, error) {
	if code := strings.TrimSpace(p.Immediate); code != "" {
		return code, nil
	}
	if p.Path == "" {
		return "", errors.New("TAPO_MFA_CODE_FILE is not configured")
	}
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}
	interval := p.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if content, err := os.ReadFile(p.Path); err == nil {
			if code := strings.TrimSpace(string(content)); code != "" {
				return code, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}
