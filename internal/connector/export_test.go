package connector

// SetHealthLoop lets tests trigger health rounds and observe their end.
// Call before Start; returns a restore func.
func SetHealthLoop(kick chan struct{}, round func()) func() {
	healthKick, healthRound = kick, round
	return func() { healthKick, healthRound = nil, nil }
}

// SetHealthBefore runs f before each probe round (call before Start).
func SetHealthBefore(f func()) func() {
	healthBefore = f
	return func() { healthBefore = nil }
}

// SetAuditHook runs f before each audit write.
func SetAuditHook(f func()) func() {
	auditHook = f
	return func() { auditHook = nil }
}

// SetMutHook runs f inside UpdateAdapter between validation and the save.
func SetMutHook(f func()) func() {
	mutHook = f
	return func() { mutHook = nil }
}
