package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestEarlyApprovalCancellationDuringSend(t *testing.T) {
	for _, quorum := range []bool{false, true} {
		for i := 0; i < 20; i++ {
			bot, s := newScriptedBot()
			ctx, cancel := context.WithCancel(context.Background())
			bot.api = func(method string, payload map[string]interface{}) (json.RawMessage, error) {
				raw, err := s.api(method, payload)
				if method == "sendMessage" {
					cancel()
					bot.pump().route(cbUpdate(1, 1, 7, "approve"))
					bot.pump().route(cbUpdate(2, 1, 8, "approve"))
				}
				return raw, err
			}
			if quorum {
				d, err := bot.SendForQuorumApproval(ctx, "draft", 2, nil)
				if err == nil {
					t.Fatalf("cancelled quorum allowed %+v", d)
				}
			} else {
				d, err := bot.SendForApproval(ctx, "draft", nil)
				if err == nil {
					t.Fatalf("cancelled approval allowed %+v", d)
				}
			}
			cancel()
		}
	}
}
