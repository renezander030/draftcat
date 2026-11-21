package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestApprovalCallbackBeforeSendReturns(t *testing.T) {
	for _, quorum := range []bool{false, true} {
		name := "single"
		if quorum {
			name = "quorum"
		}
		t.Run(name, func(t *testing.T) {
			bot, s := newScriptedBot()
			bot.api = func(method string, payload map[string]interface{}) (json.RawMessage, error) {
				raw, err := s.api(method, payload)
				if method == "sendMessage" && err == nil {
					bot.pump().route(cbUpdate(1, 1, 7, "approve"))
					if quorum {
						bot.pump().route(cbUpdate(2, 1, 8, "approve"))
					}
					bot.drainUpdates()
				}
				return raw, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if quorum {
				d, err := bot.SendForQuorumApproval(ctx, "draft", 2, nil)
				if err != nil || d.Action != "approve" || len(d.Approvers) != 2 {
					t.Fatalf("decision=%+v err=%v", d, err)
				}
			} else {
				d, err := bot.SendForApproval(ctx, "draft", nil)
				if err != nil || d.Action != "approve" {
					t.Fatalf("decision=%+v err=%v", d, err)
				}
			}
		})
	}
}

func TestEarlyApprovalPreservesAuthorizationAndCommandRouting(t *testing.T) {
	bot, s := newScriptedBot()
	bot.api = func(method string, payload map[string]interface{}) (json.RawMessage, error) {
		raw, err := s.api(method, payload)
		if method == "sendMessage" {
			bot.pump().route(cbUpdate(1, 1, 999, "approve"))
			bot.pump().route(cbUpdate(2, 99, 7, "approve"))
			bot.pump().route(cbUpdate(3, 98, 7, "cron:pause:invoices"))
			commands := bot.drainUpdates()
			if len(commands) != 1 || commands[0].UpdateID != 3 {
				t.Fatalf("commands during send = %+v", commands)
			}
			bot.pump().route(cbUpdate(4, 1, 8, "approve"))
			bot.pump().route(cbUpdate(5, 1, 7, "skip"))
		}
		return raw, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := bot.SendForApproval(ctx, "draft", []int64{7})
	if err != nil || d.Action != "skip" || d.DeciderID != 7 {
		t.Fatalf("decision=%+v err=%v; only the step's operator may decide", d, err)
	}
	commands := bot.drainUpdates()
	if len(commands) != 1 || commands[0].UpdateID != 2 {
		t.Fatalf("unmatched commands after send = %+v", commands)
	}
	if bot.pump().waiterCount() != 0 {
		t.Fatal("completed approval retained its waiter")
	}
}

func TestEarlyApprovalKeepsFirstDecision(t *testing.T) {
	bot, s := newScriptedBot()
	bot.api = func(method string, payload map[string]interface{}) (json.RawMessage, error) {
		raw, err := s.api(method, payload)
		if method == "sendMessage" {
			bot.pump().route(cbUpdate(1, 1, 7, "skip"))
			bot.pump().route(cbUpdate(2, 1, 8, "approve"))
		}
		return raw, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := bot.SendForApproval(ctx, "draft", nil)
	if err != nil || d.Action != "skip" || d.DeciderID != 7 {
		t.Fatalf("first decision lost: %+v err=%v", d, err)
	}
}

func TestEarlyApprovalSendFailureReleasesUnmatchedCallbacks(t *testing.T) {
	bot, _ := newScriptedBot()
	bot.api = func(method string, _ map[string]interface{}) (json.RawMessage, error) {
		if method == "sendMessage" {
			bot.pump().route(cbUpdate(1, 42, 7, "approve"))
			return nil, fmt.Errorf("send failed")
		}
		return json.RawMessage(`true`), nil
	}
	_, err := bot.SendForApproval(context.Background(), "draft", nil)
	if err == nil || !strings.Contains(err.Error(), "send failed") {
		t.Fatalf("send error = %v", err)
	}
	commands := bot.drainUpdates()
	if len(commands) != 1 || commands[0].UpdateID != 1 {
		t.Fatalf("unclaimed callback = %+v", commands)
	}
	if bot.pump().waiterCount() != 0 {
		t.Fatal("failed send registered a waiter")
	}
	bot.pump().route(cbUpdate(2, 43, 7, "skip"))
	if commands = bot.drainUpdates(); len(commands) != 1 || commands[0].UpdateID != 2 {
		t.Fatalf("failed send left callback buffering active: %+v", commands)
	}
}

func TestEarlyApprovalConcurrentSendsResolveSeparately(t *testing.T) {
	bot, _ := newScriptedBot()
	started := make(chan string, 2)
	firstSend, secondSend := make(chan struct{}), make(chan struct{})
	bot.api = func(method string, payload map[string]interface{}) (json.RawMessage, error) {
		if method != "sendMessage" {
			return json.RawMessage(`true`), nil
		}
		name := payload["text"].(string)
		started <- name
		if name == "first" {
			<-firstSend
			return json.RawMessage(`{"message_id":1}`), nil
		}
		<-secondSend
		return json.RawMessage(`{"message_id":2}`), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type result struct {
		d   OperatorDecision
		err error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go func() { d, err := bot.SendForApproval(ctx, "first", nil); first <- result{d, err} }()
	go func() { d, err := bot.SendForApproval(ctx, "second", nil); second <- result{d, err} }()
	<-started
	<-started
	bot.pump().route(cbUpdate(1, 2, 8, "skip"))
	bot.pump().route(cbUpdate(2, 1, 7, "approve"))
	close(firstSend)
	r := <-first
	if r.err != nil || r.d.Action != "approve" || r.d.ApproverID != 7 {
		t.Fatalf("first approval = %+v", r)
	}
	close(secondSend)
	r = <-second
	if r.err != nil || r.d.Action != "skip" || r.d.DeciderID != 8 {
		t.Fatalf("second approval = %+v", r)
	}
	if commands := bot.drainUpdates(); len(commands) != 0 {
		t.Fatalf("claimed callbacks reached commands: %+v", commands)
	}
}

func TestEarlyApprovalOverflowKeepsExistingWaiterAndCommandsLive(t *testing.T) {
	bot, s := newScriptedBot()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first := make(chan OperatorDecision, 1)
	go func() { d, _ := bot.SendForApproval(ctx, "first", nil); first <- d }()
	waitFor(t, "first waiter", func() bool { return bot.pump().waiterCount() == 1 })
	bot.api = func(method string, payload map[string]interface{}) (json.RawMessage, error) {
		if method == "sendMessage" && payload["text"] == "second" {
			for i := 0; i < 1000; i++ {
				bot.pump().route(cbUpdate(i+1, 1000+i, 7, "approve"))
			}
			bot.pump().route(cbUpdate(1001, 1, 7, "approve"))
			bot.pump().route(cbUpdate(1002, 99, 7, "cron:pause:invoices"))
			commands := bot.drainUpdates()
			if len(commands) != 1 || commands[0].UpdateID != 1002 {
				t.Errorf("commands during overflow=%+v", commands)
			}
			return nil, fmt.Errorf("second send failed")
		}
		return s.api(method, payload)
	}
	if _, err := bot.SendForApproval(ctx, "second", nil); err == nil {
		t.Fatal("failed send succeeded")
	}
	if d := <-first; d.Action != "approve" || d.ApproverID != 7 {
		t.Fatalf("existing waiter stalled: %+v", d)
	}
	commands := bot.drainUpdates()
	if len(commands) != tgCommandBuffer {
		t.Fatalf("retained %d callbacks, want bounded %d", len(commands), tgCommandBuffer)
	}
	if len(bot.drainUpdates()) != 0 {
		t.Fatal("unmatched callbacks delivered twice")
	}
	bot.pump().route(cbUpdate(2000, 99, 7, "skip"))
	if commands = bot.drainUpdates(); len(commands) != 1 || commands[0].UpdateID != 2000 {
		t.Fatal("completed send still retains callbacks")
	}
}
