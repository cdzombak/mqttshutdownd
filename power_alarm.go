package main

//goland:noinspection GoUnusedConst
const (
	PowerTypeUtility   = 1
	PowerTypeGenerator = 2
	PowerTypeBattery   = 3
	PowerTypeSolar     = 4
	PowerTypeUnknown   = 5

	ScopeGlobal      = "global"
	ScopeLocal       = "local"
	ScopeSinglePhase = "1p"
	ScopeOneCircuit  = "1c"
)

// PowerAlarmMessage is the shape of messages on the power/alarms topic.
type PowerAlarmMessage struct {
	Online    bool   `json:"up"`
	PowerType int    `json:"type"`
	Scope     string `json:"scope"`
}

func (p *PowerAlarmMessage) Valid() bool {
	if p.PowerType < PowerTypeUtility || p.PowerType > PowerTypeUnknown {
		return false
	}
	if p.Scope != ScopeGlobal && p.Scope != ScopeLocal && p.Scope != ScopeSinglePhase && p.Scope != ScopeOneCircuit {
		return false
	}
	return true
}
