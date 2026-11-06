package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestVoiceCallCountsSurviveRestartAndRefuseAtCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice.db")
	ctx := context.Background()
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	caps := VoiceCallCaps{Calls: 2}
	st, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c1", "c2"} {
		if counted, err := st.AdmitVoiceCall(ctx, id, at, caps); err != nil || !counted {
			t.Fatalf("admit %s: counted=%v err=%v", id, counted, err)
		}
	}
	// A repeated start of an admitted call is admitted without a second count.
	if counted, err := st.AdmitVoiceCall(ctx, "c1", at, caps); err != nil || counted {
		t.Fatalf("repeat admit: counted=%v err=%v", counted, err)
	}
	_ = st.Close()

	st, err = OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var capErr *VoiceCapError
	if _, err := st.AdmitVoiceCall(ctx, "c3", at, caps); !errors.As(err, &capErr) || capErr.Cap != "per_day_calls" || capErr.Used != 2 {
		t.Fatalf("third call must be refused after a restart: %v", err)
	}
	if err := st.CheckVoiceCall(ctx, at, caps); !errors.As(err, &capErr) {
		t.Fatalf("check must report the cap: %v", err)
	}
	day, err := st.BudgetDay(ctx, at)
	if err != nil || day.Calls != 2 {
		t.Fatalf("calls=%d err=%v", day.Calls, err)
	}
	// The next UTC day starts from zero.
	if counted, err := st.AdmitVoiceCall(ctx, "c3", at.Add(24*time.Hour), caps); err != nil || !counted {
		t.Fatalf("a refused call may start the next day: counted=%v err=%v", counted, err)
	}
	// No caps: always admitted, still counted.
	if counted, err := st.AdmitVoiceCall(ctx, "c4", at, VoiceCallCaps{}); err != nil || !counted {
		t.Fatalf("uncapped admit: counted=%v err=%v", counted, err)
	}
}

func TestVoiceCallMinutesRecordedOnceOnAdmissionDay(t *testing.T) {
	st, err := OpenStateStore(filepath.Join(t.TempDir(), "voice.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	start := time.Date(2026, 11, 6, 23, 50, 0, 0, time.UTC)
	end := start.Add(30 * time.Minute) // ends on the next UTC day
	caps := VoiceCallCaps{Minutes: 40}
	if _, err := st.AdmitVoiceCall(ctx, "c1", start, caps); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		counted, err := st.EndVoiceCall(ctx, "c1", end, 30)
		if err != nil || counted != (i == 0) {
			t.Fatalf("end %d: counted=%v err=%v", i, counted, err)
		}
	}
	d, _ := st.BudgetDay(ctx, start)
	next, _ := st.BudgetDay(ctx, end)
	if d.Calls != 1 || d.CallMinutes != 30 || next.CallMinutes != 0 {
		t.Fatalf("admission day %+v, next day %+v", d, next)
	}
	if _, err := st.AdmitVoiceCall(ctx, "c2", start, caps); err != nil {
		t.Fatalf("30 of 40 minutes leaves room: %v", err)
	}
	if _, err := st.EndVoiceCall(ctx, "c2", end, 15); err != nil {
		t.Fatal(err)
	}
	var capErr *VoiceCapError
	if _, err := st.AdmitVoiceCall(ctx, "c3", start, caps); !errors.As(err, &capErr) || capErr.Cap != "per_day_call_minutes" || capErr.Used != 45 {
		t.Fatalf("minute cap must refuse: %v", err)
	}
}

func TestVoiceCallEndWithoutAdmissionAndRefusedEnd(t *testing.T) {
	st, err := OpenStateStore(filepath.Join(t.TempDir(), "voice.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	// An end whose start never arrived still counts the call.
	if counted, err := st.EndVoiceCall(ctx, "late", at, 4); err != nil || !counted {
		t.Fatalf("unadmitted end: counted=%v err=%v", counted, err)
	}
	// Its start arriving afterwards does not count it again.
	if counted, err := st.AdmitVoiceCall(ctx, "late", at, VoiceCallCaps{Calls: 1}); err != nil || counted {
		t.Fatalf("start after end: counted=%v err=%v", counted, err)
	}
	if _, err := st.AdmitVoiceCall(ctx, "refused", at, VoiceCallCaps{Calls: 1}); err == nil {
		t.Fatal("expected a refusal")
	}
	if counted, err := st.EndVoiceCall(ctx, "refused", at, 9); err != nil || counted {
		t.Fatalf("a refused call's end records nothing: counted=%v err=%v", counted, err)
	}
	d, _ := st.BudgetDay(ctx, at)
	if d.Calls != 1 || d.CallMinutes != 4 {
		t.Fatalf("day = %+v", d)
	}
	if _, err := st.EndVoiceCall(ctx, "x", at, -1); err == nil {
		t.Fatal("negative minutes must be refused")
	}
}
