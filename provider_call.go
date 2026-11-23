package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/renezander030/draftcat/internal/config"
)

const maxProviderResponseBytes int64 = 4 << 20

type providerCallError struct {
	err       error
	uncertain bool
}

func (e *providerCallError) Error() string { return e.err.Error() }
func (e *providerCallError) Unwrap() error { return e.err }
func providerError(err error, uncertain bool) error {
	return &providerCallError{err: err, uncertain: uncertain}
}
func uncertainProviderError(err error) bool {
	var e *providerCallError
	if errors.As(err, &e) {
		return e.uncertain
	}
	return true
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func readProviderResponse(ctx context.Context, body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, r: body}, maxProviderResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read provider response: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read provider response: %w", err)
	}
	if int64(len(data)) > maxProviderResponseBytes {
		return nil, fmt.Errorf("provider response exceeds %d-byte limit", maxProviderResponseBytes)
	}
	return data, nil
}

// callLLM is the single governance boundary for engine model calls. A tracker
// is optional for standalone client tests; every engine caller supplies one.
func callLLM(ctx context.Context, cfg *config.Config, role, prompt string, trackers ...*BudgetTracker) (*CompletionResponse, error) {
	modelName, ok := cfg.Roles[role]
	if !ok {
		return nil, fmt.Errorf("unknown role: %s", role)
	}
	model, ok := cfg.Models[modelName]
	if !ok {
		return nil, fmt.Errorf("unknown model: %s", modelName)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := enforceModelPolicy(ctx, cfg, role, "input", prompt); err != nil {
		return nil, err
	}
	maxTokens := model.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	if cap := cfg.Budgets.PerStepTokens; cap > 0 && maxTokens > cap {
		maxTokens = cap
	}
	var b *BudgetTracker
	if len(trackers) > 0 {
		b = trackers[0]
	}
	var resp *CompletionResponse
	var callErr error
	if b == nil {
		resp, callErr = providerCallLLM(ctx, cfg, role, prompt, maxTokens)
	} else {
		// The admission gate protects provider dispatch and settlement only.
		// Human review of a paid response must not hold another run's budget.
		resp, callErr = func() (*CompletionResponse, error) {
			if err := b.acquire(ctx); err != nil {
				return nil, fmt.Errorf("budget admission canceled: %w", err)
			}
			defer b.release()
			worst := model.WorstCaseCost(len(prompt), maxTokens)
			if cfg.Provider.CachesPrompt(model) {
				worst += model.WorstCaseCost(len(prompt), 0) * cacheWritePremium
			}
			id, err := b.admit(ctx, cfg, maxTokens, model.Model, worst)
			if err != nil {
				return nil, err
			}
			result, providerErr := providerCallLLM(ctx, cfg, role, prompt, maxTokens)
			if err := b.finish(id, pipelineFromContext(ctx), result, providerErr, cfg); err != nil {
				return nil, err
			}
			return result, providerErr
		}()
	}
	if callErr != nil {
		return nil, callErr
	}
	if err := enforceModelPolicy(ctx, cfg, role, "output", resp.Text); err != nil {
		return nil, err
	}
	return resp, nil
}

// cacheWritePremium is what a prompt cache write costs on top of the input
// price: Anthropic bills a five-minute cache write at 1.25x.
const cacheWritePremium = 0.25

type stablePrefixKey struct{}

// withStablePrefix records the start of the prompt that is the same on every
// run of the step making the call, for a model with prompt_cache on.
func withStablePrefix(ctx context.Context, prefix string) context.Context {
	return context.WithValue(ctx, stablePrefixKey{}, prefix)
}

// promptContent is the user message's content. For a model that caches
// prompts it is two text parts: the stable prefix marked with cache_control,
// so the next call starting with the same instructions reads them from the
// cache, then the rest of the prompt. Everything else gets the plain string.
func promptContent(ctx context.Context, cfg *config.Config, model config.ModelConfig, prompt string) interface{} {
	prefix, _ := ctx.Value(stablePrefixKey{}).(string)
	if !cfg.Provider.CachesPrompt(model) || strings.TrimSpace(prefix) == "" || !strings.HasPrefix(prompt, prefix) {
		return prompt
	}
	marked := map[string]interface{}{"type": "text", "text": prefix, "cache_control": map[string]string{"type": "ephemeral"}}
	rest := prompt[len(prefix):]
	if strings.TrimSpace(rest) == "" {
		// Anthropic refuses an empty or blank text part.
		marked["text"] = prompt
		return []map[string]interface{}{marked}
	}
	return []map[string]interface{}{marked, {"type": "text", "text": rest}}
}

func providerDeclaresUsage(body []byte) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return false
	}
	usage, ok := object["usage"]
	return ok && !bytes.Equal(bytes.TrimSpace(usage), []byte("null"))
}
