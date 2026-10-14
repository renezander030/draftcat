package state

import (
	"reflect"
	"testing"
	"time"
)

// Every signed v2 field is copied from the envelope field of the same name, so
// a field added to approval.FieldsV2 cannot be signed as its zero value.
func TestApprovalEnvelopeFieldsV2CoversEveryField(t *testing.T) {
	decided := time.Unix(1700000000, 0)
	expires := time.Unix(1700003600, 0)
	e := ApprovalEnvelope{
		ReceiptID: "r", RunID: "run", ActionID: "a", Pipeline: "p", Step: "s",
		DecidedAt: decided, Decision: "approve", OperatorID: 7, PayloadHash: "h",
		QuorumN: 2, QuorumGot: 3, Policy: "pol", PolicyHash: "ph", BindingHash: "bh",
		ExpiresAt: expires,
	}
	got := reflect.ValueOf(e.FieldsV2())
	src := reflect.ValueOf(e)
	for i := 0; i < got.NumField(); i++ {
		name := got.Type().Field(i).Name
		want := src.FieldByName(name)
		if !want.IsValid() {
			t.Fatalf("FieldsV2.%s has no ApprovalEnvelope field", name)
		}
		if tm, ok := want.Interface().(time.Time); ok {
			if got.Field(i).Int() != tm.Unix() {
				t.Fatalf("FieldsV2.%s = %d, want %d", name, got.Field(i).Int(), tm.Unix())
			}
			continue
		}
		if !reflect.DeepEqual(got.Field(i).Interface(), want.Interface()) {
			t.Fatalf("FieldsV2.%s = %v, want %v", name, got.Field(i).Interface(), want.Interface())
		}
	}
}
