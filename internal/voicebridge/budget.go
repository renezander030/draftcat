package voicebridge

// CallBudget carries budgets.per_day_calls and budgets.per_day_call_minutes
// into the voice plugin. Caps is read on every call, so a config reload
// applies to the next call; Notify tells the operator, once per cap and UTC
// day, that new calls are being refused. Both may be nil.
type CallBudget struct {
	Caps   func() (perDayCalls, perDayCallMinutes int)
	Notify func(string)
}
